package planner

import (
	"context"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/snehendu098/sweem-terminal/internal/config"
)

type SourceParams struct {
	Leverage string `json:"leverage,omitempty"` // basis only, e.g. "2"
}

type Source struct {
	Kind      string       `json:"kind"`  // lending | staking | lp | basis
	Venue     string       `json:"venue"` // aave | neverland | <4626 slug> | shmonad | kuru-activevault | perpl
	WeightBps int          `json:"weightBps"`
	Params    SourceParams `json:"params"`
}

type Gate struct {
	APR          string `json:"apr"`
	LookbackDays int    `json:"lookbackDays"`
}

type Rules struct {
	DriftBps            int         `json:"driftBps,omitempty"`
	MinRebalanceSeconds int64       `json:"minRebalanceSeconds,omitempty"`
	FundingGate         *Gate       `json:"fundingGate,omitempty"`
	RateSwitch          *RateSwitch `json:"rateSwitch,omitempty"`
}

// RateSwitch: the scheduler moves lending weight to a venue whose APY beats the current one by > MinGain (fraction).
type RateSwitch struct {
	MinGain string `json:"minGain"`
}

// Strategy is one asset + sources, or (Sleeves set) several token parts sharing Rules. The token split applies at deposit
// only; each part then lives on as its own single-asset strategy (Part).
type Strategy struct {
	Asset   string   `json:"asset,omitempty"`
	Sources []Source `json:"sources,omitempty"`
	Sleeves []Sleeve `json:"sleeves,omitempty"`
	Rules   Rules    `json:"rules"`
}

// Sleeve is one token part of a multi-token strategy.
type Sleeve struct {
	Asset     string   `json:"asset"`
	WeightBps int      `json:"weightBps"`
	Sources   []Source `json:"sources"`
}

// Multi is the asset stored for multi-token strategies (sleeves column holds the parts).
const Multi = "MULTI"

// Parts is the strategy's sleeves; a single-asset strategy is one sleeve at 10000.
func (s Strategy) Parts() []Sleeve {
	if len(s.Sleeves) == 0 {
		return []Sleeve{{s.Asset, 10_000, s.Sources}}
	}
	return s.Sleeves
}

// Part is sleeve sl as a single-asset strategy with the shared rules.
func (s Strategy) Part(sl Sleeve) Strategy {
	return Strategy{Asset: sl.Asset, Sources: sl.Sources, Rules: s.Rules}
}

// Split cuts a flat per-source list (legs, balances, targets) into one slice per part.
func Split[T any](parts []Sleeve, xs []T) ([][]T, error) {
	n := 0
	for _, pt := range parts {
		n += len(pt.Sources)
	}
	if n != len(xs) {
		return nil, fail("bad_request", "%d entries do not match %d sources", len(xs), n)
	}
	out, i := make([][]T, len(parts)), 0
	for k, pt := range parts {
		j := i + len(pt.Sources)
		out[k], i = xs[i:j:j], j
	}
	return out, nil
}

// LegState is what a source holds for one position (stored in strategy_positions.legs). Raw atoms; EntryPx display USDC/unit.
type LegState struct {
	Token      string `json:"token,omitempty"` // sleeve asset; "" = the strategy's asset (single-token rows)
	Kind       string `json:"kind"`
	Venue      string `json:"venue"`
	Asset      string `json:"asset"`                // asset atoms supplied (basis: spot kept)
	Collateral string `json:"collateral,omitempty"` // basis: AUSD atoms in Perpl
	Lot        string `json:"lot,omitempty"`        // basis: short size in perp lot units
	EntryPx    string `json:"entryPx,omitempty"`    // basis
}

// Balance is a source's live state for one position, same order as Strategy.Sources.
// Value: asset atoms the source returns on exit (lp: MON part; basis: spot + collateral in asset terms). Shares: vault/shMON/ActiveVault share atoms.
type Balance struct {
	Value  string `json:"value"`
	Shares string `json:"shares"`
}

type Plan struct {
	Steps   []Step     `json:"steps"`
	FeeUSDC string     `json:"feeUsdc,omitempty"` // atoms
	Legs    []LegState `json:"legs,omitempty"`
}

var Assets = map[string]bool{"USDC": true, "WETH": true, "cbBTC": true, "MON": true, "XAUt0": true}

// perp: Perpl market hedging a Kuru spot asset; decimals from app.perpl.xyz/api/v1/pub/context (verified 2026-09-30).
type perp struct {
	Name           string
	PriceDec, Size int32
	MaxLev         int64
}

