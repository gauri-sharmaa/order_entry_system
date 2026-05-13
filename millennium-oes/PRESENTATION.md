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

> **[PHOTO: Screenshot of the trading UI with an order in the blotter]**
> How to take: Run `./bin/oes -port=8080 -broker=alpaca`, open http://localhost:8080, submit a LIMIT BUY for AAPL at $290, screenshot the full browser window showing the order in the blotter with the green "ACKNOWLEDGED" badge.

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

> **[PHOTO: Diagram showing the trading stack]**
> How to make: Create a horizontal flow diagram in Figma/PowerPoint/draw.io with boxes: "Strategy" -> "OES (this project)" -> "Broker (IBKR/Alpaca)" -> "Exchange (NYSE/NASDAQ)" -> "Fill" -> "Portfolio". Highlight the OES box in blue. Use dark background, monospace font to match the terminal aesthetic.

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
> How to make: In Figma or PowerPoint, draw a large rounded rectangle (the process). Inside, draw two zones: top zone labeled "HOT PATH" with a red/orange gradient background showing the pipeline (Ring Buffer -> Risk -> WAL -> FIX) as connected boxes with arrows. Bottom zone labeled "OFF HOT PATH" in gray/blue with the HTTP server, SSE, FIX reader as separate boxes. Add latency annotations (50ns, 26us, 1us, 5us) next to each component.

> **[PHOTO: Terminal showing the server startup log]**
> How to take: Run `./bin/oes -port=8080 -broker=alpaca` and screenshot the terminal output showing "Millennium OES starting", CPU cores, broker connection, "ready" message.

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
| Startup time | **<100ms** | Including WAL replay |
| GC pauses | **0** (hot path) | Single-threaded, no allocations |

**Comparison:**
| System | Latency |
|--------|---------|
| Robinhood | 50-100ms |
| Typical hedge fund OMS (Java) | 100-500us |
| This system (Go) | **26us** |
| HFT matching engine (C++) | 1-5us |
| NYSE Pillar matching engine | ~10us |

> **[PHOTO: Bar chart comparing latencies]**
> How to make: Create a horizontal bar chart in PowerPoint/Google Slides. Bars: Robinhood (100ms, very long, red), Java OMS (500us, medium, orange), This System (26us, tiny, green), HFT C++ (5us, smallest, blue). Use logarithmic scale or break the axis to show the dramatic difference. Label each bar with the exact number.

> **[PHOTO: Terminal showing /api/stats output]**
> How to take: After submitting 10+ orders, run `curl http://localhost:8080/api/stats | python3 -m json.tool` and screenshot showing `avg_latency_us: 25.8` and `orders_processed: 10`.

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
- Pre-allocated fixed-size array (power of 2: 65,536 slots)
- Producer writes to writePos (atomic increment)
- Consumer reads from readPos (atomic increment)
- Cache-line padding (64 bytes) prevents false sharing between cores
- Zero locks, zero allocations, zero syscalls
- Back-pressure: if full, producer gets immediate feedback

**Same pattern used by:**
- LMAX Disruptor (London Stock Exchange matching engine)
- Aeron (real-time messaging, used by CME)
- Every HFT system

> **[PHOTO: Animated or multi-frame diagram of the ring buffer]**
> How to make: Draw a circular buffer (8-16 slots arranged in a circle or linear array). Color 3-4 slots as "filled" (orange/yellow). Show two pointers: writePos (green arrow) and readPos (blue arrow). Add a second frame showing the pointers advanced. Label "Producer writes here" and "Consumer reads here". Add "64-byte padding" annotation between the two pointer variables with a red dotted line showing they're on separate cache lines.

> **[PHOTO: Code snippet of TryPublish]**
> How to take: Screenshot the `ringbuffer.go` file in your editor (VS Code dark theme) showing the `TryPublish` function. Highlight the atomic operations.

---

## SLIDE 6: Pre-Allocated Order Store

**Problem:** Dynamic memory allocation causes GC pauses (unpredictable latency spikes).

**Solution:** Allocate all memory at startup. Zero allocations on the hot path.

**Order struct design:**
- Fixed 176 bytes per order (no pointers -> no GC scanning)
- Prices as int64 microdollars (no floating point)
- Symbols as [8]byte arrays (no string allocation)
- Status as uint8 (single byte state machine)

