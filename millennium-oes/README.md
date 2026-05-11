# Millennium OES — Order Entry System

A production-grade, low-latency order entry and execution engine built in Go, deployed on AWS, integrated with Alpaca Markets.

## Architecture

```
Client (Browser)
    │
    ▼
AWS ALB (Application Load Balancer)
    │
    ▼
ECS Fargate (Go OES — 2+ tasks, auto-scaling)
    │         │                    │
    ▼         ▼                    ▼
ElastiCache  DynamoDB          Alpaca API
  Redis      (durable)     (broker / fills)
(hot state)                  WebSocket
```

**Latency profile:**
- Order submission → broker ACK: ~5–15ms (same AWS region as Alpaca)
- Redis read/write: <1ms
- DynamoDB write: async (not on hot path)

## Order Types Supported

| Category | Types |
|----------|-------|
| Basic | Market, Limit, Stop, Stop-Limit |
| Auction | MOO, MOC, LOO, LOC |
| Conditional | Trailing Stop, MIT, LIT, Funari |
| Linked | Bracket, OCO, OTO, OTOCO |
| Algorithmic | TWAP, VWAP, Iceberg |

## Quick Start (Local)

```bash
# 1. Get Alpaca paper trading keys at https://app.alpaca.markets
export ALPACA_API_KEY=your_key
export ALPACA_API_SECRET=your_secret

# 2. Run (requires Docker for Redis, Go 1.22+)
./scripts/run-local.sh

# 3. Open http://localhost:8080
```

## Deploy to AWS

```bash
# 1. Configure Terraform variables
cp terraform/terraform.tfvars.example terraform/terraform.tfvars
# Edit terraform.tfvars with your Alpaca keys

# 2. Initialize and apply infrastructure
cd terraform
terraform init
terraform plan
terraform apply

# 3. Build and deploy the container
cd ..
./scripts/deploy.sh paper
```

## API Reference

### Submit Order
```
POST /api/orders
Content-Type: application/json

{
  "symbol": "AAPL",
  "side": "buy",
  "type": "LIMIT",
  "qty": 100,
  "limit_price": 175.00,
  "time_in_force": "day"
}
```

### Order Types & Required Fields

| Type | Required Fields |
|------|----------------|
| MARKET | symbol, side, qty |
| LIMIT | symbol, side, qty, limit_price |
| STOP | symbol, side, qty, stop_price |
| STOP_LIMIT | symbol, side, qty, stop_price, limit_price |
| TRAILING_STOP | symbol, side, qty, trail_type, trail_value |
| BRACKET | symbol, side, qty, limit_price, take_profit_price, stop_loss_price |
| OCO | symbol, side, qty, limit_price, oco_pair.stop_price |
| OTO | symbol, side, qty, oto_secondary (full order object) |
| TWAP | symbol, side, qty, algo_params.end_time |
| ICEBERG | symbol, side, qty, limit_price, visible_qty |
| MIT/LIT | symbol, side, qty, touch_price |

### Other Endpoints

```
GET    /api/orders              # List active orders
GET    /api/orders/{id}         # Get order by ID
DELETE /api/orders/{id}         # Cancel order
PATCH  /api/orders/{id}         # Modify order
DELETE /api/orders              # Cancel all orders
GET    /api/quote/{symbol}      # Get latest quote
GET    /api/positions           # Get positions
GET    /api/account             # Get account info
GET    /api/risk                # Risk status
POST   /api/risk/killswitch     # Toggle kill switch
GET    /api/stream              # SSE order updates
```

## Risk Controls

Pre-trade checks run on every order:
- Max order size (default: 10,000 shares)
- Max position value (default: $1,000,000)
- Max daily loss (default: $50,000)
- Price reasonability (default: ±5% from market)
- Duplicate order detection
- Kill switch (halts all trading instantly)

## Tech Stack

| Layer | Technology | Why |
|-------|-----------|-----|
| Language | Go 1.22 | Compiled, low GC pauses, goroutines |
| Hot state | Redis 7 (ElastiCache r7g) | Sub-ms reads, in-memory |
| Persistence | DynamoDB | Serverless, single-digit ms, TTL |
| Compute | ECS Fargate | No EC2 management, auto-scaling |
| Load balancer | ALB | SSE support, sticky sessions |
| Broker | Alpaca Markets | REST + WebSocket, paper trading |
| IaC | Terraform | Reproducible infra |
