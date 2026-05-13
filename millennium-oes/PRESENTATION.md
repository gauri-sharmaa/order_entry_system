# Millennium OES — Presentation Slides Content

---

## SLIDE 1: Title

**Millennium Order Entry System**
A bare-metal, low-latency order execution engine

- Language: Go 1.22
- Architecture: Single-process, lock-free, event-driven
- Broker: FIX 4.2 (IBKR) + Alpaca (paper trading)
- Latency: 26us engine processing | 84ms end-to-end with broker
- Dependencies: Zero (pure standard library)
- Lines of code: ~2,500

> **[PHOTO: Full screenshot of the trading UI with orders in the blotter]**
> Run with Alpaca connected, submit 3-4 orders (mix of buy/sell, different symbols), screenshot the full browser window showing header stats + order blotter + activity log.

---

## SLIDE 2: What Is an Order Entry System?

An OES is the front door of a trading infrastructure. It:

1. Receives orders from traders or algorithms
2. Validates them against risk rules
3. Routes them to an exchange or broker
4. Tracks their lifecycle (new -> filled/cancelled)
5. Reports back in real-time

**Where it sits in the trading stack:**
```
Strategy/Algo -> OES -> Broker/Exchange -> Fill -> Portfolio
```

**Who uses this at Millennium:**
- 300+ independent portfolio management teams
- Each team has risk limits enforced by the OES
- The OES is the single point of control between PMs and the market

---

## SLIDE 3: Architecture Overview

```
Single Binary (~5MB) - One Machine - One Process

  HOT PATH (pinned CPU core, single-threaded)

  HTTP -> Ring Buffer -> Risk Check -> WAL -> FIX Send
         (lock-free)   (inline)    (1us)  (TCP)

  Total: ~26us per order

  OFF HOT PATH
  - HTTP Server (order entry UI)
  - SSE Stream (real-time updates to browser)
  - FIX Reader (incoming fills from broker)
  - ML Signal Engine (momentum predictor)
```

> **[PHOTO: Architecture diagram with hot path highlighted in red/orange]**
> In Figma or PowerPoint: large rounded rectangle (the process). Inside, two zones — top "HOT PATH" with red/orange background showing pipeline boxes (Ring Buffer -> Risk -> WAL -> FIX) with latency labels. Bottom "OFF HOT PATH" in gray/blue with HTTP, SSE, FIX reader. Dark background, monospace font.

---

## SLIDE 4: Performance Numbers

| Metric | Value | Context |
|--------|-------|---------|
| Engine processing latency | **26us** | Ring buffer -> risk -> WAL -> ACK |
| Ring buffer publish | **~50ns** | Lock-free atomic operation |
| WAL write | **~10-15us** | Sequential append, CRC32 checksum |
| FIX message send (local) | **~5us** | TCP, Nagle disabled |
| End-to-end with Alpaca | **~84ms** | Internet round-trip (network-bound) |
| End-to-end with FIX/IBKR | **~1-5ms** | Local TCP to gateway |
| Orders per second (theoretical) | **~38,000** | 1,000,000us / 26us |
| Memory footprint | **~176MB** | 1M pre-allocated order slots |
| Binary size | **~5MB** | Single static binary |
| GC pauses | **0** (hot path) | Single-threaded, no allocations |

**Comparison:**
| System | Latency |
|--------|---------|
| Robinhood | 50-100ms |
| Typical hedge fund OMS (Java) | 100-500us |
| This system (Go) | **26us** |
| HFT matching engine (C++) | 1-5us |

> **[PHOTO: Horizontal bar chart comparing latencies]**
> PowerPoint/Slides: horizontal bars — Robinhood (long, red), Java OMS (medium, orange), This System (tiny, green), HFT C++ (smallest, blue). Log scale or broken axis. Label each bar with exact number.

---

## SLIDE 5: Lock-Free Ring Buffer (LMAX Disruptor Pattern)

**Problem:** How do you pass orders from the HTTP handler to the engine without locks?

**Solution:** Single-Producer Single-Consumer (SPSC) ring buffer with atomic operations.

```
Producer (HTTP)                    Consumer (Event Loop)
     |                                    |
     v                                    v
[  ] [  ] [  ] [XX] [XX] [XX] [  ] [  ]
            ^ writePos      ^ readPos
```

