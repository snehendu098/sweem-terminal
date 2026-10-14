package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/snehendu098/sweem-terminal/internal/httpx"
	"github.com/snehendu098/sweem-terminal/internal/planner"
	"github.com/snehendu098/sweem-terminal/internal/streams"
)

// History is the Backtester over the history service.
type History struct {
	URL  string
	HTTP *http.Client
}

func (h History) Backtest(ctx context.Context, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL+"/api/v1/backtest", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := h.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return res.StatusCode, b, err
}

type APYSource struct {
	Asset string  `json:"asset"`
	Kind  string  `json:"kind"`
	Venue string  `json:"venue"`
	APY   *string `json:"apy"`
}

type APY struct {
	Estimate string      `json:"estimate"`
	Sources  []APYSource `json:"sources"`
}

// liveAPY: Σ sleeve weight × source weight × current venue rate; basis earns short funding (7d) on the L/(L+1) spot
// notional. Missing rates count 0.
func (s *Server) liveAPY(st planner.Strategy, r streams.RatesSnapshot) *APY {
	out := &APY{Sources: []APYSource{}}
	est := decimal.Zero
	for _, pt := range st.Parts() {
		e := s.partAPY(pt.Asset, pt.Sources, r, out)
		est = est.Add(e.Mul(decimal.NewFromInt(int64(pt.WeightBps))).Div(decimal.NewFromInt(10_000)))
	}
	out.Estimate = est.StringFixed(6)
	return out
}

// partAPY appends one sleeve's sources to out and returns its weighted APY.
func (s *Server) partAPY(asset string, srcs []planner.Source, r streams.RatesSnapshot, out *APY) decimal.Decimal {
	est := decimal.Zero
	for _, src := range srcs {
		var apy *decimal.Decimal
		for _, x := range r.Rates {
			a := asset
			if src.Kind == "lending" && asset == "MON" {
				a = "WMON"
			}
			if x.Source == src.Kind && x.Venue == src.Venue && x.Asset == a {
				v := x.Apy
				apy = &v
			}
		}
		if src.Kind == "hold" {
			z := decimal.Zero // spot in the wallet earns nothing
			apy = &z
		}
		if src.Kind == "basis" {
			perpID := s.Plan.PerpID(asset)
			for _, f := range r.Funding {
				if f.PerpID == perpID {
					l, _ := decimal.NewFromString(src.Params.Leverage)
					v := f.AprShort7d.Mul(l).Div(l.Add(decimal.NewFromInt(1)))
					apy = &v
				}
			}
		}
		as := APYSource{Asset: asset, Kind: src.Kind, Venue: src.Venue}
		if apy != nil {
			str := apy.StringFixed(6)
			as.APY = &str
			est = est.Add(apy.Mul(decimal.NewFromInt(int64(src.WeightBps))).Div(decimal.NewFromInt(10_000)))
		}
		out.Sources = append(out.Sources, as)
	}
	return est
}

func (s *Server) withAPY(ctx context.Context, xs []Strategy) {
	r, err := s.Rates.Rates(ctx)
	if err != nil {
		slog.Warn("rates", "err", err)
		return
	}
	for i := range xs {
		xs[i].APY = s.liveAPY(xs[i].Strategy, r)
	}
}

func (s *Server) strategies(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Strategies(r.Context(), r.URL.Query().Get("creator"), false)
	if err == nil {
		s.withAPY(r.Context(), v)
	}
	reply(w, "strategies", v, err)
}

func (s *Server) myStrategies(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Strategies(r.Context(), did(r), true)
	if err == nil {
		s.withAPY(r.Context(), v)
	}
	reply(w, "my strategies", v, err)
}

func (s *Server) strategy(w http.ResponseWriter, r *http.Request) {
	if id, ok := id(w, r); ok {
		v, err := s.Store.Strategy(r.Context(), id, did(r))
		if err == nil {
			xs := []Strategy{v}
			s.withAPY(r.Context(), xs)
			v = xs[0]
		}
		reply(w, "strategy", v, err)
	}
}

