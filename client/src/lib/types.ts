// Wire shapes (tasks/api-v2.md, market-data, history). Amounts on api = human decimal strings; quotes = raw atoms.

export type Level = [px: string, sz: string];

// GET /books/{orderbook} adds stale; WS book.<orderbook> omits it.
export type Book = { market: string; seq: number; ts: number; bids: Level[]; asks: Level[]; stale?: boolean };

export type Trade = { market: string; ts: number; px: string; sz: string; side: "buy" | "sell" };

export type Interval = "1m" | "5m" | "1h" | "1d";
export type Candle = { market: string; interval: Interval; t: number; o: string; h: string; l: string; c: string; v: string; q: string; n: number };

// market-data GET /quote/{orderbook}: raw atoms (buy in USDC, out base; sell reverse)
export type Quote = {
  market: string;
  side: "buy" | "sell";
  amount: string;
  fillable: string;
  partial: boolean;
  expectedOut: string;
  minOut: string;
  orderSize: string;
  avgPx: string;
  bestPx: string;
  impactBps: number;
  maxSafeAmount: string;
  maxImpactBps: number;
};

type RateInfo = { source: string; venue: string; asset: string; address: string; apy: string; kind: string; ts: number };
type FundingInfo = { perpId: number; name: string; rate: string; intervalSec: number; aprShort: string; aprShort7d: string; block: number; ts: number };
export type Rates = { ts: number; rates: RateInfo[]; funding: FundingInfo[]; stale: boolean };

export type SourceKind = "hold" | "lending" | "staking" | "lp" | "basis";
export type Source = { kind: SourceKind; venue: string; weightBps: number; params?: { leverage?: string } };
// one token's part of a multi-token strategy; old rows have none (asset + sources = one sleeve)
export type Sleeve = { asset: string; weightBps: number; sources: Source[] };
export type Rules = { driftBps?: number; minRebalanceSeconds?: number; fundingGate?: { apr: string; lookbackDays: number } | null };

export type Strategy = {
  id: string;
  creator: string;
  creatorWallet: string | null;
  name: string;
  asset: string; // "MULTI" when sleeves is set
  sources: Source[]; // [] when sleeves is set
  sleeves?: Sleeve[] | null;
  rules: Rules;
  public: boolean;
  createdAt: string;
  backtest: {
    from: string;
    to: string;
    days: number;
    apyUsd: string;
    apyAsset: string;
    maxDrawdownUsd: string;
    finalUsd: string;
    hodlApyUsd: string;
    ranAt: string;
  } | null;
  apy: { estimate: string; sources: { kind: string; venue: string; asset?: string; apy: string | null }[]; stale: boolean } | null;
};

export type StrategyInput = { name: string; rules?: Rules; public?: boolean } & ({ asset: string; sources: Source[] } | { sleeves: Sleeve[] });

export type Delegation = {
  id: string;
  walletId: string;
  walletAddress: string;
  keyQuorumId: string;
  active: boolean;
  createdAt: string;
  revokedAt: string | null;
};

export type Me = { id: string; walletAddress: string | null; createdAt: string; delegation: Delegation | null };

type ExecutionStatus = "pending" | "submitted" | "confirmed" | "failed" | "rejected";

export type Step = { action: string; market?: string; token?: string; amount?: string; side?: string; venue?: string; [k: string]: unknown };

export type Execution = {
  id: string;
  kind: string;
  signer?: "executor" | "client";
  status: ExecutionStatus;
  stepDone: number;
  stepsTotal: number;
  steps: Step[];
  txHashes: string[];
  txHash: string | null;
  error: string | null;
  scheduleId: string | null;
  positionId: string | null;
  createdAt: string;
  updatedAt: string;
};

// 202 body of every execution-creating route
export type Accepted = { executionId: string; kind: string; steps: Step[]; orderId?: string; positionId?: string; feeUsdc?: string };

export type Earnings = {
  owedUsdc: string;
  paidUsdc: string;
  feesPaidUsdc: string;
  rows: { executionId: string; strategyId: string; feeUsdc: string; creatorOwedUsdc: string; paid: boolean; createdAt: string }[];
};

export type Holdings = {
  wallet: string;
  tokens: { symbol: string; address: string; raw: string; amount: string }[];
};

