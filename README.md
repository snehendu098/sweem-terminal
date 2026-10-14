# sweem

**Non custodial trading and yield baskets on Monad.**

sweem is a trading terminal and yield platform on Monad mainnet (chain 143). Users trade Kuru order books, build and buy yield baskets, and automate DCA, take profit, stop loss and rebalancing. Every position lives in the user's own Privy wallet. sweem never takes custody: a delegated signer, restricted by a Privy policy allowlist, runs planned steps on the user's behalf, and every step is rechecked against a fixed list of deployed contracts before it is signed.

## Architecture

sweem splits work across services that each own one concern:

| Service | Language | Port | Role |
|---------|----------|------|------|
| **publisher** | Go | 8081 | Sole writer to Redis. Streams Kuru books, trades, reference prices and candles from the Kuru websocket, polls lending and staking rates and Perpl funding every minute, writes a liveness heartbeat |
| **market-data** | Go | 8082 | Read only API over the publisher state: books, quotes, candles, rates, plus a WebSocket fan out hub |
| **api** | Go | 8080 | Privy authenticated user API: delegation, trades, limit orders, baskets, deploy and withdraw, schedules, earnings, Perpl risk |
| **scheduler** | Go | 8084 | Fires DCA, recurring basket buys and TP/SL; evaluates open positions for the Perpl liquidation guard, funding gate, rate switch and drift rebalance |
| **executor** | Rust | 8083 | The only signer. Rechecks every plan step, enforces a per step stablecoin cap, approves exact amounts and sends through the Privy wallet API |
| **indexer** | Rust | 8085 | Indexes user wallet events from HyperSync: Kuru trades and orders, venue deposits and withdrawals, Perpl accounts and positions |
| **history** | Go | 8087 | Ingests historical rates, funding and prices, and runs strategy backtests |
| **client** | TypeScript | 3000 | Next.js web app |

The executor is reachable only on the internal compose network and accepts requests only from the api and scheduler, authenticated with a shared internal secret. The api and scheduler decide what to do; the executor decides nothing and refuses anything outside the allowlist.

## Key Mechanisms

**Baskets.** A basket splits a USDC deposit across up to five tokens (USDC, WETH, cbBTC, MON, XAUt0), and each token across up to six yield sources: lending (Aave V3, Neverland, Euler and Morpho ERC4626 vaults), staking (shMonad), liquidity (Kuru ActiveVault) and funding basis. Baskets are public or private, can be bought by anyone, and run in the buyer's own wallet.

**Funding Basis.** A basis source holds the token as spot on Kuru and opens a Perpl short of equal size, collateralised with AUSD bought through the AUSD 3pool. The position is delta neutral and earns short side funding. Capital is split L/(L+1) to spot and 1/(L+1) to collateral, where L is the chosen leverage.

**Liquidation Guard.** Perpl margin is isolated. When a short's margin fraction falls within a buffer of maintenance, the scheduler moves free account balance into the position, then tops up from the wallet's USDC, then trims the short, and only as a last resort closes the hedge and parks the value in the best lending venue.

**Funding Gate.** A basket may require a minimum trailing short funding APR. While funding sits below it, the basis weight is parked in the highest yielding lending venue; it reopens once funding clears the threshold plus a hysteresis band.

**Rebalancing.** Positions drift as prices and yields move. The scheduler rebalances back to target weights once drift exceeds the basket's band, no more often than its minimum interval. An optional rate switch moves lending weight to a venue that pays more by a set margin.

**Resumable Withdrawals.** A withdrawal that fails partway stays in a withdrawing state. Retrying rebuilds the unfinished steps from live on chain balances, so a retry never closes a short twice or withdraws the same funds twice.

**Two Signing Paths.** Market trades are signed by the user directly: the api prepares unsigned transactions, the client signs them in the Privy embedded wallet, and the api verifies every hash on chain before recording the execution. Baskets, limit orders and all automation run through the delegated signer.

## Fees

| Event | Fee |
|-------|-----|
| Basket buy | 0.4% of the deposit, paid to the treasury in the first step |
| Creator share | Half of the open fee is owed to the basket creator when someone else buys |
| Withdraw | None |

