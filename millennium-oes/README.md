# Millennium OES — Bare-Metal Order Entry System

A low-latency order entry and execution engine modeled after institutional trading infrastructure. Single process, single machine, everything in memory.

**Standard library only. Integer prices. Single-threaded event loop.**

This is an order-entry / routing engine, not an exchange matching engine. See the [top-level README](../README.md) for measured performance and known limitations.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│  Single Process (one binary, ~5MB)                              │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  HOT PATH (single-threaded event loop)                  │   │
│  │                                                         │   │
│  │  Ring Buffer ──→ Risk Check ──→ WAL Write ──→ FIX Send  │   │
│  │  (lock-free)     (inline)       (sequential)  (TCP)     │   │
│  │                                                         │   │
│  │  ~26µs avg per order (see top-level README)             │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  OFF HOT PATH (separate goroutines)                     │   │
│  │                                                         │   │
│  │  HTTP Server ──→ Web UI ──→ SSE Stream                  │   │
│  │  FIX Reader  ──→ Fill Events ──→ Ring Buffer            │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  Storage: Write-Ahead Log (append-only file, CRC32)            │
│  Orders:  Pre-allocated array (1M slots, ~176MB)               │
└─────────────────────────────────────────────────────────────────┘
```

## What Makes This Different

| Traditional (Cloud) | This System (Bare Metal) |
|--------------------|-----------------------|
| Redis for state | In-process memory array |
| DynamoDB for persistence | Write-Ahead Log (append-only file) |
| ALB + ECS Fargate | Single binary, one machine |
| REST API to broker | Raw FIX 4.2 over TCP |
| Goroutines + mutexes | Lock-free ring buffer, single-threaded event loop |
| JSON everywhere | Fixed-size structs, integer prices |
| Float64 prices | Int64 microdollars (no floating point) |

## Key Design Decisions

- **Lock-free SPSC ring buffer** — same idea as the LMAX Disruptor (from the LMAX exchange). Assumes one producer; see Known Limitations
- **Pre-allocated order store** — 1M order slots allocated at startup; `processSubmit` without the WAL makes 0 allocations (`make bench`)
- **Integer prices** — all prices stored as int64 microdollars ($175.50 = 175,500,000), eliminates floating point
- **Single-threaded event loop** — no context switches, no lock contention, deterministic latency
- **CPU pinning (partial)** — `sched_setaffinity` on Linux, currently applied to the main goroutine rather than the event-loop goroutine
- **Write-Ahead Log** — append-only sequential I/O, CRC32 checksums, replay on restart
- **Raw FIX implementation** — no QuickFIX dependency
- **Nagle disabled** — TCP_NODELAY on FIX connection for immediate sends

## Quick Start

```bash
# Build (produces a single ~5MB binary)
go build -o ./bin/oes ./cmd/oes

# Run in simulation mode (no broker connection)
./bin/oes -port=8080

# Run with IBKR FIX gateway
./bin/oes -fix-host=127.0.0.1 -fix-port=4002 -fix-account=YOUR_ACCOUNT

# Open http://localhost:8080
```

## Dependencies

**None.** Pure Go standard library. No external packages.

```
$ go list -m all
github.com/millennium-oes
```

## Project Structure

```
millennium-oes/
├── cmd/oes/                 # Entry point — config, CPU pinning, startup
├── internal/
│   ├── engine/              # Core engine
│   │   ├── engine.go        # Event loop, order processing, risk checks
│   │   ├── ringbuffer.go    # Lock-free SPSC ring buffer
│   │   └── types.go         # Order struct, enums, price helpers
│   ├── fix/                 # Raw FIX 4.2 TCP client (no framework)
│   │   └── client.go        # Connection, message building, fill handling
│   ├── gateway/             # HTTP server + SSE (off hot path)
│   │   └── gateway.go       # REST API, order submission, streaming
│   ├── wal/                 # Write-Ahead Log (durability)
│   │   └── wal.go           # Append, replay, CRC verification
│   ├── broker/alpaca/       # Alpaca REST broker (paper trading)
│   ├── risk/                # Portfolio risk metrics (off hot path)
│   ├── signal/              # Momentum signal
│   └── strategy/            # Strategy manager
├── web/                     # Trading UI (HTML/CSS/JS)
└── scripts/
    └── run-local.sh         # Build and run
```

## API

```
POST   /api/orders          Submit order
GET    /api/orders          List active orders
GET    /api/orders/{id}     Get order by ID
DELETE /api/orders/{id}     Cancel order
GET    /api/risk            Risk status
POST   /api/risk/killswitch Toggle kill switch
GET    /api/stats           Engine performance stats
GET    /api/stream          SSE real-time updates
```

## Performance

~26µs average per order inside the event loop (simulation mode, `/api/stats`). Warm microbenchmarks:

```
make bench
BenchmarkRingPublishConsume      ~34 ns/op     0 allocs/op
BenchmarkProcessSubmitNoWAL     ~108 ns/op     0 allocs/op
BenchmarkProcessSubmitWithWAL   ~1.1 µs/op     2 allocs/op
```

Measured on an Intel Xeon 2.1GHz, Go 1.24. See the top-level README for exactly what each number covers.

## How It Mirrors Institutional Systems

| Millennium/Citadel | This Project |
|-------------------|-------------|
| Bare metal in NY4/NY5 | Single machine, no cloud |
| Kernel bypass (DPDK) | TCP_NODELAY (no kernel bypass) |
| Lock-free structures | SPSC ring buffer |
| Single-threaded event loop | Go event loop with LockOSThread |
| FIX 4.2 to prime broker | Raw FIX 4.2 to IBKR |
| In-memory order state | Pre-allocated order array |
| Sequential log for audit | Write-Ahead Log |
| FPGA for market data | (not implemented — would need hardware) |