export type Order = {
  id: string;
  market: string;
  side: "buy" | "sell";
  price: string;
  size: string;
  kuruOrderId: string | null;
  status: "pending" | "open" | "cancelled" | "failed";
  executionId: string;
  createdAt: string;
};

// token: sleeve asset; absent = position asset (single-token rows)
type LegState = { token?: string; kind: string; venue: string; asset: string; collateral?: string; lot?: string; entryPx?: string };

export type Position = {
  id: string;
  strategyId: string;
  strategyName: string;
  asset: string;
  wallet: string;
  principalUsdc: string;
  status: "opening" | "open" | "withdrawing" | "closed" | "failed";
  legs: LegState[];
  openedAt: string;
  closedAt: string | null;
  lastRebalanceAt: string | null;
};

export type ScheduleKind = "dca" | "take_profit" | "stop_loss" | "rebalance";
export type Target = { type: "market" | "position" | "strategy"; id: string };

export type ScheduleInput = {
  kind: ScheduleKind;
  target: Target;
  amountUsdc?: string;
  intervalSeconds?: number;
  triggerPrice?: string;
  sellBps?: number;
  driftBps?: number;
  maxSlippageBps: number;
  maxPerRunUsdc: string;
};

export type Schedule = {
  id: string;
  kind: ScheduleKind | "recurring";
  target: Target | null;
  amountUsdc: string | null;
  intervalSeconds: number | null;
  nextRunAt: string | null;
  triggerPrice: string | null;
  sellBps: number | null;
  driftBps: number | null;
  maxSlippageBps: number;
  maxPerRunUsdc: string;
  active: boolean;
  createdAt: string;
};

// history POST /backtest
type BacktestPoint = { day: string; usd: string; asset: string; hodlUsd: string; basisActive: boolean };
export type Backtest = {
  label: string;
  asset: string;
  amount: string;
  from: string;
  to: string;
  days: number;
  estimated: boolean;
  coverage: { series: string; first: string; last: string; days: number; note?: string }[];
  initialUsd: string;
  finalUsd: string;
  apyUsd: string;
  apyAsset: string;
  maxDrawdownUsd: string;
  hodl: { finalUsd: string; apyUsd: string; maxDrawdownUsd: string };
  rebalances: number;
  costsUsd: string;
  contributions: Record<string, string>;
  curve: BacktestPoint[];
  notes: string[];
};

// POST /me/trade/prepare
export type PrepareInput = { kind: "trade"; market: string; side: "buy" | "sell"; amount: string; slippageBps?: number };
export type TxRequest = { to: string; data: string; value: string; chainId: number; description: string; step: number };
export type Prepared = { prepareId: string; kind: string; steps: Step[]; txs: TxRequest[]; expiresAt: string };

// Perpl risk inputs (api /perpl/markets): contract state + Perpl API funding. Fractions: 0.05 = 5%; APR positive = shorts receive.
export type PerplMarket = {
  asset: string;
  perpId: number;
  name: string;
  mark: string;
  oracle: string;
  longOi: string;
  shortOi: string;
  initMargin: string;
  maintMargin: string;
  fundingApr?: string;
  fundingApr7d?: string;
};
export type PerplMarkets = { guardBuffer: string; gateBand: string; markets: PerplMarket[] };

// one basis sleeve of a position, evaluated as the scheduler does (api /me/positions/{id}/perpl); money in USD
export type PerplRisk = {
  asset: string;
  perpId: number;
  name: string;
  account: number;
  open: boolean;
  size?: string;
  entry?: string;
  mark?: string;
  markValid: boolean;
  collateral?: string;
  pnl?: string;
  funding?: string;
  equity?: string;
  notional?: string;
  free: string;
  marginFrac?: string;
  maintMargin?: string;
  guardAt?: string;
  liqPrice?: string;
  guardPrice?: string;
  guard: { fire: boolean; closeAll: boolean; topUp: string; lotCut: string };
  gate: { apr: string; threshold: string; band: string; lookbackDays: number; want: boolean } | null;
  next: "none" | "guard_top_up" | "guard_cut" | "guard_close" | "gate_close" | "gate_open";
};
export type FundingDay = { day: string; rate: string; apr: string; intervals: number };