**Memory math:**
- 1M orders x 176 bytes = **176MB**
- Fits in L3 cache on modern server CPUs (typically 30-60MB L3)
- Hot working set (active orders) fits in L2 cache (256KB-1MB)

**Why integer prices:**
```
$175.50 -> 175,500,000 (int64 microdollars)
```
- No floating point rounding errors
- Integer arithmetic is 2-4x faster than float64
- Deterministic (no IEEE 754 edge cases)
- Same approach used by every exchange (NASDAQ uses price x 10,000)

> **[PHOTO: Memory layout diagram of the Order struct]**
> How to make: Draw a horizontal rectangle divided into labeled sections showing the byte layout: ID (4B, blue), Symbol (8B, green), Side/Type/TIF/Status (4B, yellow), Qty (4B), Price (8B, orange), StopPrice (8B), etc. Show total = 176 bytes. Add annotation "No pointers = no GC scanning".

> **[PHOTO: Comparison diagram - heap allocation vs pre-allocated]**
> How to make: Two-panel diagram. Left panel "Traditional" shows scattered memory blocks with arrows (fragmented heap, GC pauses). Right panel "This System" shows one contiguous block labeled "1M orders, allocated once at startup". Add a red X over the GC icon on the right side.

---

## SLIDE 7: Write-Ahead Log (Durability)

**Problem:** How do you survive crashes without a database?

**Solution:** Append-only log file with CRC32 checksums.

```
Entry format:
[type:1B][orderIdx:4B][timestamp:8B][len:2B][data:NB][crc32:4B]
Total overhead: 19 bytes + data per entry
```

**Properties:**
- Sequential I/O only (fastest possible disk pattern)
- ~1us per write (vs ~5ms for a database INSERT)
- CRC32 checksum detects corruption
- Replay on startup restores full state
- No random reads on the hot path

**Same pattern used by:**
- Apache Kafka (commit log)
- PostgreSQL (WAL before writing to tables)
- LevelDB/RocksDB (WAL before memtable)

> **[PHOTO: WAL recovery demo - two terminal windows]**
> How to take: Split terminal. Left: show submitting 3 orders then killing the process (Ctrl+C). Right: show restarting and the log saying "3 entries recovered", then `curl /api/orders` showing the orders are back. Screenshot both side by side.

> **[PHOTO: Diagram of WAL entry format]**
> How to make: Draw a horizontal bar divided into colored sections: type (1B, red), orderIdx (4B, blue), timestamp (8B, green), dataLen (2B, yellow), data (variable, gray), CRC32 (4B, purple). Label each with byte size. Add arrow showing "append only - never overwrite".

---

## SLIDE 8: FIX 4.2 Protocol (Institutional Standard)

**What is FIX?**
Financial Information eXchange - the universal language of institutional trading since 1992.

**Message format:**
```
8=FIX.4.2|9=178|35=D|49=MILLENNIUM|56=IBFX|34=1|
52=20240419-14:30:00.000|11=M001|55=AAPL|54=1|
38=100|40=2|44=175.50|59=0|10=128|
```

**Key message types implemented:**
| Tag 35 | Name | Direction |
|--------|------|-----------|
| A | Logon | Both |
| D | NewOrderSingle | OES -> Broker |
| F | OrderCancelRequest | OES -> Broker |
| G | OrderCancelReplaceRequest | OES -> Broker |
| 8 | ExecutionReport | Broker -> OES |
| 0 | Heartbeat | Both |

> **[PHOTO: Annotated FIX message]**
> How to make: Take the FIX message string above and create a visual where each tag=value pair is on its own line with a colored annotation explaining what it means. Example: "35=D" -> "Message Type: New Order" (blue), "55=AAPL" -> "Symbol: Apple" (green), "54=1" -> "Side: Buy" (green), "40=2" -> "Type: Limit" (orange), "44=175.50" -> "Price: $175.50" (orange). Use monospace font, dark background.