var perps = map[string]perp{"WETH": {"ETH", 2, 3, 12}, "cbBTC": {"BTC", 1, 5, 15}, "MON": {"MON", 6, 0, 10}}

// HedgeAssets are the basis assets, in display order.
var HedgeAssets = []string{"WETH", "cbBTC", "MON"}

// PerpName is the Perpl market hedging asset ("" = none).
func PerpName(asset string) string { return perps[asset].Name }

// PerpID is the Perpl perp id hedging asset (0 = none).
func (p *Planner) PerpID(asset string) int { return p.Dep.Perpl.PerpIds[PerpName(asset)] }

const (
	perplSlipBps   = 100 // IOC limit through spot; Perpl caps market slippage at 100 bps
	poolFeeBps     = 5   // 3pool
	shmonHaircut   = 100 // instant redeem ~0.93%
	perplMinOpen   = 10_000_000
	perplExitBufBp = 50 // withdraw estimate buffer on notional
)

// Slug is the venue id of a 4626 vault: "Euler k3USDC" → "euler-k3usdc" (same as publisher rates).
func Slug(name string) string { return strings.ToLower(strings.Join(strings.Fields(name), "-")) }

func (p *Planner) vault(slug string) (config.Vault, bool) {
	for _, v := range p.Dep.Lending.ERC4626 {
		if Slug(v.Name) == slug {
			return v, true
		}
	}
	return config.Vault{}, false
}

func d(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }

func (s Source) leverage() decimal.Decimal { return d(s.Params.Leverage) }

// TokenOr is the leg's token, def for legs written before sleeves.
func (l LegState) TokenOr(def string) string {
	if l.Token == "" {
		return def
	}
	return l.Token
}

// Validate checks a strategy against what the executor can run.
func (p *Planner) Validate(s Strategy) error {
	if len(s.Sleeves) == 0 {
		return p.validPart(s)
	}
	if (s.Asset != "" && s.Asset != Multi) || len(s.Sources) > 0 || len(s.Sleeves) < 2 || len(s.Sleeves) > 5 {
		return fail("invalid_sleeves", "need 2..5 sleeves and no top-level asset/sources")
	}
	sum, n, seen := 0, 0, map[string]bool{}
	for _, sl := range s.Sleeves {
		if sl.WeightBps <= 0 || seen[sl.Asset] {
			return fail("invalid_sleeves", "bad weight or duplicate %s", sl.Asset)
		}
		seen[sl.Asset], sum, n = true, sum+sl.WeightBps, n+len(sl.Sources)
		if err := p.validPart(s.Part(sl)); err != nil {
			return err
		}
	}
	if sum != 10_000 {
		return fail("invalid_sleeves", "weights sum %d, want 10000", sum)
	}
	if n > 10 { // executor MaxSteps 32
		return fail("invalid_sleeves", "at most 10 sources in total")
	}
	return nil
}

func (p *Planner) validPart(s Strategy) error {
	if !Assets[s.Asset] {
		return fail("invalid_asset", "asset %q", s.Asset)
	}
	if len(s.Sources) == 0 || len(s.Sources) > 6 {
		return fail("invalid_sources", "need 1..6 sources")
	}
	sum, seen, basis := 0, map[string]bool{}, 0
	for _, src := range s.Sources {
		key := src.Kind + ":" + src.Venue
		if src.WeightBps <= 0 || seen[key] {
			return fail("invalid_sources", "bad weight or duplicate %s", key)
		}
		seen[key], sum = true, sum+src.WeightBps
		if err := p.validSource(s.Asset, src); err != nil {
			return err
		}
		if src.Kind == "basis" {
			basis++
		}
	}
	if sum != 10_000 {
		return fail("invalid_sources", "weights sum %d, want 10000", sum)
	}
	if basis > 1 {
		return fail("invalid_sources", "at most one basis source")
	}
	if r := s.Rules; r.DriftBps < 0 || r.MinRebalanceSeconds < 0 || (r.FundingGate != nil && (r.FundingGate.LookbackDays < 1 || d(r.FundingGate.APR).IsNegative())) {
		return fail("invalid_rules", "negative or bad funding gate")
	}
	if r := s.Rules.RateSwitch; r != nil && !d(r.MinGain).IsPositive() {
		return fail("invalid_rules", "rateSwitch.minGain must be > 0")
	}
	return nil
}