type StrategyInput struct {
	Name string `json:"name"`
	planner.Strategy
	Public *bool `json:"public"`
}

func (s *Server) createStrategy(w http.ResponseWriter, r *http.Request) {
	var in StrategyInput
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Sleeves) == 1 { // one token = the single-asset shape
		in.Asset, in.Sources, in.Sleeves = in.Sleeves[0].Asset, in.Sleeves[0].Sources, nil
	}
	if in.Name == "" || len(in.Name) > 64 {
		httpx.Error(w, http.StatusBadRequest, "invalid_name")
		return
	}
	if err := s.Plan.Validate(in.Strategy); err != nil {
		fail(w, "validate strategy", err)
		return
	}
	v, err := s.Store.CreateStrategy(r.Context(), did(r), in)
	if err != nil {
		fail(w, "create strategy", err)
		return
	}
	// best effort: a strategy is listed without a backtest when history is down or lacks data
	if status, _, sum, err := s.runBacktest(r.Context(), v, BacktestInput{}); err == nil && status == 200 {
		v.Backtest = sum
	}
	httpx.JSON(w, http.StatusCreated, v)
}

type BacktestInput struct {
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Amount string `json:"amount,omitempty"`
}

func bpsFrac(w int) string {
	return decimal.NewFromInt(int64(w)).Div(decimal.NewFromInt(10_000)).String()
}

// historyBody is one single-asset strategy as history's backtest request (asset, legs, rules); venues are the raw ids
// history stores rates under (aave, neverland, 4626 slug, shmonad, kuru-activevault).
func (s *Server) historyBody(st planner.Strategy) map[string]any {
	legs := []map[string]any{}
	rules := map[string]any{}
	for _, src := range st.Sources {
		leg := map[string]any{"kind": src.Kind, "venue": src.Venue, "weight": bpsFrac(src.WeightBps)}
		if src.Kind == "basis" {
			leg["venue"] = ""
			leg["perpId"] = s.Plan.PerpID(st.Asset)
			rules["basisLeverage"] = src.Params.Leverage
		}
		legs = append(legs, leg)
	}
	if d := st.Rules.DriftBps; d > 0 {
		rules["driftBps"] = fmt.Sprint(d)
	}
	if m := st.Rules.MinRebalanceSeconds; m > 0 {
		rules["minIntervalDays"] = int(m / 86400)
	}
	if g := st.Rules.FundingGate; g != nil {
		rules["fundingGate"] = map[string]any{"apr": g.APR, "lookbackDays": g.LookbackDays}
	}
	return map[string]any{"asset": st.Asset, "legs": legs, "rules": rules}
}

// runBacktest calls history; on 200 it stores and returns the summary next to history's full response.
// Multi-token: {amount (USD), from, to, sleeves:[historyBody(part) + amount "1" + weight]}; history combines them.
func (s *Server) runBacktest(ctx context.Context, st Strategy, in BacktestInput) (status int, full []byte, summary json.RawMessage, err error) {
	amount := in.Amount
	if amount == "" {
		amount = "1"
		if st.Asset == "USDC" || len(st.Sleeves) > 0 {
			amount = "1000"
		}
	}
	req := s.historyBody(st.Strategy)
	if len(st.Sleeves) > 0 {
		var sleeves []map[string]any
		for _, pt := range st.Sleeves {
			b := s.historyBody(st.Part(pt))
			b["amount"], b["weight"] = "1", bpsFrac(pt.WeightBps)
			sleeves = append(sleeves, b)
		}
		req = map[string]any{"sleeves": sleeves}
	}
	req["amount"] = amount
	if in.From != "" {
		req["from"] = in.From
	}
	if in.To != "" {
		req["to"] = in.To
	}
	body, _ := json.Marshal(req)
	status, full, err = s.History.Backtest(ctx, body)
	if err != nil || status != 200 {
		return status, full, nil, err
	}
	var res struct {
		From, To                                   string
		Days                                       int
		ApyUsd, ApyAsset, MaxDrawdownUsd, FinalUsd string
		Hodl                                       struct{ ApyUsd string }
	}
	if err := json.Unmarshal(full, &res); err != nil {
		return 0, nil, nil, err
	}
	summary, _ = json.Marshal(map[string]any{"from": res.From, "to": res.To, "days": res.Days, "apyUsd": res.ApyUsd, "apyAsset": res.ApyAsset,
		"maxDrawdownUsd": res.MaxDrawdownUsd, "finalUsd": res.FinalUsd, "hodlApyUsd": res.Hodl.ApyUsd, "ranAt": time.Now().UTC()})
	if err := s.Store.SetBacktest(ctx, st.ID, summary); err != nil {
		slog.Error("store backtest", "err", err)
	}
	return status, full, summary, nil
}

