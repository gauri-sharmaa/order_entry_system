# Millennium OES — Complete Explainer

## What Is This Project?

Imagine you work at a large investment firm and you want to buy 10,000 shares of Apple stock. You can't just click "buy" on a retail app — you need a system that:

- Checks whether that trade is safe and within your firm's rules
- Sends the order to a real stock exchange or broker in milliseconds
- Tracks whether it got filled (executed), partially filled, or rejected
- Updates your screen in real time
- Keeps a permanent record of everything

That's exactly what **Millennium OES** (Order Entry System) does. It's a professional-grade trading platform — the kind of software used inside hedge funds and investment banks — built for speed, safety, and scale.

---

## The Big Picture

```
┌─────────────────────────────────────────────────────────────────┐
│   YOU (the trader) sitting at your desk, looking at a web page  │
└──────────────────────────┬──────────────────────────────────────┘
                           │ you click "BUY"
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│   THE INTERNET — your request travels to Amazon's cloud servers │
└──────────────────────────┬──────────────────────────────────────┘
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│   THE OES SERVER (the brain)                                    │
│   1. Is this order safe? (Risk Engine)                          │
│   2. What type of order is it? (Order Engine)                   │
│   3. Send it to the broker (Broker Client)                      │
│   4. Save a record (Redis + DynamoDB)                           │
└──────────────────────────┬──────────────────────────────────────┘
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│   THE BROKER (Alpaca or Interactive Brokers)                    │
│   sends your order to the actual stock exchange                 │
└──────────────────────────┬──────────────────────────────────────┘
                           │ "Order filled at $175.23"
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│   YOUR SCREEN UPDATES INSTANTLY                                 │
│   the row in the order table flashes green                      │
└─────────────────────────────────────────────────────────────────┘
```

The whole journey — from clicking "buy" to seeing "filled" — takes about **10-15 milliseconds**. That's 60x faster than a blink of your eye.

---

## The Files — What Each One Does

Think of the project like a building. Each folder is a floor, each file is a room with a specific job.

```
millennium-oes/
│
├── cmd/server/          ← The front door (starts everything)
├── internal/
│   ├── api/             ← The reception desk (handles requests)
│   ├── order/           ← The trading floor (core logic)
│   ├── risk/            ← The compliance department
│   ├── broker/          ← The phone to the exchange
│   └── store/           ← The filing cabinets
├── web/                 ← The screen you look at
├── terraform/           ← The building blueprints (cloud setup)
└── scripts/             ← The maintenance crew
```

---

### `cmd/server/main.go` — The Front Door

This is the file that starts the entire system. When you turn on the server, this file runs first. It's like the opening checklist a pilot runs before takeoff — connect to the database, connect to the broker, start listening for requests, and be ready to shut down cleanly if needed.

---

### `internal/api/` — The Reception Desk

Handles all communication between the outside world (your browser) and the internal system.

| File | Role |
|------|------|
| `router.go` | Maps web addresses to the right handler |
| `handlers.go` | Processes each type of request (submit order, cancel, etc.) |
| `middleware.go` | Logs every request and handles security headers |

---

### `internal/order/` — The Trading Floor

The heart of the system.

**`types.go`** defines every concept:

| Order Type | Plain English |
|-----------|---------------|
| Market | "Buy now at whatever price is available" |
| Limit | "Buy, but only if price is $175 or lower" |
| Stop | "If price drops to $170, sell immediately" |
| Trailing Stop | "Sell if price drops 5% from its peak" |
| Bracket | "Buy at $175, auto-sell at $185 profit or $165 loss" |
| OCO | "Place two orders — whichever fills first, cancel the other" |
| TWAP | "Spread my buy over 2 hours so I don't move the market" |
| Iceberg | "Show only 100 shares, but I'm actually buying 10,000" |

**`engine.go`** manages the entire life of every order:

```
NEW → PENDING → ACKNOWLEDGED → PARTIALLY FILLED → FILLED
                             → CANCELLED
                             → REJECTED
```

---

### `internal/risk/` — The Compliance Department

Before any order reaches the broker, it must pass these checks:

| Check | What it prevents |
|-------|-----------------|
| Order Size | Buying more than 10,000 shares at once |
| Position Value | Any single position exceeding $1,000,000 |
| Price Reasonability | Limit price more than 5% from market (fat-finger) |
| Daily Loss | Total losses exceeding $50,000 in one day |
| Duplicate Detection | Accidental double-clicks |
| Kill Switch | Emergency halt of all trading |

---

### `internal/broker/` — The Phone to the Exchange

Two broker options:

| Broker | Protocol | Latency | Use Case |
|--------|----------|---------|----------|
| Alpaca | REST + WebSocket | 5-50ms | Development, paper trading |
| Interactive Brokers | FIX 4.2 (TCP) | 1-5ms | Production, institutional |

FIX (Financial Information eXchange) is the industry standard protocol used by every major bank and hedge fund since 1992.

---

### `internal/store/` — The Filing Cabinets

| Store | Speed | Purpose |
|-------|-------|---------|
| Redis | <1ms | Active order state (fast, temporary) |
| DynamoDB | ~5ms | Permanent record (durable, searchable) |

DynamoDB writes happen in the background so they never slow down order processing.

---

### `web/` — The Trading Screen

A dark terminal-style interface with three panels:
- **Left**: Order entry form (symbol, side, type, quantity, price)
- **Center**: Order blotter (live table of all orders)
- **Right**: Current positions (what you own)

Updates in real time via Server-Sent Events — no page refresh needed.

---

### `terraform/` — The Cloud Blueprints

Infrastructure as Code — describes exactly what AWS resources to build:

| File | What it builds |
|------|---------------|
| `vpc.tf` | Private network (walls of the building) |
| `ecs.tf` | Go servers (2-10 auto-scaling copies) |
| `alb.tf` | Load balancer (routes traffic) |
| `redis.tf` | Fast memory cache |
| `dynamodb.tf` | Permanent database |

---

## The Full Order Journey

```
Trader clicks "BUY 100 AAPL @ $175"
        │
        ▼
API Handler reads the request, fetches current price ($174.50)
        │
        ▼
Risk Engine checks:
  ✓ 100 shares < 10,000 max
  ✓ $17,500 < $1,000,000 position limit
  ✓ $175 within 5% of $174.50
  ✓ Daily loss within limits
  ✓ Not a duplicate
  ✓ Kill switch OFF
  → APPROVED
        │
        ▼
Order Engine creates order, saves to Redis + DynamoDB
        │
        ▼
Broker Client sends to exchange (FIX or REST)
        │
        ▼  ~10ms later...
Fill event arrives: "100 shares @ $174.98"
        │
        ▼
Order Engine updates status → FILLED
        │
        ▼
SSE pushes update to browser → row flashes green
```

---

## Why Is It Built This Way?

### Speed
- Go language (compiled, no interpreter)
- Redis for active orders (RAM, not disk)
- DynamoDB writes are async (don't block orders)
- Persistent broker connections (no reconnect overhead)
- FIX protocol (binary, no HTTP overhead)

### Safety
- Risk Engine checks every order
- Kill Switch for emergencies
- Duplicate detection
- AWS VPC (servers hidden from internet)
- Secrets stored in AWS SSM (not in code)

### Scale
- Auto-scales from 2 to 10 server copies based on load
- Load balancer distributes traffic
- Redis handles millions of operations per second
- DynamoDB scales infinitely with no capacity planning

---

## Tech Stack

| Layer | Technology | Why |
|-------|-----------|-----|
| Language | Go 1.22 | Fast, compiled, great concurrency |
| Hot Cache | Redis 7 | Sub-millisecond, in-memory |
| Database | DynamoDB | Serverless, auto-scaling |
| Compute | ECS Fargate | No server management |
| Broker 1 | Alpaca | Free paper trading |
| Broker 2 | IBKR (FIX) | Institutional, low latency |
| Frontend | Vanilla JS | Lightweight, no framework overhead |
| Infra | Terraform | Reproducible cloud setup |
| Container | Docker | Consistent deployments |