func (p *Planner) validSource(asset string, src Source) error {
	bad := func(why string) error { return fail("unsupported_source", "%s:%s %s", src.Kind, src.Venue, why) }
	switch src.Kind {
	case "lending":
		switch src.Venue {
		case "aave", "neverland":
			pool := p.Dep.Lending.AaveV3
			if src.Venue == "neverland" {
				pool = p.Dep.Lending.Neverland
			}
			if pool.ATokens[lendAsset(asset)] == "" {
				return bad("no reserve for " + asset)
			}
		default:
			v, ok := p.vault(src.Venue)
			if !ok || v.Asset != lendAsset(asset) {
				return bad("unknown vault for " + asset)
			}
		}
	case "hold":
		if src.Venue != "wallet" {
			return bad("hold is venue wallet")
		}
	case "staking":
		if src.Venue != "shmonad" || asset != "MON" {
			return bad("staking is shmonad on MON only")
		}
	case "lp":
		if src.Venue != "kuru-activevault" || asset != "MON" {
			return bad("lp is kuru-activevault on MON only")
		}
	case "basis":
		pp, ok := perps[asset]
		l := src.leverage()
		if src.Venue != "perpl" || !ok {
			return bad("basis is perpl on WETH/cbBTC/MON")
		}
		if l.LessThan(decimal.NewFromInt(1)) || l.GreaterThan(decimal.NewFromInt(pp.MaxLev)) || !l.Mul(decimal.NewFromInt(100)).IsInteger() {
			return fail("invalid_sources", "basis leverage must be 1..%d with ≤2 decimals", pp.MaxLev)
		}
	default:
		return bad("unknown kind")
	}
	return nil
}

// lendAsset: MON is lent as WMON.
func lendAsset(asset string) string {
	if asset == "MON" {
		return "WMON"
	}
	return asset
}

// supply puts amt atoms of the strategy asset into a lending/staking source (MON wrapped first for lending). hold: stays in the wallet.
func (p *Planner) supply(asset string, src Source, amt decimal.Decimal) []Step {
	a, la := amt.String(), lendAsset(asset)
	var st Step
	switch {
	case src.Kind == "hold":
		return nil
	case src.Kind == "staking":
		return []Step{{Action: "shmonad.deposit", Amount: a}}
	case src.Venue == "aave":
		st = Step{Action: "lend.supply", Venue: "aaveV3", Asset: la, Amount: a}
	case src.Venue == "neverland":
		st = Step{Action: "lend.supply", Venue: "neverland", Asset: la, Amount: a}
	default:
		v, _ := p.vault(src.Venue)
		st = Step{Action: "vault.deposit", Vault: v.Vault, Amount: a}
	}
	if la != asset {
		return []Step{{Action: "wmon.wrap", Amount: a}, st}
	}
	return []Step{st}
}

// takeOut withdraws amt asset atoms from a lending/staking source (all = everything, by shares); returns the guaranteed asset out.
// hold: already in the wallet, nothing to withdraw.
func (p *Planner) takeOut(asset string, src Source, amt decimal.Decimal, b Balance, all bool) ([]Step, decimal.Decimal) {
	if src.Kind == "hold" {
		return nil, amt
	}
	shares := d(b.Shares)
	if !all && d(b.Value).IsPositive() {
		shares = shares.Mul(amt).Div(d(b.Value)).Floor()
	}
	a, la := amt.String(), lendAsset(asset)
	var st Step
	switch {
	case src.Kind == "staking":
		return []Step{{Action: "shmonad.redeem", Shares: shares.String()}}, amt.Mul(decimal.NewFromInt(10_000 - shmonHaircut)).Div(bps).Floor()
	case src.Venue == "aave" || src.Venue == "neverland":
		venue := map[string]string{"aave": "aaveV3", "neverland": "neverland"}[src.Venue]
		st = Step{Action: "lend.withdraw", Venue: venue, Asset: la, Amount: a}
	default:
		v, _ := p.vault(src.Venue)
		st = Step{Action: "vault.withdraw", Vault: v.Vault, Amount: a}
		if all {
			st = Step{Action: "vault.redeem", Vault: v.Vault, Shares: shares.String()}
		}
	}
	if la != asset {
		return []Step{st, {Action: "wmon.unwrap", Amount: a}}, amt
	}
	return []Step{st}, amt
}

func bpsOf(x decimal.Decimal, w int) decimal.Decimal {
	return x.Mul(decimal.NewFromInt(int64(w))).Div(bps).Floor()
}