func (s *Server) backtest(w http.ResponseWriter, r *http.Request) {
	sid, ok := id(w, r)
	var in BacktestInput
	if !ok || (r.ContentLength != 0 && !decode(w, r, &in)) {
		return
	}
	st, err := s.Store.Strategy(r.Context(), sid, "")
	if err != nil {
		fail(w, "strategy", err)
		return
	}
	status, full, _, err := s.runBacktest(r.Context(), st, in)
	if err != nil {
		slog.Error("history", "err", err)
		httpx.Error(w, http.StatusBadGateway, "history_unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(full)
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	sid, ok := id(w, r)
	var in struct {
		AmountUsdc  string
		SlippageBps *int
	}
	if !ok || !decode(w, r, &in) {
		return
	}
	amt, oka := human(in.AmountUsdc)
	sl, oks := slip(in.SlippageBps)
	if !oka || !oks {
		httpx.Error(w, http.StatusBadRequest, "bad_request")
		return
	}
	ctx, me := r.Context(), did(r)
	st, err := s.Store.Strategy(ctx, sid, me)
	var wallet string
	if err == nil {
		wallet, err = s.Store.Wallet(ctx, me)
	}
	var perpl bool
	if err == nil {
		perpl, err = s.Store.HasPerplAccount(ctx, me, wallet)
	}
	if err != nil {
		fail(w, "deploy", err)
		return
	}
	atoms, _ := s.Plan.ToAtoms("USDC", amt)
	plan, err := s.Plan.Deploy(ctx, st.Strategy, atoms, perpl, sl)
	if err != nil {
		fail(w, "plan deploy", err)
		return
	}
	fee := decimal.RequireFromString(plan.FeeUSDC).Shift(-6)
	owed := decimal.Zero
	if st.Creator != me {
		owed = fee.Mul(decimal.NewFromInt(planner.CreatorBps)).Div(decimal.NewFromInt(planner.FeeBps))
	}
	posID, execID, err := s.Store.OpenPosition(ctx, me, wallet, st, amt.String(), plan, fee.String(), owed.String())
	if err != nil {
		fail(w, "open position", err)
		return
	}
	s.start(execID, s.settle(posID, "open", "failed"))
	accepted(w, execID, "deploy", plan.Steps, map[string]any{"positionId": posID, "feeUsdc": fee.String()})
}

// settle moves a position to okStatus or failStatus once its execution ends.
func (s *Server) settle(posID, okStatus, failStatus string) func(context.Context, bool) {
	return func(ctx context.Context, ok bool) {
		st := failStatus
		if ok {
			st = okStatus
		}
		if err := s.Store.SettlePosition(ctx, posID, st); err != nil {
			slog.Error("settle position", "position", posID, "err", err)
		}
	}
}

func (s *Server) positions(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Positions(r.Context(), did(r))
	reply(w, "positions", v, err)
}

// withdraw unwinds an open position, or resumes a failed withdraw from its step_done. Every step is capped by live
// on-chain state (planner.ResumeWithdraw), so a retry never re-closes a short or withdraws twice.
func (s *Server) withdraw(w http.ResponseWriter, r *http.Request) {
	pid, ok := id(w, r)
	var in struct{ Bps, SlippageBps *int }
	if !ok || (r.ContentLength != 0 && !decode(w, r, &in)) {
		return
	}
	sl, oks := slip(in.SlippageBps)
	bps := 10_000
	if in.Bps != nil {
		bps = *in.Bps
	}
	if !oks || bps < 1 || bps > 10_000 {
		httpx.Error(w, http.StatusBadRequest, "bad_request")
		return
	}
	ctx, me := r.Context(), did(r)
	pos, st, err := s.Store.Position(ctx, me, pid)
	if err == nil && pos.Status != "open" && pos.Status != "withdrawing" {
		err = ErrNotOpen
	}
	var peers []planner.Peer
	if err == nil {
		_, err = s.Store.Wallet(ctx, me)
	}
	if err == nil {
		peers, err = s.Store.Peers(ctx, me, pid)
	}
	var last LastWithdraw
	var resume bool
	if err == nil && pos.Status == "withdrawing" {
		last, resume, err = s.Store.LastWithdraw(ctx, pid)
		if err == nil && (last.Status == "pending" || last.Status == "submitted") {
			err = ErrWithdrawBusy
		}
	}
	if err != nil {
		fail(w, "withdraw", err)
		return
	}
	live, err := planner.ReadLive(ctx, s.Chain, s.Plan.Dep, pos.Wallet, st, pos.Legs, peers)
	if err != nil {
		slog.Error("live state", "err", err)
		httpx.Error(w, http.StatusBadGateway, "rpc_unavailable")
		return
	}
	if bps < 10_000 && pos.Status == "open" { // partial sell: position stays open with smaller legs
		plan, err := s.Plan.WithdrawBps(ctx, st, pos.Legs, live.Bals, bps, sl)
		if err != nil {
			fail(w, "plan withdraw", err)
			return
		}
		execID, err := s.Store.PartialWithdraw(ctx, me, pid, NewExec{Kind: "withdraw", Steps: plan.Steps, PositionID: &pid, Legs: plan.Legs})
		if err != nil {
			fail(w, "partial withdraw", err)
			return
		}
		s.start(execID, nil)
		accepted(w, execID, "withdraw", plan.Steps, map[string]any{"positionId": pid, "bps": bps})
		return
	}
	steps, done := last.Steps, last.StepDone
	if !resume {
		plan, err := s.Plan.Withdraw(ctx, st, pos.Legs, live.Bals, sl)
		if err != nil && !isNothing(err) {
			fail(w, "plan withdraw", err)
			return
		}
		steps, done = plan.Steps, 0
	}
	steps, err = s.Plan.ResumeWithdraw(ctx, st, steps, done, live, sl)
	if err != nil {
		fail(w, "resume withdraw", err)
		return
	}
	if len(steps) == 0 {
		// nothing left on chain to unwind
		if err := s.Store.SettlePosition(ctx, pid, "closed"); err != nil {
			fail(w, "settle", err)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"positionId": pid, "status": "closed", "steps": []planner.Step{}})
		return
	}
	execID, err := s.Store.StartWithdraw(ctx, me, pid, NewExec{Kind: "withdraw", Steps: steps, PositionID: &pid})
	if err != nil {
		fail(w, "start withdraw", err)
		return
	}
	s.start(execID, s.settle(pid, "closed", "withdrawing"))
	accepted(w, execID, "withdraw", steps, map[string]any{"positionId": pid, "resumed": resume})
}

func isNothing(err error) bool {
	var pe *planner.Error
	return errors.As(err, &pe) && pe.Code == "nothing_to_sell"
}