Creator shares accrue in `fee_ledger` and are paid out in USDC from the treasury with the payout command.

## Repository Structure

```
sweem-terminal/
  client/                 Next.js web app
  services/
    api/                  User API and the creator payout command
    scheduler/            Automation and position evaluation
    publisher/            Kuru feed, rates and funding poller
    market-data/          Read API, quotes and WebSocket hub
    history/              Historical ingest and backtest engine
    executor/             Rust signer (Privy wallet API)
    indexer/              Rust HyperSync indexer
  internal/
    planner/              Intents to executor steps, transaction encoding, balances
    perpl/                Perpl contract reader, guard and gate rules
    streams/              Redis stream contract shared by every service
    config/ httpx/ pg/    Shared configuration, HTTP and Postgres helpers
  migrations/             Postgres schema, applied in name order
  deployments/143.json    Every contract address the system may touch
  testdata/contracts/     Golden fixtures shared by the Go planner and Rust executor
  e2e/                    Real money mainnet end to end test
  infra/                  Terraform (Vultr), Ansible and Caddy
  scripts/                Address verification, e2e funding and fund return
```

## Tech Stack

| Component | Technology |
|-----------|------------|
| Chain | Monad mainnet (143) |
| Backend services | Go 1.25, pgx, go-redis, go-ethereum, coder/websocket |
| Signer and indexer | Rust, axum, alloy, tokio-postgres, hypersync-client |
| Storage | PostgreSQL 16, Redis 7 |
| Wallets and auth | Privy embedded wallets, delegated signers, policies, ES256 JWT |
| Frontend | Next.js 16, React 19, Tailwind CSS 4, viem, lightweight-charts, Recharts, React Flow |
| Cross chain deposits | Aurora Intents (NEAR Intents 1Click) |
| Historical data | DefiLlama, Kintsu, Perpl API, Envio HyperSync |
| Infrastructure | Docker Compose, GHCR, Terraform, Ansible, Caddy |

## Integrations

| Protocol | Use |
|----------|-----|
| Kuru | Order books for spot trades, limit orders, margin account, ActiveVault liquidity |
| Perpl | Perpetual shorts for funding basis (BTC, ETH, MON) |
| Aave V3, Neverland | Lending pools |
| Euler, Morpho | ERC4626 lending vaults |
| shMonad | MON liquid staking |
| AUSD 3pool | AUSD, USDC and USDT0 swaps for Perpl collateral |

All addresses live in `deployments/143.json`. Run `scripts/verify-143.sh` to confirm each one has code on chain.

## Quick Start

### Prerequisites

| Tool | Version |
|------|---------|
| Docker with Compose | v2 |
| Go | 1.25 |
| Rust | stable |
| Bun | 1.3 |

You also need a Privy app with a key quorum and policy, and an Envio API token for HyperSync.

### Configure

```bash
cp .env.example .env
cp .env.example .env.executor   # signer secrets: PRIVY_APP_SECRET, PRIVY_AUTHORIZATION_PRIVATE_KEY, MAX_STEP_USD
cp .env.example .env.api        # PRIVY_APP_SECRET, MAX_STEP_USD
```

`INTERNAL_SECRET` must match across `.env` and `.env.executor`, and `MAX_STEP_USD` must match between the api and executor.

### Run the Stack

```bash
make up      # build and start every service, apply migrations
make logs    # follow logs
make down    # stop
```

Verify with `curl http://localhost:8080/healthz`. Each service exposes `/health` for liveness and `/healthz` for its dependencies.

### Push the Privy Policy

```bash
cd services/executor
cargo run -- sync-policy --dry-run   # print the rules
cargo run -- sync-policy             # create or update the policy
```

### Client

```bash
cd client
bun install
bun run dev
```

Create `client/.env.local`:

| Variable | Purpose |
|----------|---------|
| `NEXT_PUBLIC_PRIVY_APP_ID` | Privy app id |
| `NEXT_PUBLIC_PRIVY_SIGNER_ID` | Key quorum added as the delegated signer |
| `NEXT_PUBLIC_PRIVY_POLICY_ID` | Policy attached to the signer |
| `NEXT_PUBLIC_API_URL` | api base URL, default `http://localhost:8080` |
| `NEXT_PUBLIC_MARKET_DATA_URL` | market-data base URL, default `http://localhost:8082` |
| `NEXT_PUBLIC_MARKET_DATA_WS_URL` | market-data WebSocket, default `ws://localhost:8082/api/v1/ws` |
| `NEXT_PUBLIC_HISTORY_URL` | history base URL, default `http://localhost:8087` |
| `NEXT_PUBLIC_RPC_URL` | Monad RPC, default `https://rpc.monad.xyz` |
| `NEXT_PUBLIC_AURORA_APP_KEY` | Enables deposits from other chains |

After changing `deployments/143.json`, run `make client-abi` to copy it into the client.

## Pages

| Route | What It Shows |
|-------|---------------|
| `/` | Token markets with live prices, 24h change and volume, plus basket search |
| `/trade/[symbol]` | Chart, order book, market and limit orders, DCA and TP/SL |
| `/baskets` | Every public basket with live APY, backtest APY and drawdown |
| `/baskets/new` | Basket builder with routing diagram, risk card and backtest |
| `/baskets/[id]` | Basket index chart, allocation, backtest, buy and recurring buy |
| `/portfolio` | Holdings, positions with partial sells, trading balance, earnings, automation |
| `/risk` | Perpl hedge markets and the live state of every hedge the user holds |

## API Endpoints

### Public