// openBasis: spot atoms already in the wallet, usdc atoms → AUSD collateral → short `spot` at px.
func (p *Planner) openBasis(asset string, src Source, spot, usdc decimal.Decimal, px decimal.Decimal, hasAccount bool, slip int) ([]Step, LegState, error) {
	pp := perps[asset]
	ausd := usdc.Mul(decimal.NewFromInt(int64(10_000 - slip - poolFeeBps))).Div(bps).Floor()
	lot := spot.Shift(pp.Size - int32(p.decimals(asset))).Floor()
	if !lot.IsPositive() || !ausd.IsPositive() {
		return nil, LegState{}, fail("below_min_size", "basis leg too small to hedge")
	}
	steps := []Step{{Action: "stable.swap", From: "USDC", To: "AUSD", Amount: usdc.String(), MinOut: ausd.String()}}
	if hasAccount {
		steps = append(steps, Step{Action: "perpl.deposit", Amount: ausd.String()})
	} else {
		if ausd.LessThan(decimal.NewFromInt(perplMinOpen)) {
			return nil, LegState{}, fail("below_min_size", "Perpl account needs ≥10 AUSD collateral")
		}
		steps = append(steps, Step{Action: "perpl.createAccount", Amount: ausd.String()})
	}
	limit := px.Mul(decimal.NewFromInt(10_000 - perplSlipBps)).Div(bps).Shift(pp.PriceDec).Floor()
	steps = append(steps, Step{Action: "perpl.order", Perp: pp.Name, Kind: "openShort", Price: limit.String(), Lot: lot.String(),
		LeverageHdths: int(src.leverage().Mul(decimal.NewFromInt(100)).IntPart())})
	return steps, LegState{Token: asset, Kind: src.Kind, Venue: src.Venue, Asset: spot.String(), Collateral: ausd.String(), Lot: lot.String(), EntryPx: px.String()}, nil
}

// closeBasis closes part (bps) of the short and withdraws that share of the estimated collateral back to USDC.
// Returns the steps, guaranteed USDC out and the leg left afterwards.
func (p *Planner) closeBasis(asset string, leg LegState, px decimal.Decimal, part, slip int) ([]Step, decimal.Decimal, LegState) {
	pp := perps[asset]
	lot := bpsOf(d(leg.Lot), part)
	if !lot.IsPositive() {
		return nil, decimal.Zero, leg
	}
	units := lot.Shift(-pp.Size)
	// (collateral + short PnL) × part − taker fee − buffer on the closed notional, in AUSD atoms
	pnl := d(leg.EntryPx).Sub(px).Mul(d(leg.Lot).Shift(-pp.Size)).Shift(6)
	fees := units.Mul(px).Shift(6).Mul(decimal.NewFromFloat(3.45 + perplExitBufBp)).Div(bps)
	est := bpsOf(d(leg.Collateral).Add(pnl), part).Sub(fees).Floor()
	limit := px.Mul(decimal.NewFromInt(10_000 + perplSlipBps)).Div(bps).Shift(pp.PriceDec).Ceil()
	steps := []Step{{Action: "perpl.order", Perp: pp.Name, Kind: "closeShort", Price: limit.String(), Lot: lot.String(), LeverageHdths: 100}}
	left := leg
	left.Lot = d(leg.Lot).Sub(lot).String()
	left.Collateral = d(leg.Collateral).Sub(bpsOf(d(leg.Collateral), part)).String()
	left.Asset = d(leg.Asset).Sub(bpsOf(d(leg.Asset), part)).String()
	if part == 10_000 {
		left = LegState{Token: leg.Token, Kind: leg.Kind, Venue: leg.Venue, Asset: "0"}
	}
	if !est.IsPositive() {
		return steps, decimal.Zero, left
	}
	out := est.Mul(decimal.NewFromInt(int64(10_000 - slip - poolFeeBps))).Div(bps).Floor()
	return append(steps,
		Step{Action: "perpl.withdraw", Amount: est.String()},
		Step{Action: "stable.swap", From: "AUSD", To: "USDC", Amount: est.String(), MinOut: out.String()}), out, left
}

