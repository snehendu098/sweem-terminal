package planner

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/snehendu098/sweem-terminal/internal/config"
)

// fakeQ prices every market at a fixed USDC price; minOut = expected × (1 − slip). Amounts over maxSafe are flagged by the planner.
type fakeQ struct {
	calls []string
}

var fakePx = map[string]string{"MON": "0.03", "WETH": "4000", "cbBTC": "100000", "XAUt0": "3500"}

func (f *fakeQ) Quote(_ context.Context, ob, side string, amount decimal.Decimal, slip int) (Quote, error) {
	f.calls = append(f.calls, side+" "+ob+" "+amount.String())
	dep := testDep()
	var sym string
	for s, m := range dep.Kuru.Markets {
		if strings.EqualFold(m.Orderbook, ob) {
			sym = s
		}
	}
	px, dec := d(fakePx[sym]), int32(dep.Tokens[sym].Decimals)
	var out decimal.Decimal
	if side == "buy" {
		out = amount.Shift(-6).Div(px).Shift(dec).Floor()
	} else {
		out = amount.Shift(-dec).Mul(px).Shift(6).Floor()
	}
	if amount.LessThan(decimal.NewFromInt(1000)) {
		return Quote{}, &Error{"below_min_size", ""}
	}
	return Quote{Amount: amount.String(), Fillable: amount.String(), ExpectedOut: out.String(),
		MinOut: out.Mul(decimal.NewFromInt(int64(10_000 - slip))).Div(bps).Floor().String(), AvgPx: px.String(),
		MaxSafeAmount: "1000000000000000000000000000"}, nil
}

func testDep() *config.Deployments143 {
	dep, err := config.LoadDeployments143("../../deployments/143.json")
	if err != nil {
		panic(err)
	}
	dep.Treasury = "0x7777777777777777777777777777777777777777"
	return dep
}

func planner() (*Planner, *fakeQ) {
	q := &fakeQ{}
	return &Planner{Dep: testDep(), Q: q, MaxStepUSD: 1000}, q
}

func js(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func golden(t *testing.T, name string, got any, want string) {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: bad want: %v", name, err)
	}
	wb, _ := json.Marshal(w)
	var g any
	json.Unmarshal([]byte(js(t, got)), &g)
	gb, _ := json.Marshal(g)
	if string(gb) != string(wb) {
		t.Errorf("%s:\n got %s\nwant %s", name, gb, wb)
	}
}

func code(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// Step JSON round-trips the executor's own fixture byte-for-byte in meaning: same keys, no extras.
func TestStepMatchesExecutorFixture(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/contracts/executor/user_steps.plan.json")
	if err != nil {
		t.Fatal(err)
	}
	var want []map[string]any
	var steps []Step
	json.Unmarshal(raw, &want)
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	json.Unmarshal([]byte(js(t, steps)), &got)
	if len(got) != len(want) {
		t.Fatalf("len %d vs %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("step %d:\n got %v\nwant %v", i, got[i], want[i])
		}
	}
}

func TestTrade(t *testing.T) {
	p, _ := planner()
	ctx := context.Background()
	s, err := p.MarketBuy(ctx, "WETH", decimal.NewFromInt(100_000_000), 50)
	golden(t, "buy", s, `[{"action":"kuru.marketBuy","market":"WETH","quote":"100000000","minOut":"24875000000000000"}]`)
	s, err = p.MarketSell(ctx, "MON", decimal.RequireFromString("250000000000000000000"), 100)
	golden(t, "sell", s, `[{"action":"kuru.marketSell","market":"MON","size":"250000000000000000000","minOut":"7425000"}]`)
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"cap":         second(p.MarketBuy(ctx, "WETH", decimal.NewFromInt(1_000_000_001), 50)),
		"min":         second(p.MarketBuy(ctx, "WETH", decimal.NewFromInt(999), 50)),
		"market":      second(p.MarketBuy(ctx, "PEPE", decimal.NewFromInt(1e6), 50)),
		"limit small": second(p.Limit("MON", "buy", d("0.03"), d("199"), true, false)),
	} {
		want := map[string]string{"cap": "cap_exceeded", "min": "below_min_size", "market": "bad_request",
			"limit small": "below_min_size"}[name]
		if code(err) != want {
			t.Errorf("%s: got %v want %s", name, err, want)
		}
	}
}