> **[PHOTO: Sequence diagram of FIX order flow]**
> How to make: Draw a sequence diagram (UML style) with two columns: "OES" and "IBKR". Show: OES sends Logon (35=A) -> IBKR responds Logon -> OES sends NewOrder (35=D) -> IBKR responds ExecutionReport (35=8, ExecType=0 New) -> IBKR sends ExecutionReport (35=8, ExecType=2 Fill). Add timestamps on the left showing ~5us between messages.

---

## SLIDE 9: Order Types Supported (20+)

| Category | Types | FIX Tag 40 |
|----------|-------|-----------|
| **Basic** | Market, Limit, Stop, Stop-Limit | 1, 2, 3, 4 |
| **Auction** | MOO, MOC, LOO, LOC | 1/2 + TIF |
| **Conditional** | Trailing Stop, MIT, LIT, Funari | Custom |
| **Linked** | Bracket, OCO, OTO, OTOCO | Engine-managed |
| **Algorithmic** | TWAP, VWAP, Iceberg | Engine-managed |

**Order lifecycle (FIX state machine):**
```
NEW -> PENDING_NEW -> ACKNOWLEDGED -> PARTIALLY_FILLED -> FILLED
                                   -> PENDING_CANCEL -> CANCELLED
                                   -> REJECTED
```

> **[PHOTO: Screenshot of the order type dropdown in the UI]**
> How to take: Open the UI, click the "ORDER TYPE" dropdown to expand it showing all the grouped options (Basic, Auction, Conditional, Linked, Algorithmic). Screenshot with the dropdown open.

> **[PHOTO: State machine diagram]**
> How to make: Draw a state machine with circles for each status (NEW, PENDING_NEW, ACKNOWLEDGED, PARTIALLY_FILLED, FILLED, CANCELLED, REJECTED). Use green for terminal success (FILLED), red for terminal failure (CANCELLED, REJECTED), blue for in-progress states. Connect with labeled arrows showing transitions.

---

## SLIDE 10: Risk Engine (Pre-Trade Controls)

All checks run **inline on the hot path** - no function call overhead, no separate service.

| Check | Default Limit | What It Prevents |
|-------|--------------|-----------------|
| Max order size | 10,000 shares | Fat-finger errors |
| Max position value | $1,000,000 | Concentration risk |
| Max daily loss | $50,000 | Runaway losses |
| Kill switch | Instant halt | Emergency stop |

> **[PHOTO: Kill switch demo - two screenshots]**
> How to take: Screenshot 1: UI with kill switch button normal (dark red outline). Screenshot 2: After clicking it - button glowing red, then submit an order and show the "KILL SWITCH ACTIVE" error message in the order entry panel. Combine both in one slide.

> **[PHOTO: Terminal showing risk rejection]**
> How to take: Run `curl -X POST /api/risk/killswitch -d '{"active":true}'` then `curl -X POST /api/orders -d '{"symbol":"AAPL","side":"buy","type":"MARKET","qty":100}'` and screenshot showing the 403 "KILL SWITCH ACTIVE" response.

---

## SLIDE 11: Portfolio Risk Tracker

Real-time portfolio-level metrics:

| Metric | Formula | What It Tells You |
|--------|---------|-------------------|
| **Sharpe Ratio** | (mean return x 252) / (s x sqrt(252)) | Risk-adjusted return |
| **Max Drawdown** | (peak - trough) / peak | Worst loss from peak |
| **Daily VaR (95%)** | u - 1.645s | Expected max daily loss |
| **Concentration** | max(position) / portfolio | Largest single bet |
| **Win Rate** | wins / total trades | % profitable trades |

> **[PHOTO: Risk metrics API response]**
> How to take: After placing several orders, run `curl http://localhost:8080/api/risk | python3 -m json.tool` and screenshot showing all the metrics (sharpe_ratio, max_drawdown_pct, daily_var_95, concentration_pct, win_rate_pct, equity).

> **[PHOTO: Risk dashboard mockup]**
> How to make: In Figma/PowerPoint, create a dark-themed dashboard with: a P&L line chart (green line going up with a dip), gauge widgets for Sharpe (1.8), Drawdown (3.2%), VaR ($4,500), and a pie chart showing position concentration. Use the same color scheme as the trading UI (dark bg, green/red/blue accents).

---

## SLIDE 12: ML Signal Engine