// Deploy opens a position with usdc atoms: fee to treasury first, then per sleeve (split by weight) and source
// (buy the sleeve asset on Kuru once, supply/stake/LP/hedge).
func (p *Planner) Deploy(ctx context.Context, s Strategy, usdc decimal.Decimal, hasPerplAccount bool, slip int) (Plan, error) {
	if err := p.Validate(s); err != nil {
		return Plan{}, err
	}
	if p.Dep.Treasury == "" {
		return Plan{}, fail("treasury_unset", "treasury missing in deployments")
	}
	fee := bpsOf(usdc, FeeBps)
	if !fee.IsPositive() {
		return Plan{}, fail("below_min_size", "amount too small")
	}
	if err := p.minDeposit(ctx, s, usdc, hasPerplAccount, slip); err != nil {
		return Plan{}, err
	}
	net := usdc.Sub(fee)
	steps := []Step{{Action: "fee.transfer", Amount: fee.String()}}
	var legs []LegState
	parts, left := s.Parts(), net
	for k, pt := range parts {
		amt := bpsOf(net, pt.WeightBps)
		if k == len(parts)-1 {
			amt = left
		}
		left = left.Sub(amt)
		ps, ls, err := p.deployPart(ctx, s.Part(pt), amt, hasPerplAccount, slip)
		if err != nil {
			return Plan{}, err
		}
		steps, legs = append(steps, ps...), append(legs, ls...)
		for _, src := range pt.Sources {
			hasPerplAccount = hasPerplAccount || src.Kind == "basis" // the first basis sleeve creates the account
		}
	}
	steps, err := p.check(p.Dep, steps)
	return Plan{Steps: steps, FeeUSDC: fee.String(), Legs: legs}, err
}

// minDeposit rejects usdc too small for a sleeve's Kuru buy (min order + 5% for price moves) or, without an account,
// for the first basis leg's Perpl account. The error names the total deposit that clears it.
func (p *Planner) minDeposit(ctx context.Context, s Strategy, usdc decimal.Decimal, hasAccount bool, slip int) error {
	one := decimal.NewFromInt(1)
	need, asset, why := decimal.Zero, "", ""
	for _, pt := range s.Parts() {
		w := decimal.NewFromInt(int64(pt.WeightBps)).Div(bps).Mul(decimal.NewFromInt(10_000 - FeeBps)).Div(bps)
		buy, coll := decimal.Zero, decimal.Zero
		for _, src := range pt.Sources {
			ws, f := decimal.NewFromInt(int64(src.WeightBps)).Div(bps), one
			switch src.Kind {
			case "lp":
				f = decimal.NewFromFloat(0.5)
			case "basis":
				l := src.leverage()
				f = l.Div(l.Add(one))
				if !hasAccount {
					coll, hasAccount = ws.Div(l.Add(one)), true
				}
			}
			buy = buy.Add(ws.Mul(f))
		}
		if pt.Asset != "USDC" && buy.IsPositive() {
			m, err := p.market(pt.Asset)
			if err != nil {
				return err
			}
			q, err := p.quote(ctx, pt.Asset, "buy", decimal.New(100, 6), slip) // $100 probe for the spot price
			if err != nil {
				return err
			}
			base := decimal.NewFromInt(int64(m.MinSize)).Div(decimal.NewFromInt(int64(m.SizePrecision)))
			if v := base.Mul(d(q.AvgPx)).Mul(decimal.NewFromFloat(1.05)).Div(buy.Mul(w)); v.GreaterThan(need) {
				need, asset, why = v, pt.Asset, fmt.Sprintf("Kuru min %s %s", base, pt.Asset)
			}
		}
		if coll.IsPositive() {
			ausd := decimal.NewFromInt(int64(10_000 - slip - poolFeeBps)).Div(bps)
			if v := decimal.New(perplMinOpen, -6).Div(coll.Mul(w).Mul(ausd)); v.GreaterThan(need) {
				need, asset, why = v, pt.Asset, fmt.Sprintf("Perpl min %s AUSD collateral", decimal.New(perplMinOpen, -6))
			}
		}
	}
	if usdc.Shift(-6).LessThan(need) {
		return fail("below_min_size", "%s part needs ≥ %s USDC total deposit (%s)", asset, need.RoundCeil(2).StringFixed(2), why)
	}
	return nil
}

