package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
	"github.com/snehendu098/sweem-terminal/internal/config"
	"github.com/snehendu098/sweem-terminal/internal/httpx"
	"github.com/snehendu098/sweem-terminal/internal/streams"
)

// MarketData is the Quoter over market-data HTTP.
type MarketData struct {
	URL  string
	HTTP *http.Client
}

func (m MarketData) get(ctx context.Context, path string, out any) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL+path, nil)
	if err != nil {
		return 0, "", err
	}
	res, err := m.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.NewDecoder(res.Body).Decode(&e)
		return res.StatusCode, e.Error, nil
	}
	return 200, "", json.NewDecoder(res.Body).Decode(out)
}

func (m MarketData) Quote(ctx context.Context, orderbook, side string, amount decimal.Decimal, slippageBps int) (Quote, error) {
	var q Quote
	status, code, err := m.get(ctx, fmt.Sprintf("/api/v1/quote/%s?%s", orderbook, url.Values{
		"side": {side}, "amount": {amount.String()}, "slippageBps": {fmt.Sprint(slippageBps)}}.Encode()), &q)
	switch {
	case err != nil:
		return q, fail("quote_unavailable", "%v", err)
	case status == http.StatusBadRequest && code == "below_min_size":
		return q, fail("below_min_size", "%s %s %s below Kuru min size", side, orderbook, amount)
	case status != 200:
		return q, fail("quote_unavailable", "market-data %d %s", status, code)
	}
	return q, nil
}

func (m MarketData) Rates(ctx context.Context) (streams.RatesSnapshot, error) {
	var r struct {
		streams.RatesSnapshot
		Stale bool `json:"stale"`
	}
	status, code, err := m.get(ctx, "/api/v1/rates", &r)
	if err == nil && status != 200 {
		err = fmt.Errorf("rates %d %s", status, code)
	}
	return r.RatesSnapshot, err
}

// Executor runs a planned execution (POST /api/v1/user/steps).
type Executor struct {
	URL, Secret, Caller string
	HTTP                *http.Client
}

// Outcome is user_steps.response.json.
type Outcome struct {
	ExecutionID string   `json:"executionId"`
	Status      string   `json:"status"`
	StepsDone   int      `json:"stepsDone"`
	TxHashes    []string `json:"txHashes"`
	Error       *string  `json:"error"`
}