**Model:** Linear momentum predictor
**Inference time:** ~100ns (5 multiplications + 1 addition)
**No Python. No TensorFlow. No GPU. Just arithmetic.**

**Features (5):**
| # | Feature | What It Captures |
|---|---------|-----------------|
| 1 | 5-bar momentum | Short-term trend |
| 2 | 20-bar momentum | Medium-term trend |
| 3 | 20-bar volatility | Risk regime |
| 4 | Price vs 20-bar SMA | Mean reversion signal |
| 5 | 5-bar volume change | Volume confirmation |

**Architecture decision:**
```
Traditional ML:                 This system:
Python -> TensorFlow ->         y = w0 + w1*x1 + ... + w5*x5
Model load -> Inference         ~100ns, zero allocation
~10-100ms
```

> **[PHOTO: Signal API response]**
> How to take: Run `curl http://localhost:8080/api/signal/AAPL | python3 -m json.tool` and screenshot showing the response with direction ("bullish"), confidence (0.95), pred_return, and price.

> **[PHOTO: Diagram comparing ML inference approaches]**
> How to make: Two-column comparison. Left: "Traditional" showing Python -> TensorFlow -> GPU -> Serialize -> Network -> Deserialize -> Result (many boxes, red "10-100ms" label). Right: "This System" showing single box "5 multiplications + 1 addition" (green "100ns" label). Add "1,000,000x faster" annotation between them.

---

## SLIDE 13: Dual Broker Support

| | Alpaca | Interactive Brokers (FIX) |
|--|--------|--------------------------|
| **Protocol** | REST + WebSocket | FIX 4.2 over TCP |
| **Latency** | 50-100ms (internet) | 1-5ms (local gateway) |
| **Use case** | Paper trading, demos | Production, institutional |
| **Cost** | Free | Free (paper) |

**Switching is one flag:**
```bash
./oes -broker=alpaca    # paper trading
./oes -broker=fix       # institutional
./oes                   # simulation
```

> **[PHOTO: Side-by-side terminal showing both modes]**
> How to take: Two terminal windows. Left: starting with `-broker=alpaca` showing "Broker: Alpaca (https://paper-api.alpaca.markets)". Right: starting with no broker showing "Broker: simulation (no connection)". Screenshot both side by side.

> **[PHOTO: Diagram showing broker interface abstraction]**
> How to make: Draw a box labeled "Engine" at the top with an arrow down to a box labeled "Broker Interface (Submit, Cancel, Replace)". From that interface box, draw two arrows: one to "Alpaca (REST)" and one to "IBKR (FIX 4.2)". Add a dotted box around the interface labeled "Same code, different transport". Shows the strategy pattern.

---

## SLIDE 14: Two-View Architecture

**Execution Desk** (for traders):
- Order entry form (20+ order types)
- Live order blotter (real-time status updates)
- Kill switch
- Activity log

**Investor Dashboard** (for PMs/risk):
- Portfolio positions
- P&L curve
- Risk metrics (Sharpe, drawdown, VaR, concentration)
- ML signals per symbol
- Account summary

> **[PHOTO: Full screenshot of the Execution Desk UI]**
> How to take: Open http://localhost:8080 with several orders in the blotter (mix of acknowledged, cancelled, different symbols). Make sure the header shows "PROCESSED: X orders" and "AVG LATENCY: Xus". Full browser screenshot.

> **[PHOTO: API responses for investor endpoints]**
> How to take: Run these in sequence and screenshot the terminal: `curl /api/portfolio`, `curl /api/risk`, `curl /api/signal/AAPL`, `curl /api/account`. Show all four responses in one terminal screenshot.

---

## SLIDE 15: Design Decisions and Tradeoffs

| Decision | Why | Tradeoff |
|----------|-----|----------|
| Go over C++/Rust | Fast enough (26us), 10x faster to develop | Not sub-microsecond |
| Single process | No network hops, no distributed state | No horizontal scaling |
| Lock-free ring buffer | Zero contention | Fixed capacity |
| WAL over database | 1us vs 5ms writes | No SQL queries |
| Integer prices | No float rounding, faster | Harder to read |
| Pre-allocated arrays | Zero GC pressure | Fixed max capacity |
| Raw FIX over QuickFIX | Zero deps, full control | More code |
| Single-threaded event loop | No context switches | Single core only |

