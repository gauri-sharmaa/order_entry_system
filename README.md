# Order Entry System

A single-process order entry and execution engine in Go, built as a student project with Millennium through Georgia Tech's Center for Finance and Technology. It takes orders over HTTP, runs pre-trade risk checks, logs them to a write-ahead log, and routes them to a broker over raw FIX 4.2 (or a simulator).

It is an order-entry / routing system, not an exchange matching engine: there is no order book and no price-time matching across counterparties.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  One process, one machine                                        │
│                                                                  │
│  HTTP Gateway ──→ Ring Buffer ──→ Event Loop ──→ FIX Client      │
│  (goroutines)     (SPSC, 64K)    (single thread)  (raw TCP)      │
│                                       │                          │
│                                       └──→ Write-Ahead Log       │
│                                                                  │
│  Orders: pre-allocated array (1M slots)                          │
│  Prices: int64 microdollars ($175.50 = 175,500,000)              │
└──────────────────────────────────────────────────────────────────┘
```

## Quick Start

```bash
cd millennium-oes

go build -o ./bin/oes ./cmd/oes

# Simulation mode (no broker)
./bin/oes -port=8080          # then open http://localhost:8080

# IBKR paper gateway
./bin/oes -fix-host=127.0.0.1 -fix-port=4002 -fix-account=YOUR_ACCOUNT

# Microbenchmarks
make bench
```

## Features

- SPSC ring buffer with cache-line-padded cursors (Disruptor-style)
- Single-threaded event loop that owns all order state
- Pre-allocated order store (1M slots, fixed-width fields)
- Write-ahead log with CRC32 per record and replay on startup
- Raw FIX 4.2 client (NewOrderSingle, cancel, ExecutionReport parsing), TCP_NODELAY
- Integer fixed-point prices on the hot path
- Inline pre-trade risk: max order size, daily loss limit, kill switch
- Order types sent to brokers: Market, Limit, Stop, Stop-Limit (FIX and Alpaca), Trailing Stop (Alpaca)
- Real-time web UI over Server-Sent Events
- Standard library only

## Performance

**~26µs average engine latency per order** (simulation mode).

What that number covers: the time inside the event loop from dequeuing a submit event to finishing it — risk checks, WAL append, simulated ack, and notifying SSE subscribers. Measured with the engine's `/api/stats` counter on a laptop, averaged over a small run of orders with no warmup. It does not include HTTP parsing, the network, or a real broker round trip (with Alpaca paper trading, end to end was ~84ms, almost all of it network).

Warm microbenchmarks of the same path (`make bench`, Intel Xeon 2.1GHz, Go 1.24):

| Benchmark | Time/op | Allocs/op |
|-----------|---------|-----------|
| Ring buffer publish + consume | ~34ns | 0 |
| `processSubmit`, no WAL | ~108ns | 0 |
| `processSubmit`, WAL on (write, no fsync) | ~1.1µs | 2 |

The gap between ~1µs warm and ~26µs measured is mostly cold-start cost and SSE notification, which the microbenchmark leaves out.

## Known Limitations

- **Single-producer assumption.** The ring buffer is SPSC, but concurrent HTTP handlers and the FIX/Alpaca fill readers all publish into it. That breaks the single-producer invariant; it needs one ingress goroutine or an MPSC queue.
- **Ack semantics.** An order is marked Acknowledged when the FIX write succeeds. A real ack is the broker's ExecutionReport (35=8, OrdStatus=New); a successful TCP write is not one.
- **WAL durability.** Records are written with `write()` but only fsynced on shutdown, so a power loss can drop recent orders. WAL write errors on the hot path are not checked. After a crash, local replay is not reconciled against the broker's open orders.
- **Risk checks.** Position-value and price-deviation limits are configurable but not enforced in the submit path. Only order size, daily loss, and the kill switch are.
- **CPU pinning.** `LockOSThread` and `sched_setaffinity` are applied to the main goroutine, but the event loop runs in its own goroutine, so it is not actually pinned. Pinning is also a no-op on macOS.
- **Order types.** Enum values exist for MOO/MOC, MIT/LIT, Bracket, OCO, OTO, TWAP, VWAP, and Iceberg, but their scheduling logic (child orders, linked cancels) is not implemented in this version.
- **Allocations.** The WAL path allocates two small buffers per order. These could come from a reused buffer.

## Documentation

- [millennium-oes/README.md](millennium-oes/README.md) — technical details, API, project layout