// deployPart spends usdc atoms on one sleeve: one Kuru buy for all its sources, then supply/stake/LP/hedge.
func (p *Planner) deployPart(ctx context.Context, s Strategy, net decimal.Decimal, hasPerplAccount bool, slip int) ([]Step, []LegState, error) {
	var steps []Step
	allocs, buys := make([]decimal.Decimal, len(s.Sources)), make([]decimal.Decimal, len(s.Sources))
	total := decimal.Zero
	for i, src := range s.Sources {
		allocs[i] = bpsOf(net, src.WeightBps)
		if s.Asset == "USDC" {
			continue
		}
		switch src.Kind {
		case "lp":
			buys[i] = allocs[i].Div(decimal.NewFromInt(2)).Floor()
		case "basis":
			l := src.leverage()
			buys[i] = allocs[i].Mul(l).Div(l.Add(decimal.NewFromInt(1))).Floor()
		default:
			buys[i] = allocs[i]
		}
		total = total.Add(buys[i])
	}
	got, px := decimal.Zero, decimal.Zero
	if total.IsPositive() {
		q, err := p.quote(ctx, s.Asset, "buy", total, slip)
		if err != nil {
			return nil, nil, err
		}
		got, px = d(q.MinOut), d(q.AvgPx)
		steps = append(steps, Step{Action: "kuru.marketBuy", Market: s.Asset, Quote: total.String(), MinOut: q.MinOut})
	}
	legs := make([]LegState, len(s.Sources))
	for i, src := range s.Sources {
		amt := allocs[i]
		if s.Asset != "USDC" {
			amt = got.Mul(buys[i]).Div(total).Floor()
		}
		legs[i] = LegState{Token: s.Asset, Kind: src.Kind, Venue: src.Venue, Asset: amt.String()}
		switch src.Kind {
		case "lp":
			steps = append(steps, Step{Action: "activeVault.deposit", Base: amt.String(), Quote: allocs[i].Sub(buys[i]).String()})
		case "basis":
			bs, leg, err := p.openBasis(s.Asset, src, amt, allocs[i].Sub(buys[i]), px, hasPerplAccount, slip)
			if err != nil {
				return nil, nil, err
			}
			steps, legs[i] = append(steps, bs...), leg
		default:
			steps = append(steps, p.supply(s.Asset, src, amt)...)
		}
	}
	return steps, legs, nil
}

// Withdraw unwinds every source and sells the asset to USDC. No fee.
func (p *Planner) Withdraw(ctx context.Context, s Strategy, legs []LegState, bals []Balance, slip int) (Plan, error) {
	return p.WithdrawBps(ctx, s, legs, bals, 10_000, slip)
}

// WithdrawBps unwinds part (bps) of every source pro rata and sells each sleeve asset to USDC (position TP/SL). Plan.Legs = what remains.
func (p *Planner) WithdrawBps(ctx context.Context, s Strategy, legs []LegState, bals []Balance, part, slip int) (Plan, error) {
	parts := s.Parts()
	ls, err := Split(parts, legs)
	bs, err2 := Split(parts, bals)
	if err != nil || err2 != nil || part < 1 || part > 10_000 {
		return Plan{}, fail("bad_request", "legs/balances do not match sources or bps out of 1..10000")
	}
	var steps []Step
	var out []LegState
	for k, pt := range parts {
		st, l, err := p.withdrawPart(ctx, s.Part(pt), ls[k], bs[k], part, slip)
		if err != nil {
			return Plan{}, err
		}
		steps, out = append(steps, st...), append(out, l...)
	}
	steps, err = p.check(p.Dep, steps)
	return Plan{Steps: steps, Legs: out}, err
}

func (p *Planner) withdrawPart(ctx context.Context, s Strategy, legs []LegState, bals []Balance, part, slip int) ([]Step, []LegState, error) {
	all := part == 10_000
	var steps []Step
	sell := decimal.Zero
	basis := -1
	out := make([]LegState, len(legs))
	for i, src := range s.Sources {
		b := bals[i]
		out[i] = legs[i]
		out[i].Asset = d(legs[i].Asset).Sub(bpsOf(d(legs[i].Asset), part)).String()
		switch src.Kind {
		case "lp":
			if sh := bpsOf(d(b.Shares), part); sh.IsPositive() {
				steps = append(steps, Step{Action: "activeVault.withdraw", Shares: sh.String()})
				sell = sell.Add(bpsOf(d(b.Value), part))
			}
		case "basis":
			basis = i
			sell = sell.Add(bpsOf(d(legs[i].Asset), part))
		default:
			if v := bpsOf(d(b.Value), part); v.IsPositive() {
				st, got := p.takeOut(s.Asset, src, v, b, all)
				steps, sell = append(steps, st...), sell.Add(got)
			}
		}
	}
	var q Quote
	var err error
	if s.Asset != "USDC" && sell.IsPositive() {
		q, err = p.quote(ctx, s.Asset, "sell", sell, slip)
		if err != nil && !isCode(err, "below_min_size") {
			return nil, nil, err
		}
	}
	if basis >= 0 && d(legs[basis].Lot).IsPositive() {
		px := d(q.AvgPx)
		if !px.IsPositive() {
			return nil, nil, fail("quote_unavailable", "no %s price to close the hedge", s.Asset)
		}
		var bs []Step
		bs, _, out[basis] = p.closeBasis(s.Asset, legs[basis], px, part, slip)
		steps = append(steps, bs...)
	}
	if err == nil && q.MinOut != "" {
		steps = append(steps, Step{Action: "kuru.marketSell", Market: s.Asset, Size: sell.String(), MinOut: q.MinOut})
	}
	return steps, out, nil
}