func second[T any](_ T, err error) error { return err }

func TestLimitAndMargin(t *testing.T) {
	p, _ := planner()
	s, err := p.Limit("WETH", "buy", d("3600.0099"), d("0.01"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	// price floored to tick 100 in 1e4 precision; deposit = 3600 × 0.01 USDC
	golden(t, "limit", s, `[{"action":"kuru.marginDeposit","token":"USDC","amount":"36000000"},
		{"action":"kuru.limit","market":"WETH","side":"buy","price":"36000000","size":"100000000","postOnly":true}]`)
	s, _ = p.Limit("MON", "sell", d("0.031"), d("250"), false, true)
	golden(t, "limit sell", s, `[{"action":"kuru.marginDeposit","token":"MON","amount":"250000000000000000000"},
		{"action":"kuru.limit","market":"MON","side":"sell","price":"3100000","size":"2500000000000","postOnly":false}]`)
	s, _ = p.Cancel("WETH", []string{"1234"})
	golden(t, "cancel", s, `[{"action":"kuru.cancel","market":"WETH","orderIds":["1234"]}]`)
	s, _ = p.Margin(false, "USDC", decimal.NewFromInt(5))
	golden(t, "margin", s, `[{"action":"kuru.marginWithdraw","token":"USDC","amount":"5"}]`)
}

func TestValidate(t *testing.T) {
	p, _ := planner()
	src := func(k, v string, w int, lev string) Source { return Source{k, v, w, SourceParams{lev}} }
	cases := []struct {
		s    Strategy
		want string
	}{
		{Strategy{Asset: "USDC", Sources: []Source{src("lending", "aave", 5000, ""), src("lending", "euler-k3usdc", 5000, "")}}, ""},
		{Strategy{Asset: "MON", Sources: []Source{src("lending", "neverland", 2500, ""), src("staking", "shmonad", 2500, ""), src("lp", "kuru-activevault", 2500, ""), src("basis", "perpl", 2500, "2")}}, ""},
		{Strategy{Asset: "MON", Sources: []Source{src("lending", "aave", 10000, "")}}, "unsupported_source"},
		{Strategy{Asset: "DOGE", Sources: []Source{src("lending", "aave", 10000, "")}}, "invalid_asset"},
		{Strategy{Asset: "USDC", Sources: []Source{src("lending", "aave", 9000, "")}}, "invalid_sources"},
		{Strategy{Asset: "USDC", Sources: []Source{src("lending", "euler-k3weth", 10000, "")}}, "unsupported_source"},
		{Strategy{Asset: "USDC", Sources: []Source{src("staking", "shmonad", 10000, "")}}, "unsupported_source"},
		{Strategy{Asset: "USDC", Sources: []Source{src("basis", "perpl", 10000, "2")}}, "unsupported_source"},
		{Strategy{Asset: "WETH", Sources: []Source{src("basis", "perpl", 10000, "13")}}, "invalid_sources"},
		{Strategy{Asset: "WETH", Sources: []Source{src("basis", "perpl", 10000, "1.005")}}, "invalid_sources"},
		{Strategy{Asset: "USDC", Sources: []Source{src("lending", "aave", 5000, ""), src("lending", "aave", 5000, "")}}, "invalid_sources"},
	}
	for i, c := range cases {
		if got := code(p.Validate(c.s)); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

var monStrat = Strategy{Asset: "MON", Sources: []Source{
	{"lending", "neverland", 4000, SourceParams{}},
	{"staking", "shmonad", 2000, SourceParams{}},
	{"lp", "kuru-activevault", 2000, SourceParams{}},
	{"basis", "perpl", 2000, SourceParams{"3"}},
}}

func TestDeploy(t *testing.T) {
	p, q := planner()
	ctx := context.Background()
	usdcStrat := Strategy{Asset: "USDC", Sources: []Source{{"lending", "aave", 6000, SourceParams{}}, {"lending", "morpho-hyperusdca", 4000, SourceParams{}}}}
	plan, err := p.Deploy(ctx, usdcStrat, decimal.NewFromInt(100_000_000), false, 50)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "usdc deploy", plan, `{"steps":[
		{"action":"fee.transfer","amount":"400000"},
		{"action":"lend.supply","venue":"aaveV3","asset":"USDC","amount":"59760000"},
		{"action":"vault.deposit","vault":"0x78999cc96d2Ba0341588C60CcB0E91c6C33CF371","amount":"39840000"}],
		"feeUsdc":"400000","legs":[{"token":"USDC","kind":"lending","venue":"aave","asset":"59760000"},{"token":"USDC","kind":"lending","venue":"morpho-hyperusdca","asset":"39840000"}]}`)
	if len(q.calls) != 0 {
		t.Errorf("usdc deploy quoted: %v", q.calls)
	}

	// $1000: fee 4, net 996; buys: lending 398.4, staking 199.2, lp 99.6 (half), basis 149.4 (3/4) = 846.6 USDC in one buy
	plan, err = p.Deploy(ctx, monStrat, decimal.NewFromInt(1_000_000_000), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "mon deploy", plan, `{"steps":[
		{"action":"fee.transfer","amount":"4000000"},
		{"action":"kuru.marketBuy","market":"MON","quote":"846600000","minOut":"28220000000000000000000"},
		{"action":"wmon.wrap","amount":"13280000000000000000000"},
		{"action":"lend.supply","venue":"neverland","asset":"WMON","amount":"13280000000000000000000"},
		{"action":"shmonad.deposit","amount":"6640000000000000000000"},
		{"action":"activeVault.deposit","base":"3320000000000000000000","quote":"99600000"},
		{"action":"stable.swap","from":"USDC","to":"AUSD","amount":"49800000","minOut":"49775100"},
		{"action":"perpl.createAccount","amount":"49775100"},
		{"action":"perpl.order","perp":"MON","kind":"openShort","price":"29700","lot":"4980","leverageHdths":300}],
		"feeUsdc":"4000000","legs":[
		{"token":"MON","kind":"lending","venue":"neverland","asset":"13280000000000000000000"},
		{"token":"MON","kind":"staking","venue":"shmonad","asset":"6640000000000000000000"},
		{"token":"MON","kind":"lp","venue":"kuru-activevault","asset":"3320000000000000000000"},
		{"token":"MON","kind":"basis","venue":"perpl","asset":"4980000000000000000000","collateral":"49775100","lot":"4980","entryPx":"0.03"}]}`)

	plan, _ = p.Deploy(ctx, monStrat, decimal.NewFromInt(1_000_000_000), true, 0)
	if plan.Steps[7].Action != "perpl.deposit" {
		t.Errorf("existing account should deposit: %+v", plan.Steps[7])
	}
	for name, c := range map[string]struct {
		amt  int64
		dep  func(*Planner)
		want string
	}{
		"treasury":  {1e9, func(p *Planner) { p.Dep.Treasury = "" }, "treasury_unset"},
		"tiny":      {100, nil, "below_min_size"},
		"perpl min": {50_000_000, nil, "below_min_size"}, // basis collateral $2.5 < 10 AUSD
		"cap":       {3_000_000_000, nil, "cap_exceeded"},
	} {
		p, _ := planner()
		if c.dep != nil {
			c.dep(p)
		}
		if _, err := p.Deploy(ctx, monStrat, decimal.NewFromInt(c.amt), false, 0); code(err) != c.want {
			t.Errorf("%s: got %v want %s", name, err, c.want)
		}
	}
}

func TestWithdraw(t *testing.T) {
	p, _ := planner()
	ctx := context.Background()
	legs := []LegState{
		{Kind: "lending", Venue: "neverland", Asset: "13280000000000000000000"},
		{Kind: "staking", Venue: "shmonad", Asset: "6640000000000000000000"},
		{Kind: "lp", Venue: "kuru-activevault", Asset: "3320000000000000000000"},
		{Kind: "basis", Venue: "perpl", Asset: "4980000000000000000000", Collateral: "49775100", Lot: "4980", EntryPx: "0.03"},
	}
	bals := []Balance{{Value: "13300000000000000000000"}, {Value: "6650000000000000000000", Shares: "6000000000000000000000"}, {Value: "3320000000000000000000", Shares: "777"}, {Value: "0"}}
	plan, err := p.Withdraw(ctx, monStrat, legs, bals, 0)
	if err != nil {
		t.Fatal(err)
	}
	// sell = 13300 + 6650×0.99 + 3320 + 4980 = 28183.5 MON; basis at px 0.03: pnl 0, fee+buffer 53.45 bps of $149.4
	golden(t, "mon withdraw", plan.Steps, `[
		{"action":"lend.withdraw","venue":"neverland","asset":"WMON","amount":"13300000000000000000000"},
		{"action":"wmon.unwrap","amount":"13300000000000000000000"},
		{"action":"shmonad.redeem","shares":"6000000000000000000000"},
		{"action":"activeVault.withdraw","shares":"777"},
		{"action":"perpl.order","perp":"MON","kind":"closeShort","price":"30300","lot":"4980","leverageHdths":100},
		{"action":"perpl.withdraw","amount":"48976557"},
		{"action":"stable.swap","from":"AUSD","to":"USDC","amount":"48976557","minOut":"48952068"},
		{"action":"kuru.marketSell","market":"MON","size":"28183500000000000000000","minOut":"845505000"}]`)
	if _, err := p.Withdraw(ctx, monStrat, legs[:1], bals, 0); code(err) != "bad_request" {
		t.Errorf("mismatch: %v", err)
	}
}

func TestRebalance(t *testing.T) {
	p, _ := planner()
	ctx := context.Background()
	s := Strategy{Asset: "WETH", Sources: []Source{
		{"lending", "aave", 5000, SourceParams{}},
		{"lending", "euler-k3weth", 3000, SourceParams{}},
		{"basis", "perpl", 2000, SourceParams{"2"}},
	}}
	legs := []LegState{{Kind: "lending", Venue: "aave", Asset: "500000000000000000"}, {Kind: "lending", Venue: "euler-k3weth", Asset: "300000000000000000"},
		{Kind: "basis", Venue: "perpl", Asset: "133333333333333333", Collateral: "266000000", Lot: "133", EntryPx: "4000"}}
	bals := []Balance{{Value: "600000000000000000"}, {Value: "200000000000000000", Shares: "190000000000000000"}, {Value: "200000000000000000"}}

	// drift: aave over by 0.1, euler under by 0.1 → one withdraw + one supply
	plan, err := p.Rebalance(ctx, s, legs, bals, []int{5000, 3000, 2000}, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "drift", plan.Steps, `[
		{"action":"lend.withdraw","venue":"aaveV3","asset":"WETH","amount":"100000000000000000"},
		{"action":"vault.deposit","vault":"0x2C803c8C3C57b9D4A9abA011615561315c2b6aC1","amount":"100000000000000000"}]`)

	// funding gate off: basis → 0; freed = spot 0.1333 + rebought collateral ($266 − 53.45 bps of $532 notional), split over both under-weights
	plan, err = p.Rebalance(ctx, s, legs, bals, []int{7000, 3000, 0}, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "gate off", plan.Steps, `[
		{"action":"perpl.order","perp":"ETH","kind":"closeShort","price":"404000","lot":"133","leverageHdths":100},
		{"action":"perpl.withdraw","amount":"263156460"},
		{"action":"stable.swap","from":"AUSD","to":"USDC","amount":"263156460","minOut":"263024881"},
		{"action":"kuru.marketBuy","market":"WETH","quote":"263024881","minOut":"65756220250000000"},
		{"action":"lend.supply","venue":"aaveV3","asset":"WETH","amount":"99544776791666666"},
		{"action":"vault.deposit","vault":"0x2C803c8C3C57b9D4A9abA011615561315c2b6aC1","amount":"99544776791666666"}]`)
	if plan.Legs[2].Lot != "" || plan.Legs[2].Asset != "0" {
		t.Errorf("basis leg not cleared: %+v", plan.Legs[2])
	}

	// gate back on: basis 0 → 20%, taken from aave
	legs2 := append([]LegState(nil), legs...)
	legs2[2] = LegState{Kind: "basis", Venue: "perpl", Asset: "0"}
	bals2 := []Balance{{Value: "700000000000000000"}, {Value: "300000000000000000", Shares: "1"}, {Value: "0"}}
	plan, err = p.Rebalance(ctx, s, legs2, bals2, []int{5000, 3000, 2000}, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "gate on", plan.Steps, `[
		{"action":"lend.withdraw","venue":"aaveV3","asset":"WETH","amount":"200000000000000000"},
		{"action":"kuru.marketSell","market":"WETH","size":"66666666666666666","minOut":"266666666"},
		{"action":"stable.swap","from":"USDC","to":"AUSD","amount":"266666666","minOut":"266533332"},
		{"action":"perpl.deposit","amount":"266533332"},
		{"action":"perpl.order","perp":"ETH","kind":"openShort","price":"396000","lot":"133","leverageHdths":200}]`)

	plan, err = p.Rebalance(ctx, s, legs2, bals2, []int{7000, 3000, 0}, true, 0)
	if err != nil || len(plan.Steps) != 0 {
		t.Errorf("balanced should be empty: %v %v", plan.Steps, err)
	}
}

func TestWithdrawBpsAndResize(t *testing.T) {
	p, _ := planner()
	ctx := context.Background()
	s := Strategy{Asset: "WETH", Sources: []Source{{"lending", "aave", 5000, SourceParams{}}, {"lending", "euler-k3weth", 3000, SourceParams{}}, {"basis", "perpl", 2000, SourceParams{"2"}}}}
	legs := []LegState{{Kind: "lending", Venue: "aave", Asset: "500000000000000000"}, {Kind: "lending", Venue: "euler-k3weth", Asset: "300000000000000000"},
		{Kind: "basis", Venue: "perpl", Asset: "133000000000000000", Collateral: "266000000", Lot: "134", EntryPx: "4000"}}
	bals := []Balance{{Value: "500000000000000000"}, {Value: "300000000000000000", Shares: "290000000000000000"}, {Value: "200000000000000000"}}
	plan, err := p.WithdrawBps(ctx, s, legs, bals, 5000, 0)
	if err != nil {
		t.Fatal(err)
	}
	// half of each: exact lending/vault withdraws (not redeem), close 67 of 134 lots, half collateral − 53.45 bps of $268 closed notional
	golden(t, "half", plan, `{"steps":[
		{"action":"lend.withdraw","venue":"aaveV3","asset":"WETH","amount":"250000000000000000"},
		{"action":"vault.withdraw","vault":"0x2C803c8C3C57b9D4A9abA011615561315c2b6aC1","amount":"150000000000000000"},
		{"action":"perpl.order","perp":"ETH","kind":"closeShort","price":"404000","lot":"67","leverageHdths":100},
		{"action":"perpl.withdraw","amount":"131567540"},
		{"action":"stable.swap","from":"AUSD","to":"USDC","amount":"131567540","minOut":"131501756"},
		{"action":"kuru.marketSell","market":"WETH","size":"466500000000000000","minOut":"1866000000"}],
		"legs":[{"kind":"lending","venue":"aave","asset":"250000000000000000"},{"kind":"lending","venue":"euler-k3weth","asset":"150000000000000000"},
		{"kind":"basis","venue":"perpl","asset":"66500000000000000","collateral":"133000000","lot":"67","entryPx":"4000"}]}`)
	if _, err := p.WithdrawBps(ctx, s, legs, bals, 0, 0); code(err) != "bad_request" {
		t.Errorf("bps 0: %v", err)
	}

	// guard: cut the short by 34 lots, add $50 collateral and re-margin 20 AUSD the close releases (isolated margin)
	plan, err = p.ResizeBasis(ctx, s, legs[2], decimal.NewFromInt(-34), decimal.NewFromInt(50_000_000), decimal.NewFromInt(20_000_000), 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "resize down", plan, `{"steps":[
		{"action":"stable.swap","from":"USDC","to":"AUSD","amount":"50000000","minOut":"49975000"},
		{"action":"perpl.deposit","amount":"49975000"},
		{"action":"perpl.order","perp":"ETH","kind":"closeShort","price":"404000","lot":"34","leverageHdths":100},
		{"action":"perpl.order","perp":"ETH","kind":"addMargin","amount":"69975000"}],
		"legs":[{"kind":"basis","venue":"perpl","asset":"133000000000000000","collateral":"315975000","lot":"100","entryPx":"4000"}]}`)
	plan, _ = p.ResizeBasis(ctx, s, legs[2], decimal.NewFromInt(10), decimal.NewFromInt(-6_000_000), decimal.Zero, 0)
	golden(t, "resize up + free collateral", plan.Steps, `[
		{"action":"perpl.order","perp":"ETH","kind":"openShort","price":"396000","lot":"10","leverageHdths":200},
		{"action":"perpl.withdraw","amount":"6000000"},
		{"action":"stable.swap","from":"AUSD","to":"USDC","amount":"6000000","minOut":"5997000"}]`)
	for name, err := range map[string]error{
		"over close":  second(p.ResizeBasis(ctx, s, legs[2], decimal.NewFromInt(-135), decimal.Zero, decimal.Zero, 0)),
		"over collat": second(p.ResizeBasis(ctx, s, legs[2], decimal.Zero, decimal.NewFromInt(-266_000_001), decimal.Zero, 0)),
		"no basis":    second(p.ResizeBasis(ctx, Strategy{Asset: "WETH", Sources: s.Sources[:2]}, legs[2], decimal.NewFromInt(-1), decimal.Zero, decimal.Zero, 0)),
		"nothing":     second(p.ResizeBasis(ctx, s, legs[2], decimal.Zero, decimal.Zero, decimal.Zero, 0)),
		"neg margin":  second(p.ResizeBasis(ctx, s, legs[2], decimal.Zero, decimal.Zero, decimal.NewFromInt(-1), 0)),
	} {
		want := map[string]string{"neg margin": "bad_request", "over close": "bad_request", "over collat": "bad_request", "no basis": "bad_request", "nothing": "nothing_to_sell"}[name]
		if code(err) != want {
			t.Errorf("%s: %v", name, err)
		}
	}
	if code(p.Validate(Strategy{Asset: "USDC", Sources: []Source{{"lending", "aave", 10000, SourceParams{}}}, Rules: Rules{RateSwitch: &RateSwitch{"0"}}})) != "invalid_rules" {
		t.Error("rateSwitch minGain 0 accepted")
	}
}

// 40% USDC aave, 30% WETH basis 2x, 30% MON half neverland / half basis 1x.
var multiStrat = Strategy{Sleeves: []Sleeve{
	{"USDC", 4000, []Source{{"lending", "aave", 10000, SourceParams{}}}},
	{"WETH", 3000, []Source{{"basis", "perpl", 10000, SourceParams{"2"}}}},
	{"MON", 3000, []Source{{"lending", "neverland", 5000, SourceParams{}}, {"basis", "perpl", 5000, SourceParams{"1"}}}},
}}

func TestValidateSleeves(t *testing.T) {
	p, _ := planner()
	sl := func(a string, w int, srcs ...Source) Sleeve { return Sleeve{a, w, srcs} }
	aave := Source{"lending", "aave", 10000, SourceParams{}}
	cases := []struct {
		s    Strategy
		want string
	}{
		{multiStrat, ""},
		{Strategy{Asset: Multi, Sleeves: multiStrat.Sleeves}, ""},
		{Strategy{Sleeves: []Sleeve{sl("USDC", 10000, aave)}}, "invalid_sleeves"},
		{Strategy{Sleeves: []Sleeve{sl("USDC", 5000, aave), sl("USDC", 5000, aave)}}, "invalid_sleeves"},
		{Strategy{Sleeves: []Sleeve{sl("USDC", 5000, aave), sl("WETH", 4000, aave)}}, "invalid_sleeves"},
		{Strategy{Asset: "USDC", Sleeves: []Sleeve{sl("USDC", 5000, aave), sl("WETH", 5000, aave)}}, "invalid_sleeves"},
		{Strategy{Sleeves: []Sleeve{sl("USDC", 5000, aave), sl("MON", 5000, aave)}}, "unsupported_source"},
	}
	for i, c := range cases {
		if got := code(p.Validate(c.s)); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
	if _, err := Split(multiStrat.Parts(), []int{1, 2, 3}); code(err) != "bad_request" {
		t.Errorf("split mismatch: %v", err)
	}
	if xs, _ := Split(multiStrat.Parts(), []int{1, 2, 3, 4}); len(xs) != 3 || len(xs[2]) != 2 || xs[2][1] != 4 {
		t.Errorf("split: %v", xs)
	}
}

func TestDeployMulti(t *testing.T) {
	p, _ := planner()
	ctx := context.Background()
	plan, err := p.Deploy(ctx, multiStrat, decimal.NewFromInt(1_000_000_000), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	// $1000: fee 4, net 996 → USDC 398.4, WETH 298.8, MON 298.8 (remainder); only the first basis sleeve creates the Perpl account
	golden(t, "multi deploy", plan, `{"steps":[
		{"action":"fee.transfer","amount":"4000000"},
		{"action":"lend.supply","venue":"aaveV3","asset":"USDC","amount":"398400000"},
		{"action":"kuru.marketBuy","market":"WETH","quote":"199200000","minOut":"49800000000000000"},
		{"action":"stable.swap","from":"USDC","to":"AUSD","amount":"99600000","minOut":"99550200"},
		{"action":"perpl.createAccount","amount":"99550200"},
		{"action":"perpl.order","perp":"ETH","kind":"openShort","price":"396000","lot":"49","leverageHdths":200},
		{"action":"kuru.marketBuy","market":"MON","quote":"224100000","minOut":"7470000000000000000000"},
		{"action":"wmon.wrap","amount":"4980000000000000000000"},
		{"action":"lend.supply","venue":"neverland","asset":"WMON","amount":"4980000000000000000000"},
		{"action":"stable.swap","from":"USDC","to":"AUSD","amount":"74700000","minOut":"74662650"},
		{"action":"perpl.deposit","amount":"74662650"},
		{"action":"perpl.order","perp":"MON","kind":"openShort","price":"29700","lot":"2490","leverageHdths":100}],
		"feeUsdc":"4000000","legs":[
		{"token":"USDC","kind":"lending","venue":"aave","asset":"398400000"},
		{"token":"WETH","kind":"basis","venue":"perpl","asset":"49800000000000000","collateral":"99550200","lot":"49","entryPx":"4000"},
		{"token":"MON","kind":"lending","venue":"neverland","asset":"4980000000000000000000"},
		{"token":"MON","kind":"basis","venue":"perpl","asset":"2490000000000000000000","collateral":"74662650","lot":"2490","entryPx":"0.03"}]}`)

	for name, c := range map[string]struct {
		amt     int64
		account bool
		want    string
	}{
		"perpl":   {90_000_000, false, "WETH part needs ≥ 100.46 USDC total deposit (Perpl min 10 AUSD collateral)"},
		"kuru":    {40_000_000, true, "WETH part needs ≥ 42.17 USDC total deposit (Kuru min 0.002 WETH)"},
		"enough":  {42_170_000, true, ""},
		"usdc ok": {1_000_000, false, ""},
	} {
		s := multiStrat
		if name == "usdc ok" {
			s = Strategy{Asset: "USDC", Sources: []Source{{"lending", "aave", 10000, SourceParams{}}}}
		}
		_, err := p.Deploy(ctx, s, decimal.NewFromInt(c.amt), c.account, 0)
		if e, _ := err.(*Error); (c.want == "") != (err == nil) || (e != nil && (e.Code != "below_min_size" || e.Detail != c.want)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWithdrawMulti(t *testing.T) {
	p, _ := planner()
	ctx := context.Background()
	dep, _ := p.Deploy(ctx, multiStrat, decimal.NewFromInt(1_000_000_000), false, 0)
	bals := make([]Balance, len(dep.Legs))
	for i, l := range dep.Legs {
		bals[i] = Balance{Value: l.Asset}
	}
	plan, err := p.WithdrawBps(ctx, multiStrat, dep.Legs, bals, 10_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "multi withdraw", plan.Steps, `[
		{"action":"lend.withdraw","venue":"aaveV3","asset":"USDC","amount":"398400000"},
		{"action":"perpl.order","perp":"ETH","kind":"closeShort","price":"404000","lot":"49","leverageHdths":100},
		{"action":"perpl.withdraw","amount":"98502580"},
		{"action":"stable.swap","from":"AUSD","to":"USDC","amount":"98502580","minOut":"98453328"},
		{"action":"kuru.marketSell","market":"WETH","size":"49800000000000000","minOut":"199200000"},
		{"action":"lend.withdraw","venue":"neverland","asset":"WMON","amount":"4980000000000000000000"},
		{"action":"wmon.unwrap","amount":"4980000000000000000000"},
		{"action":"perpl.order","perp":"MON","kind":"closeShort","price":"30300","lot":"2490","leverageHdths":100},
		{"action":"perpl.withdraw","amount":"74263378"},
		{"action":"stable.swap","from":"AUSD","to":"USDC","amount":"74263378","minOut":"74226246"},
		{"action":"kuru.marketSell","market":"MON","size":"7470000000000000000000","minOut":"224100000"}]`)
	if len(plan.Legs) != 4 || plan.Legs[1].Token != "WETH" || plan.Legs[1].Lot != "" || plan.Legs[3].Token != "MON" {
		t.Errorf("legs %+v", plan.Legs)
	}

	// resume after the ETH close landed: only the MON short is still open; each sell is capped by its own wallet token
	live := Live{Bals: []Balance{{Value: "0"}, {Value: "0"}, {Value: "4980000000000000000000"}, {Value: "0"}},
		Wallet:  map[string]decimal.Decimal{"USDC": d("398400000"), "WETH": d("40000000000000000"), "MON": d("2490000000000000000000"), "WMON": decimal.Zero, "AUSD": decimal.Zero},
		PerpLot: map[string]decimal.Decimal{"ETH": decimal.Zero, "MON": d("2490")}, PerpFree: d("98502410")}
	got, err := p.ResumeWithdraw(ctx, multiStrat, plan.Steps, 1, live, 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "multi resume", got, `[
		{"action":"perpl.withdraw","amount":"98502410"},
		{"action":"stable.swap","from":"AUSD","to":"USDC","amount":"98502410","minOut":"98453158"},
		{"action":"kuru.marketSell","market":"WETH","size":"40000000000000000","minOut":"160000000"},
		{"action":"lend.withdraw","venue":"neverland","asset":"WMON","amount":"4980000000000000000000"},
		{"action":"wmon.unwrap","amount":"4980000000000000000000"},
		{"action":"perpl.order","perp":"MON","kind":"closeShort","price":"30300","lot":"2490","leverageHdths":100},
		{"action":"perpl.withdraw","amount":"74263378"},
		{"action":"stable.swap","from":"AUSD","to":"USDC","amount":"74263378","minOut":"74226246"},
		{"action":"kuru.marketSell","market":"MON","size":"7470000000000000000000","minOut":"224100000"}]`)
}

// hold: bought with the rest of the sleeve, stays in the wallet (no venue step), sold on exit, moved on rebalance with no withdraw.
func TestHold(t *testing.T) {
	p, _ := planner()
	ctx := context.Background()
	s := Strategy{Asset: "MON", Sources: []Source{{"hold", "wallet", 5000, SourceParams{}}, {"lending", "neverland", 5000, SourceParams{}}}}
	if err := p.Validate(s); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(Strategy{Asset: "MON", Sources: []Source{{"hold", "aave", 10000, SourceParams{}}}}); code(err) != "unsupported_source" {
		t.Errorf("hold venue: %v", err)
	}
	plan, err := p.Deploy(ctx, s, decimal.NewFromInt(1_000_000_000), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, st := range plan.Steps {
		actions = append(actions, st.Action)
	}
	if got := strings.Join(actions, ","); got != "fee.transfer,kuru.marketBuy,wmon.wrap,lend.supply" {
		t.Errorf("deploy steps: %s", got)
	}
	if plan.Legs[0].Kind != "hold" || plan.Legs[0].Asset != plan.Legs[1].Asset || plan.Legs[0].Asset == "0" {
		t.Errorf("legs: %+v", plan.Legs)
	}
	h := plan.Legs[0].Asset
	bals := []Balance{{Value: h}, {Value: plan.Legs[1].Asset}}
	w, err := p.Withdraw(ctx, s, plan.Legs, bals, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := w.Steps[len(w.Steps)-1]
	if w.Steps[0].Action != "lend.withdraw" || last.Action != "kuru.marketSell" || last.Size != d(h).Mul(decimal.NewFromInt(2)).String() {
		t.Errorf("withdraw: %+v", w.Steps)
	}
	// all to lending: no withdraw step for hold, the freed tokens are supplied
	r, err := p.Rebalance(ctx, s, plan.Legs, bals, []int{0, 10000}, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Steps[0].Action != "wmon.wrap" || r.Legs[0].Asset != "0" {
		t.Errorf("rebalance: %+v %+v", r.Steps, r.Legs)
	}
}
