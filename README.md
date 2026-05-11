# Millennium Order Entry System

A production-grade, low-latency order entry and execution engine built in **Go**, deployed on **AWS**, with real broker integration via **FIX 4.2 (Interactive Brokers)** and **Alpaca Markets**.

Built as a project for Millennium — replicating the core infrastructure of an institutional order management system.

## Architecture

```
Browser (Web UI)
    │
    ▼
AWS ALB (Load Balancer)
    │
    ▼
ECS Fargate (Go OES — auto-scaling 2-10 tasks)
    │              │                    │
    ▼              ▼                    ▼
ElastiCache     DynamoDB          Broker Gateway
  Redis         (durable)        (IBKR FIX / Alpaca)
(hot state)     (audit trail)    (order execution)
```

## Key Features

- **Sub-15ms order-to-ACK latency** (same region as broker)
- **FIX 4.2 protocol** — the institutional standard since 1992
- **20+ order types** — Market, Limit, Stop, Bracket, OCO, OTO, TWAP, VWAP, Iceberg, Trailing Stop, MIT, LIT, Funari, and more
- **Pre-trade risk engine** — position limits, daily loss, fat-finger protection, kill switch
- **Real-time UI** — Server-Sent Events push every state change instantly
- **Dual broker support** — swap between Alpaca (paper) and IBKR (institutional) with one env var
- **Full AWS infrastructure** — Terraform for VPC, ECS, ElastiCache, DynamoDB, ALB
- **Two-tier storage** — Redis (hot path, <1ms) + DynamoDB (durable, async)

## Order Types Supported

| Category | Types |
|----------|-------|
| Basic | Market, Limit, Stop, Stop-Limit |
| Auction | Market-on-Open, Market-on-Close, Limit-on-Open, Limit-on-Close |
| Conditional | Trailing Stop, Market-if-Touched, Limit-if-Touched, Funari |
| Linked | Bracket, OCO (One-Cancels-Other), OTO (One-Triggers-Other), OTOCO |
| Algorithmic | TWAP, VWAP, Iceberg |

## Quick Start

```bash
# Clone
git clone https://github.com/gauri-sharmaa/order_entry_system.git
cd order_entry_system/millennium-oes

# Set broker credentials (Alpaca paper trading — free at https://app.alpaca.markets)
export ALPACA_API_KEY=your_key
export ALPACA_API_SECRET=your_secret

# Run locally (requires Docker for Redis, Go 1.22+)
./scripts/run-local.sh

# Open http://localhost:8080
```

### Switch to FIX / Interactive Brokers

```bash
export BROKER_TYPE=fix
export FIX_HOST=127.0.0.1
export FIX_PORT=4002          # IBKR Gateway paper trading port
export FIX_ACCOUNT=your_account
export FIX_SENDER_COMP_ID=MILLENNIUM
export FIX_TARGET_COMP_ID=IBFX
```

## Deploy to AWS

```bash
cd millennium-oes/terraform
terraform init
terraform plan
terraform apply

cd ..
./scripts/deploy.sh paper
```

## Project Structure

```
millennium-oes/
├── cmd/server/              # Application entrypoint
├── internal/
│   ├── api/                 # HTTP handlers, routing, middleware
│   ├── order/               # Order engine — lifecycle, linked orders, algos
│   ├── risk/                # Pre-trade risk checks, kill switch
│   ├── broker/              # Broker interface + Alpaca implementation
│   │   └── fix/             # FIX 4.2 client (QuickFIX/Go + IBKR)
│   └── store/               # Redis (hot) + DynamoDB (durable) storage
├── web/                     # Trading UI (HTML/CSS/JS)
├── terraform/               # AWS infrastructure (VPC, ECS, Redis, DynamoDB)
├── scripts/                 # Local dev + deployment scripts
├── Dockerfile               # Multi-stage build (~20MB final image)
├── EXPLAINER.md             # Non-technical full system explanation
└── README.md                # Technical documentation
```

## Tech Stack

| Component | Technology | Rationale |
|-----------|-----------|-----------|
| Language | Go 1.22 | Compiled, low GC pauses, goroutines |
| Hot State | Redis 7 (ElastiCache r7g) | Sub-ms reads, in-memory |
| Persistence | DynamoDB | Serverless, single-digit ms, TTL |
| Compute | ECS Fargate | Auto-scaling, no EC2 management |
| Load Balancer | AWS ALB | Sticky sessions for SSE |
| Broker (Retail) | Alpaca Markets | REST + WebSocket, paper trading |
| Broker (Institutional) | Interactive Brokers | FIX 4.2, <5ms latency |
| FIX Engine | QuickFIX/Go | Industry standard implementation |
| Frontend | Vanilla JS | No framework, SSE for real-time |
| Infrastructure | Terraform | Reproducible, version-controlled |

## Risk Controls

| Control | Default Limit |
|---------|--------------|
| Max order size | 10,000 shares |
| Max position value | $1,000,000 |
| Max daily loss | $50,000 |
| Price deviation | ±5% from market |
| Duplicate detection | 3 similar orders/minute |
| Kill switch | Instant halt of all trading |

## API Reference

```
POST   /api/orders              Submit order
GET    /api/orders              List active orders
GET    /api/orders/{id}         Get order by ID
DELETE /api/orders/{id}         Cancel order
PATCH  /api/orders/{id}         Modify order
DELETE /api/orders              Cancel all orders
GET    /api/quote/{symbol}      Get latest quote
GET    /api/positions           Get positions
GET    /api/account             Get account info
GET    /api/risk                Risk status + daily P&L
POST   /api/risk/killswitch     Toggle kill switch
GET    /api/stream              SSE real-time order updates
```

## Documentation

- **[EXPLAINER.md](millennium-oes/EXPLAINER.md)** — Full non-technical explanation of every component with diagrams
- **[millennium-oes/README.md](millennium-oes/README.md)** — Technical documentation with API examples