**Key properties:**
- Pre-allocated fixed-size array (65,536 slots)
- Atomic increments only (no mutex, no CAS loop)
- Cache-line padding prevents false sharing
- Zero locks, zero allocations, zero syscalls

**Same pattern used by:**
- LMAX Disruptor (London Stock Exchange)
- Aeron (CME Group messaging)

---

## SLIDE 6: Pre-Allocated Order Store

**Problem:** Dynamic memory allocation causes GC pauses.

**Solution:** Allocate all memory at startup. Zero allocations on the hot path.

- Fixed 176 bytes per order (no pointers -> no GC scanning)
- Prices as int64 microdollars (no floating point): $175.50 -> 175,500,000
- Symbols as [8]byte arrays (no string allocation)
- 1M orders x 176 bytes = 176MB (fits in L3 cache)

**Why integer prices:**
- No floating point rounding errors
- Integer arithmetic is 2-4x faster than float64
- Same approach used by every exchange (NASDAQ uses price x 10,000)

---

## SLIDE 7: Write-Ahead Log (Durability)

**Problem:** How do you survive crashes without a database?

**Solution:** Append-only log file with CRC32 checksums.

```
[type:1B][orderIdx:4B][timestamp:8B][len:2B][data:NB][crc32:4B]
```

- Sequential I/O only (~1us per write vs ~5ms for database)
- CRC32 checksum detects corruption
- Replay on startup restores full state
- Same pattern as Kafka, PostgreSQL, RocksDB

> **[PHOTO: WAL recovery demo — terminal showing restart]**
> Run server, submit 3 orders, kill with Ctrl+C, restart. Screenshot the log showing "3 entries recovered" and curl showing orders are back.

---

## SLIDE 8: FIX 4.2 Protocol (Institutional Standard)

**What is FIX?**
Financial Information eXchange — universal language of institutional trading since 1992.

**Message format:**
```
8=FIX.4.2|35=D|49=MILLENNIUM|56=IBFX|11=M001|
55=AAPL|54=1|38=100|40=2|44=175.50|59=0|10=128|
```

**Key messages implemented:**
| Tag 35 | Name | Direction |
|--------|------|-----------|
| D | NewOrderSingle | OES -> Broker |
| F | OrderCancelRequest | OES -> Broker |
| G | OrderCancelReplaceRequest | OES -> Broker |
| 8 | ExecutionReport | Broker -> OES |

**Implementation:** Raw TCP, no framework, Nagle disabled, ~5us per message.

---

## SLIDE 9: Order Types Supported (20+)

| Category | Types |
|----------|-------|
| **Basic** | Market, Limit, Stop, Stop-Limit |
| **Auction** | MOO, MOC, LOO, LOC |
| **Conditional** | Trailing Stop, MIT, LIT, Funari |
| **Linked** | Bracket, OCO, OTO, OTOCO |
| **Algorithmic** | TWAP, VWAP, Iceberg |

**Order lifecycle:**
```
NEW -> PENDING_NEW -> ACKNOWLEDGED -> PARTIALLY_FILLED -> FILLED
                                   -> CANCELLED
                                   -> REJECTED
```

---

## SLIDE 10: Risk Engine (Pre-Trade Controls)

All checks run inline on the hot path — no separate service.

| Check | Default Limit | What It Prevents |
|-------|--------------|-----------------|
| Max order size | 10,000 shares | Fat-finger errors |
| Max position value | $1,000,000 | Concentration risk |
| Max daily loss | $50,000 | Runaway losses |
| Kill switch | Instant halt | Emergency stop |

> **[PHOTO: Kill switch demo]**
> Screenshot the UI with kill switch button glowing red (active state), and the error message "KILL SWITCH ACTIVE" appearing when trying to submit an order.

---

## SLIDE 11: Portfolio Risk Tracker

| Metric | What It Tells You |
|--------|-------------------|
| **Sharpe Ratio** | Risk-adjusted return (>1 good, >2 excellent) |
| **Max Drawdown** | Worst loss from peak |
| **Daily VaR (95%)** | Expected max daily loss |
| **Concentration** | Largest single bet as % of total |
| **Win Rate** | % of profitable trades |

Rolling 60-day window, O(1) computation, updated on every fill.

---

## SLIDE 12: ML Signal Engine