// ResizeBasis changes the Perpl hedge of a basis leg without touching spot (liquidation guard / re-hedge).
// Perpl margin is isolated (fork-verified): deposits and closed shares land in the free account balance, so collateral only
// reaches the position through perpl.order kind addMargin.
// lotDelta: perp lot units, < 0 reduces the short, > 0 adds to it (at the source leverage). collateralDelta: > 0 USDC atoms
// swapped to AUSD, deposited and added as position margin; < 0 AUSD atoms of free balance withdrawn back to USDC.
// margin: extra free-balance AUSD atoms (e.g. released by the close) moved into the position at the end. Plan.Legs = the updated leg.
func (p *Planner) ResizeBasis(ctx context.Context, s Strategy, leg LegState, lotDelta, collateralDelta, margin decimal.Decimal, slip int) (Plan, error) {
	var src *Source
	for i := range s.Sources {
		if s.Sources[i].Kind == "basis" {
			src = &s.Sources[i]
		}
	}
	pp, ok := perps[s.Asset]
	if src == nil || !ok {
		return Plan{}, fail("bad_request", "strategy has no basis source")
	}
	if !lotDelta.IsInteger() || d(leg.Lot).Add(lotDelta).IsNegative() || d(leg.Collateral).Add(decimal.Min(collateralDelta, decimal.Zero)).IsNegative() ||
		margin.IsNegative() || (margin.IsPositive() && collateralDelta.IsNegative()) {
		return Plan{}, fail("bad_request", "resize beyond the position")
	}
	var steps []Step
	out := leg
	if collateralDelta.IsPositive() {
		ausd := collateralDelta.Mul(decimal.NewFromInt(int64(10_000 - slip - poolFeeBps))).Div(bps).Floor()
		if !ausd.IsPositive() {
			return Plan{}, fail("bad_request", "amount too small")
		}
		steps = append(steps, Step{Action: "stable.swap", From: "USDC", To: "AUSD", Amount: collateralDelta.String(), MinOut: ausd.String()},
			Step{Action: "perpl.deposit", Amount: ausd.String()})
		out.Collateral = d(leg.Collateral).Add(ausd).String()
		margin = margin.Add(ausd)
	}
	if !lotDelta.IsZero() {
		q, err := p.quote(ctx, s.Asset, "buy", decimal.New(100, 6), slip) // $100 probe for the spot price
		if err != nil {
			return Plan{}, err
		}
		px := d(q.AvgPx)
		kind, f, lot := "openShort", decimal.NewFromInt(10_000-perplSlipBps), lotDelta
		lev := int(src.leverage().Mul(decimal.NewFromInt(100)).IntPart())
		if lotDelta.IsNegative() {
			kind, f, lot, lev = "closeShort", decimal.NewFromInt(10_000+perplSlipBps), lotDelta.Neg(), 100
		}
		limit := px.Mul(f).Div(bps).Shift(pp.PriceDec)
		if kind == "openShort" {
			limit = limit.Floor()
		} else {
			limit = limit.Ceil()
		}
		steps = append(steps, Step{Action: "perpl.order", Perp: pp.Name, Kind: kind, Price: limit.String(), Lot: lot.String(), LeverageHdths: lev})
		out.Lot = d(leg.Lot).Add(lotDelta).String()
	}
	if margin.IsPositive() {
		steps = append(steps, Step{Action: "perpl.order", Perp: pp.Name, Kind: "addMargin", Amount: margin.String()})
	}
	if collateralDelta.IsNegative() {
		amt := collateralDelta.Neg()
		min := amt.Mul(decimal.NewFromInt(int64(10_000 - slip - poolFeeBps))).Div(bps).Floor()
		steps = append(steps, Step{Action: "perpl.withdraw", Amount: amt.String()},
			Step{Action: "stable.swap", From: "AUSD", To: "USDC", Amount: amt.String(), MinOut: min.String()})
		out.Collateral = d(leg.Collateral).Sub(amt).String()
	}
	steps, err := p.check(p.Dep, steps)
	return Plan{Steps: steps, Legs: []LegState{out}}, err
}