| Method | Path | Description |
|--------|------|-------------|
| GET | [`/api/v1/markets`](services/api/internal/api/trade.go#L26) | Tokens and Kuru markets |
| GET | [`/api/v1/strategies`](services/api/internal/api/earn.go#L114) | Public baskets with live APY |
| GET | [`/api/v1/strategies/{id}`](services/api/internal/api/earn.go#L130) | One basket (private ones for their creator) |
| POST | [`/api/v1/strategies/{id}/backtest`](services/api/internal/api/earn.go#L262) | Backtest a basket |
| GET | [`/api/v1/perpl/markets`](services/api/internal/api/perpl.go#L51) | Perpl mark, open interest, margins and funding |

### User (Privy bearer token)

| Method | Path | Description |
|--------|------|-------------|
| GET | [`/api/v1/me`](services/api/internal/api/api.go#L240) | Profile and delegation |
| POST, DELETE | [`/api/v1/me/delegation`](services/api/internal/api/api.go#L245) | Enable or revoke the delegated signer |
| POST | [`/api/v1/me/trade/prepare`](services/api/internal/api/trade.go#L109) | Unsigned transactions for a market trade |
| POST | [`/api/v1/me/trade/record`](services/api/internal/api/trade.go#L146) | Verify signed hashes and record the trade |
| POST | [`/api/v1/me/trade`](services/api/internal/api/trade.go#L88) | Market trade through the delegated signer |
| POST | [`/api/v1/me/margin/deposit`](services/api/internal/api/trade.go#L229) | Fund the Kuru margin account |
| POST | [`/api/v1/me/margin/withdraw`](services/api/internal/api/trade.go#L229) | Withdraw from the Kuru margin account |
| GET, POST | [`/api/v1/me/orders`](services/api/internal/api/trade.go#L258) | List or place limit orders |
| DELETE | [`/api/v1/me/orders/{id}`](services/api/internal/api/trade.go#L308) | Cancel a limit order |
| GET | [`/api/v1/me/holdings`](services/api/internal/api/trade.go#L341) | Wallet balances |
| POST | [`/api/v1/strategies`](services/api/internal/api/earn.go#L148) | Create a basket |
| GET | [`/api/v1/me/strategies`](services/api/internal/api/earn.go#L122) | Baskets the user created |
| POST | [`/api/v1/me/strategies/{id}/deploy`](services/api/internal/api/earn.go#L284) | Buy a basket |
| GET | [`/api/v1/me/positions`](services/api/internal/api/earn.go#L346) | Basket positions |
| POST | [`/api/v1/me/positions/{id}/withdraw`](services/api/internal/api/earn.go#L353) | Sell all or part of a position, or resume a failed sell |
| GET | [`/api/v1/me/positions/{id}/perpl`](services/api/internal/api/perpl.go#L117) | Hedge risk as the scheduler sees it |
| GET | [`/api/v1/me/executions`](services/api/internal/api/api.go#L275) | Execution history |
| GET | [`/api/v1/me/executions/{id}`](services/api/internal/api/api.go#L294) | One execution with step progress |
| GET, POST | [`/api/v1/me/schedules`](services/api/internal/api/api.go#L306) | List or create automations |
| DELETE | [`/api/v1/me/schedules/{id}`](services/api/internal/api/api.go#L327) | Deactivate an automation |
| GET | [`/api/v1/me/earnings`](services/api/internal/api/api.go#L289) | Creator earnings and fees paid |

### Market Data

| Method | Path | Description |
|--------|------|-------------|
| GET | [`/api/v1/books/{orderbook}`](services/market-data/internal/routes/routes.go#L103) | Top 20 book snapshot |
| GET | [`/api/v1/quote/{orderbook}`](services/market-data/internal/routes/routes.go#L166) | Market order quote with minimum out and max safe size |
| GET | [`/api/v1/candles/{orderbook}`](services/market-data/internal/routes/routes.go#L197) | OHLCV candles (1m, 5m, 1h, 1d) |
| GET | [`/api/v1/rates`](services/market-data/internal/routes/routes.go#L117) | Current venue yields and Perpl funding |
| GET | [`/api/v1/ws`](services/market-data/internal/hub/hub.go#L143) | WebSocket topics `book.*`, `trades.*`, `candles.*`, `rates` |

### History

| Method | Path | Description |
|--------|------|-------------|
| GET | [`/api/v1/series/rates`](services/history/internal/routes/routes.go#L78) | Daily lending and staking APY |
| GET | [`/api/v1/series/funding`](services/history/internal/routes/routes.go#L95) | Daily Perpl funding |
| GET | [`/api/v1/series/prices`](services/history/internal/routes/routes.go#L116) | Daily Kuru prices |
| POST | [`/api/v1/backtest`](services/history/internal/routes/routes.go#L271) | Simulate a single or multi token basket |

## Automation

| Kind | Target | Behaviour |
|------|--------|-----------|
| DCA | Market | Buys a fixed USDC amount every interval |
| Recurring buy | Basket | Opens a new basket position every interval |
| Take profit | Market or position | Sells a share once price or value reaches the trigger |
| Stop loss | Market or position | Sells a share once price or value falls to the trigger |
| Rebalance | Position | Overrides the basket's drift band and check cadence |

Schedules fire at most once per slot across scheduler instances, act only on fresh reference prices and rates, and stop entirely when the user revokes delegation.

## Safety Model

| Layer | Guarantee |
|-------|-----------|
| Privy policy | User wallets may only call allowlisted venue methods on chain 143, approve only venue spenders, and send USDC only to the treasury |
| Executor recheck | Every step must build against `deployments/143.json` with the user's wallet as receiver, within 32 steps and the per step stablecoin cap |
| Delegation | Revocable at any time; revoking deactivates every schedule |
| Quotes | Every swap carries a minimum out from a fresh quote; trades above the book's safe size are refused |
| Client signing | Recorded trades are verified hash by hash against the prepared transactions |

## Testing

```bash
make test               # Go unit tests and Rust tests
make infra              # Postgres and Redis for integration tests
make test-integration   # Go integration tests
cd client && bun run check && bun run lint
```

The real money end to end test spends mainnet funds and is guarded by an explicit flag:

```bash
E2E_DRY=1 make e2e          # no spend checks
E2E_REAL_MONEY=1 make e2e   # full run against `make up`
```

## Deployment

Production runs on a single Vultr instance. GitHub Actions builds one image per service and pushes it to GHCR on every push to `main`.

```bash
make provision   # Terraform: instance, firewall, SSH key
make deploy      # Ansible: ship compose files, env and migrations, pull images, restart
make payout      # dry run creator payouts; ARGS=--send to pay
```

Caddy terminates TLS on `api.$DOMAIN` and routes `/` to the api, `/data` to market-data and `/history` to history. The client deploys to Vercel.

## License

Apache License 2.0