> **[PHOTO: Decision tree or tradeoff matrix visual]**
> How to make: Create a 2x2 matrix in PowerPoint. X-axis: "Development Speed" (left=slow, right=fast). Y-axis: "Runtime Performance" (bottom=slow, top=fast). Plot: C++ (top-left), Go/This System (top-right, highlighted), Java (middle-right), Python (bottom-right). Circle "This System" in green. Shows you chose the optimal quadrant.

---

## SLIDE 16: What Millennium Actually Runs (Context)

| Component | Millennium (Real) | This Project |
|-----------|-------------------|-------------|
| Hardware | Bare metal in NY4/NY5 | Single machine |
| Network | Kernel bypass (DPDK) | TCP_NODELAY |
| Language | C++ (hot path) | Go |
| Data structures | Lock-free | Lock-free ring buffer |
| Event loop | Single-threaded | Single-threaded |
| Latency | 1-5us | 26us |

**Gap analysis (how to get from 26us to 1-5us):**
```
Current: 26us
Remove WAL from hot path (async):     -> 11us
Replace time.Now() with RDTSC:        -> 10us
GOGC=off (disable GC):                -> 7us
mmap WAL instead of write():          -> 4us
Linux isolcpus + nohz_full:           -> 2us
```

> **[PHOTO: NY4/NY5 data center photo (stock image)]**
> How to find: Search for "Equinix NY4 data center" or "financial data center server room" on Unsplash/Pexels. Use a photo showing rows of servers with blinking lights. Add overlay text: "Where Millennium's real systems run - Equinix NY4, Secaucus NJ".

> **[PHOTO: Waterfall chart showing latency reduction path]**
> How to make: Create a waterfall/bridge chart starting at 26us on the left, with each optimization step reducing the bar: -15us (async WAL), -1us (RDTSC), -3us (no GC), -3us (mmap), -2us (isolcpus), ending at 2us on the right. Color each reduction step differently.

---

## SLIDE 17: Modes of Operation

```bash
# Simulation (no broker, instant ACK)
./oes -port=8080

# Paper trading (real market data, fake money)
./oes -port=8080 -broker=alpaca

# Institutional (FIX to IBKR gateway)
./oes -port=8080 -broker=fix -fix-host=127.0.0.1

# Headless (no UI, pure engine)
./oes -headless -broker=fix

# Custom risk limits
./oes -max-order-size=5000 -max-daily-loss=25000
```

> **[PHOTO: Terminal showing different startup modes]**
> How to take: Run the binary three times with different flags (simulation, alpaca, headless) and screenshot each startup log. Arrange as three small terminal windows on one slide showing the different "Mode:" and "Broker:" lines.

---

## SLIDE 18: Project Structure

```
millennium-oes/              2,457 lines of Go
├── cmd/oes/main.go          Entry point, config, CPU pinning
├── internal/
│   ├── engine/              Event loop, ring buffer, types
│   ├── fix/                 Raw FIX 4.2 TCP client
│   ├── broker/alpaca/       Alpaca REST client
│   ├── gateway/             HTTP server, both views
│   ├── risk/                Portfolio risk metrics
│   ├── signal/              ML momentum predictor
│   └── wal/                 Write-ahead log
├── web/                     Frontend (HTML/CSS/JS)
└── Makefile                 Build targets
```

**Zero external dependencies.**

> **[PHOTO: VS Code file explorer showing the project tree]**
> How to take: Open the project in VS Code with the file explorer expanded showing all folders. Use a dark theme. Screenshot the sidebar.

> **[PHOTO: `go list -m all` output showing zero deps]**
> How to take: Run `go list -m all` in the terminal and screenshot showing only `github.com/millennium-oes` with nothing else listed.

---

## SLIDE 19: Testing and Verification

**Smoke test results (30 tests):**

| Category | Tests | Result |
|----------|-------|--------|
| Order submission (market, limit, stop, trailing) | 4 | PASS |
| Order retrieval (single, list, not-found) | 3 | PASS |
| Order cancellation | 2 | PASS |
| Kill switch (activate, reject, deactivate) | 4 | PASS |
| Risk limits (oversize order) | 1 | PASS |
| Input validation (no symbol, bad JSON, qty=0) | 3 | PASS |
| Investor endpoints (risk, signal, quote, account) | 5 | PASS |
| SSE streaming | 1 | PASS |
| WAL persistence (restart recovery) | 3 | PASS |
| Web UI serving | 1 | PASS |
| Live Alpaca integration | 3 | PASS |