// Rebalance moves value toward targetBps (same order as sources). Lending/staking move partially; basis only toggles
// fully off (target 0) or on (was off, target > 0); lp is never moved. Empty Steps = nothing to do.
func (p *Planner) Rebalance(ctx context.Context, s Strategy, legs []LegState, bals []Balance, targetBps []int, hasPerplAccount bool, slip int) (Plan, error) {
	n := len(s.Sources)
	if len(legs) != n || len(bals) != n || len(targetBps) != n {
		return Plan{}, fail("bad_request", "legs/balances/targets do not match sources")
	}
	total, sum := decimal.Zero, 0
	for i := range s.Sources {
		total, sum = total.Add(d(bals[i].Value)), sum+targetBps[i]
	}
	if sum != 10_000 {
		return Plan{}, fail("bad_request", "targets sum %d", sum)
	}
	out := append([]LegState(nil), legs...)
	var steps []Step
	freed := decimal.Zero
	deltas := make([]decimal.Decimal, n)
	px := func() (decimal.Decimal, error) {
		q, err := p.quote(ctx, s.Asset, "buy", decimal.New(100, 6), slip) // $100 probe for the spot price
		return d(q.AvgPx), err
	}
	for i, src := range s.Sources {
		cur := d(bals[i].Value)
		deltas[i] = bpsOf(total, targetBps[i]).Sub(cur)
		switch {
		case src.Kind == "lp" || deltas[i].Sign() >= 0:
		case src.Kind == "basis":
			if targetBps[i] != 0 || !d(legs[i].Lot).IsPositive() {
				deltas[i] = decimal.Zero // resize unsupported: toggle only
				continue
			}
			spot, err := px()
			if err != nil {
				return Plan{}, err
			}
			bs, usdc, _ := p.closeBasis(s.Asset, legs[i], spot, 10_000, slip)
			steps, freed = append(steps, bs...), freed.Add(d(legs[i].Asset))
			if usdc.IsPositive() {
				q, err := p.quote(ctx, s.Asset, "buy", usdc, slip)
				if err != nil {
					return Plan{}, err
				}
				steps, freed = append(steps, Step{Action: "kuru.marketBuy", Market: s.Asset, Quote: usdc.String(), MinOut: q.MinOut}), freed.Add(d(q.MinOut))
			}
			out[i] = LegState{Token: legs[i].Token, Kind: src.Kind, Venue: src.Venue, Asset: "0"}
		default:
			st, got := p.takeOut(s.Asset, src, deltas[i].Neg(), bals[i], false)
			steps, freed = append(steps, st...), freed.Add(got)
			out[i].Asset = decimal.Max(decimal.Zero, d(legs[i].Asset).Add(deltas[i])).String()
		}
	}
	// basis switching on takes its share first; the rest of freed is split over under-weight sources by delta
	want := decimal.Zero
	for i, src := range s.Sources {
		if deltas[i].IsPositive() && src.Kind == "basis" {
			if d(legs[i].Lot).IsPositive() {
				deltas[i] = decimal.Zero
				continue
			}
			alloc := decimal.Min(deltas[i], freed)
			l := src.leverage()
			sellAmt := alloc.Div(l.Add(decimal.NewFromInt(1))).Floor()
			q, err := p.quote(ctx, s.Asset, "sell", sellAmt, slip)
			if err != nil {
				return Plan{}, err
			}
			bs, leg, err := p.openBasis(s.Asset, src, alloc.Sub(sellAmt), d(q.MinOut), d(q.AvgPx), hasPerplAccount, slip)
			if err != nil {
				return Plan{}, err
			}
			steps = append(steps, Step{Action: "kuru.marketSell", Market: s.Asset, Size: sellAmt.String(), MinOut: q.MinOut})
			steps, out[i], freed = append(steps, bs...), leg, freed.Sub(alloc)
			deltas[i] = decimal.Zero
		}
		if deltas[i].IsPositive() && src.Kind != "lp" {
			want = want.Add(deltas[i])
		}
	}
	for i, src := range s.Sources {
		if !deltas[i].IsPositive() || src.Kind == "lp" || !want.IsPositive() {
			continue
		}
		amt := decimal.Min(deltas[i], freed.Mul(deltas[i]).Div(want).Floor())
		if amt.IsPositive() {
			steps = append(steps, p.supply(s.Asset, src, amt)...)
			out[i].Asset = d(legs[i].Asset).Add(amt).String()
		}
	}
	if len(steps) == 0 {
		return Plan{Legs: out}, nil
	}
	steps, err := p.check(p.Dep, steps)
	return Plan{Steps: steps, Legs: out}, err
}