// Run blocks until the executor finished every step (or rejected the plan: *Error with its code).
func (x Executor) Run(ctx context.Context, executionID string) (Outcome, error) {
	var out Outcome
	body, _ := json.Marshal(map[string]string{"executionId": executionID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.URL+"/api/v1/user/steps", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	httpx.SetInternal(req, x.Secret, x.Caller)
	res, err := x.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.NewDecoder(res.Body).Decode(&e)
		return out, &Error{Code: e.Error, Detail: fmt.Sprintf("executor %d", res.StatusCode)}
	}
	return out, json.NewDecoder(res.Body).Decode(&out)
}

// Chain is the RPC surface balances need (*ethclient.Client satisfies it).
type Chain interface {
	ethereum.ContractCaller
	BalanceAt(ctx context.Context, account common.Address, block *big.Int) (*big.Int, error)
}

var (
	selBalanceOf       = common.FromHex("0x70a08231")
	selConvertToAssets = common.FromHex("0x07a2d13a")
)

func call(ctx context.Context, c Chain, to string, sel []byte, arg []byte) (decimal.Decimal, error) {
	a := common.HexToAddress(to)
	out, err := c.CallContract(ctx, ethereum.CallMsg{To: &a, Data: append(append([]byte{}, sel...), common.LeftPadBytes(arg, 32)...)}, nil)
	if err != nil {
		return decimal.Zero, err
	}
	if len(out) < 32 {
		return decimal.Zero, fmt.Errorf("%s: short return", to)
	}
	return decimal.NewFromBigInt(new(big.Int).SetBytes(out[:32]), 0), nil
}

// WalletBalance returns sym atoms in wallet; MON is the native balance.
func WalletBalance(ctx context.Context, c Chain, dep *config.Deployments143, wallet, sym string) (decimal.Decimal, error) {
	t, ok := dep.Tokens[sym]
	if !ok {
		return decimal.Zero, fmt.Errorf("unknown token %s", sym)
	}
	if sym == "MON" {
		b, err := c.BalanceAt(ctx, common.HexToAddress(wallet), nil)
		if err != nil {
			return decimal.Zero, err
		}
		return decimal.NewFromBigInt(b, 0), nil
	}
	return call(ctx, c, t.Address, selBalanceOf, common.HexToAddress(wallet).Bytes())
}

// Peer is another open position of the same wallet; venues it shares with this one are split pro rata by LegState.Asset.
// Asset is the strategy asset (Multi for sleeves: legs carry their Token).
type Peer struct {
	Asset string
	Legs  []LegState
}

// Balances reads each source's live Value/Shares for one position of wallet. lp and basis come from legs (no venue read):
// lp Value = MON deposited, basis Value = spot + collateral/entryPx.
func Balances(ctx context.Context, c Chain, dep *config.Deployments143, wallet string, s Strategy, legs []LegState, peers []Peer) ([]Balance, error) {
	parts := s.Parts()
	ls, err := Split(parts, legs)
	if err != nil {
		return nil, fmt.Errorf("legs do not match sources")
	}
	var out []Balance
	for k, pt := range parts {
		b, err := balancesPart(ctx, c, dep, wallet, s.Part(pt), ls[k], peers)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

func balancesPart(ctx context.Context, c Chain, dep *config.Deployments143, wallet string, s Strategy, legs []LegState, peers []Peer) ([]Balance, error) {
	w := common.HexToAddress(wallet).Bytes()
	out := make([]Balance, len(s.Sources))
	for i, src := range s.Sources {
		var shares, value decimal.Decimal
		var err error
		la := lendAsset(s.Asset)
		switch src.Kind {
		case "lp":
			if dep.Kuru.ActiveVault != "" {
				shares, err = call(ctx, c, dep.Kuru.ActiveVault, selBalanceOf, w)
			}
			value = d(legs[i].Asset)
		case "basis":
			value = d(legs[i].Asset)
			if px := d(legs[i].EntryPx); px.IsPositive() {
				value = value.Add(d(legs[i].Collateral).Shift(-6).Div(px).Shift(int32(dep.Tokens[s.Asset].Decimals)).Floor())
			}
		case "hold":
			// ponytail: the recorded amount, not the wallet balance (tokens are fungible with the user's own); a manual sell
			// makes the exit's market sell fail, clamp to balanceOf here if that bites
			value = d(legs[i].Asset)
		case "staking":
			if shares, err = call(ctx, c, dep.Staking.Shmonad, selBalanceOf, w); err == nil {
				value, err = call(ctx, c, dep.Staking.Shmonad, selConvertToAssets, shares.BigInt().Bytes())
			}
		default:
			switch src.Venue {
			case "aave":
				value, err = call(ctx, c, dep.Lending.AaveV3.ATokens[la], selBalanceOf, w)
			case "neverland":
				value, err = call(ctx, c, dep.Lending.Neverland.ATokens[la], selBalanceOf, w)
			default:
				for _, v := range dep.Lending.ERC4626 {
					if Slug(v.Name) == src.Venue {
						if shares, err = call(ctx, c, v.Vault, selBalanceOf, w); err == nil {
							value, err = call(ctx, c, v.Vault, selConvertToAssets, shares.BigInt().Bytes())
						}
					}
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%s:%s: %w", src.Kind, src.Venue, err)
		}
		if src.Kind != "lp" && src.Kind != "basis" && src.Kind != "hold" {
			// wallet-wide venue balance: this position's share by principal
			mine, all := d(legs[i].Asset), d(legs[i].Asset)
			for _, p := range peers {
				for _, l := range p.Legs {
					if lendAsset(l.TokenOr(p.Asset)) == la && l.Kind == src.Kind && l.Venue == src.Venue {
						all = all.Add(d(l.Asset))
					}
				}
			}
			if all.IsPositive() && all.GreaterThan(mine) {
				value, shares = value.Mul(mine).Div(all).Floor(), shares.Mul(mine).Div(all).Floor()
			}
		}
		out[i] = Balance{Value: value.String(), Shares: shares.String()}
	}
	return out, nil
}
