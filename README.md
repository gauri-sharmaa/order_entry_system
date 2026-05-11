# Millennium Order Entry System

A bare-metal, low-latency order entry and execution engine modeled after institutional trading infrastructure (Millennium, Citadel, Two Sigma).

**Single process. Single machine. Zero cloud. Zero dependencies. Sub-microsecond event processing.**

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│  One Binary (~5MB) — One Machine — One Process                  │
│                                                                 │
│  HTTP Gateway ──→ Ring Buffer ──→ Event Loop ──→ FIX Client     │
│  (off hot path)   (lock-free)    (pinned core)   (raw TCP)      │
│                                                                 │
│  Storage: Write-Ahead Log (append-only, ~1μs/write)             │
│  Orders:  Pre-allocated array (1M slots, zero GC)               │
│  Prices:  Int64 microdollars (no floating point on hot path)    │
└─────────────────────────────────────────────────────────────────┘
```

## Quick Start

```bash
cd millennium-oes

# Build
go build -o ./bin/oes ./cmd/oes

# Run (simulation mode — no broker needed)
./bin/oes -port=8080

# Open http://localhost:8080

# With IBKR FIX gateway:
./bin/oes -fix-host=127.0.0.1 -fix-port=4002 -fix-account=YOUR_ACCOUNT
```

## Key Features

- **Lock-free SPSC ring buffer** (LMAX Disruptor pattern)
- **Single-threaded event loop** with CPU pinning
- **Pre-allocated order store** (1M orders, ~176MB, zero GC pressure)
- **Write-Ahead Log** with CRC32 checksums and replay
- **Raw FIX 4.2 client** (no framework, TCP_NODELAY)
- **Integer arithmetic** (microdollar prices, no floats)
- **Zero external dependencies** (pure Go standard library)
- **20+ order types** (Market, Limit, Stop, Bracket, OCO, TWAP, Iceberg, etc.)
- **Inline risk checks** (position limits, daily loss, kill switch)
- **Real-time web UI** with Server-Sent Events

## Performance

| Operation | Latency |
|-----------|---------|
| Ring buffer publish | ~50ns |
| Event loop processing | ~500ns-2μs |
| WAL write | ~1μs |
| FIX message send | ~5μs |
| **Total hot path** | **~5-10μs** |

For comparison: Alpaca REST = 5-50ms (1000x slower), Redis = 0.5ms (100x slower)

## Documentation

- **[millennium-oes/README.md](millennium-oes/README.md)** — Technical deep-dive
- **[millennium-oes/EXPLAINER.md](millennium-oes/EXPLAINER.md)** — Non-technical explanation with diagrams