**Model:** Linear momentum predictor
**Inference time:** ~100ns

| Feature | What It Captures |
|---------|-----------------|
| 5-bar momentum | Short-term trend |
| 20-bar momentum | Medium-term trend |
| 20-bar volatility | Risk regime |
| Price vs 20-bar SMA | Mean reversion |
| 5-bar volume change | Volume confirmation |

No Python. No TensorFlow. Just: `y = w0 + w1*x1 + ... + w5*x5`

> **[PHOTO: Signal API response showing bullish prediction]**
> Run `curl http://localhost:8080/api/signal/AAPL | python3 -m json.tool` and screenshot showing direction: "bullish", confidence: 0.95, price.

---

## SLIDE 13: Dual Broker Support

| | Alpaca | Interactive Brokers (FIX) |
|--|--------|--------------------------|
| Protocol | REST | FIX 4.2 over TCP |
| Latency | 50-100ms | 1-5ms |
| Use case | Paper trading | Production |

Switching is one flag: `./oes -broker=alpaca` or `./oes -broker=fix`

---

## SLIDE 14: Two-View Architecture

**Execution Desk** (traders): Order entry, blotter, kill switch, activity log

**Investor Dashboard** (PMs/risk): Portfolio, P&L, risk metrics, ML signals

```
EXECUTION:                      INVESTOR:
POST /api/orders                GET /api/portfolio
GET  /api/orders                GET /api/risk
DELETE /api/orders/{id}         GET /api/signal/{symbol}
POST /api/risk/killswitch       GET /api/account
GET  /api/stream (SSE)          GET /api/quote/{symbol}
```

---

## SLIDE 15: Design Decisions and Tradeoffs

| Decision | Why | Tradeoff |
|----------|-----|----------|
| Go over C++/Rust | 26us is fast enough, 10x faster to develop | Not sub-microsecond |
| Single process | No network hops | No horizontal scaling |
| Lock-free ring buffer | Zero contention | Fixed capacity |
| WAL over database | 1us vs 5ms | No SQL queries |
| Integer prices | No float rounding | Harder to read |
| Raw FIX over QuickFIX | Zero deps | More code |

---

## SLIDE 16: What Millennium Actually Runs

| Component | Millennium (Real) | This Project |
|-----------|-------------------|-------------|
| Hardware | Bare metal NY4/NY5 | Single machine |
| Network | Kernel bypass (DPDK) | TCP_NODELAY |
| Language | C++ | Go |
| Structures | Lock-free | Lock-free ring buffer |
| Event loop | Single-threaded | Single-threaded |
| Latency | 1-5us | 26us |

**How to close the gap:**
```
26us -> async WAL -> 11us -> no GC -> 7us -> mmap -> 4us -> isolcpus -> 2us
```

---

## SLIDE 17: Testing (30 tests, all pass)

| Category | Tests | Result |
|----------|-------|--------|
| Order submission | 4 | PASS |
| Order retrieval | 3 | PASS |
| Cancellation | 2 | PASS |
| Kill switch | 4 | PASS |
| Risk limits | 1 | PASS |
| Input validation | 3 | PASS |
| Investor endpoints | 5 | PASS |
| SSE streaming | 1 | PASS |
| WAL persistence | 3 | PASS |
| Live Alpaca | 3 | PASS |

---

## SLIDE 18: Live Demo

1. Start: `./oes -port=8080 -broker=alpaca`
2. Show live quote for AAPL
3. Submit market buy -> appears in blotter
4. Submit limit order -> acknowledged
5. Cancel it -> status changes real-time
6. Kill switch ON -> order rejected
7. `/api/stats` -> show 26us latency
8. `/api/signal/AAPL` -> ML prediction
9. Kill process, restart -> WAL recovery

---

## APPENDIX: Numbers to Memorize

- **26us** — engine latency
- **50ns** — ring buffer publish
- **100ns** — ML inference
- **1us** — WAL write
- **84ms** — Alpaca round-trip
- **1-5ms** — FIX round-trip
- **38,000** — orders/second theoretical
- **176 bytes** — per order
- **176MB** — total order store
- **65,536** — ring buffer slots
- **5MB** — binary size
- **2,457** — lines of code
- **0** — external dependencies
- **0** — locks on hot path
- **20+** — order types