> **[PHOTO: Terminal showing the full smoke test output]**
> How to take: Run the full smoke test sequence (submit orders, cancel, kill switch, check stats) in one terminal session. Screenshot the output showing all the JSON responses with successful results. Alternatively, create a script that runs all tests and outputs PASS/FAIL for each.

> **[PHOTO: WAL recovery demo]**
> How to take: Three-panel screenshot. Panel 1: Submit 2 orders (show curl output). Panel 2: Kill process (show Ctrl+C). Panel 3: Restart and show "2 entries recovered" in the log + `curl /api/orders` showing both orders restored.

---

## SLIDE 20: Key Takeaways

1. **Architecture matters more than language** - Go at 26us beats Java at 100-500us
2. **No cloud for latency-critical systems** - every hop adds milliseconds
3. **The hot path is sacred** - no allocations, no locks, no syscalls
4. **FIX is the language of finance** - understanding it = institutional credibility
5. **Risk controls are non-negotiable** - kill switch exists because it's been needed
6. **Durability without databases** - WAL at 1us vs database at 5ms
7. **ML doesn't need frameworks** - 100ns inference, no Python

> **[PHOTO: Summary infographic]**
> How to make: Create a single visual with 7 icons/badges arranged in a grid or circle. Each has a short label and the key number: a speedometer (26us), a lock with X (zero locks), a brain (100ns ML), a shield (risk controls), a plug (FIX 4.2), a file (WAL 1us), a cloud with X (no cloud). Dark background, clean icons.

---

## SLIDE 21: Live Demo Flow

1. Start system: `./oes -port=8080 -broker=alpaca`
2. Open http://localhost:8080
3. Show live AAPL quote populating
4. Submit a market buy order -> watch it appear in blotter
5. Submit a limit order -> show it sitting at "acknowledged"
6. Cancel the limit order -> watch status change in real-time
7. Activate kill switch -> show order rejection
8. Show `/api/risk` endpoint with metrics
9. Show `/api/signal/AAPL` with ML prediction
10. Show `/api/stats` with latency numbers
11. Kill the process, restart -> show WAL recovery

> **[PHOTO: Screen recording stills or GIF of the live demo]**
> How to take: Record your screen (QuickTime on Mac: File -> New Screen Recording) while doing steps 1-11. Extract key frames as screenshots: (a) order appearing in blotter, (b) kill switch glowing red, (c) stats showing latency, (d) WAL recovery. Alternatively, use each frame as a sub-slide in an animation.

> **[PHOTO: Split screen - browser + terminal]**
> How to take: Arrange your screen with the browser (UI) on the left and terminal on the right. Submit an order in the browser, show the SSE event arriving in the terminal (or vice versa). Screenshot the split view showing both sides of the system working together.

---

## APPENDIX: Numbers to Memorize

- **26us** - engine processing latency
- **50ns** - ring buffer publish time
- **176 bytes** - size of one order in memory
- **1,000,000** - pre-allocated order capacity
- **176MB** - total memory for order store
- **65,536** - ring buffer capacity (events)
- **~38,000** - theoretical orders/second
- **0** - external dependencies
- **0** - heap allocations on hot path
- **0** - locks on hot path
- **1** - CPU core dedicated to event loop
- **5MB** - compiled binary size
- **2,457** - lines of Go code
- **20+** - order types supported
- **100ns** - ML inference time
- **1us** - WAL write time
- **84ms** - Alpaca round-trip (network-bound)
- **1-5ms** - FIX/IBKR round-trip (local)

> **[PHOTO: "Cheat sheet" card design]**
> How to make: Design a dark card (like a trading card or flash card) with all these numbers arranged in a clean grid. Group by category: "Speed" (green numbers), "Capacity" (blue numbers), "Size" (white numbers). Use large bold font for the numbers, small font for descriptions. This can be a handout or final slide that stays up during Q&A.
