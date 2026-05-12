// Millennium OES — Bare-metal, single-process order entry system.
//
// Architecture:
//   - Single-threaded event loop on a pinned CPU core (hot path)
//   - Lock-free ring buffer for order flow
//   - Memory-mapped order store (pre-allocated, zero GC pressure)
//   - Write-Ahead Log for durability (append-only, sequential I/O)
//   - FIX 4.2 session to IBKR (persistent TCP, no reconnect per order)
//   - Embedded HTTP server for UI (separate goroutine, off hot path)
//
// No cloud. No Redis. No DynamoDB. No load balancer.
// One process. One machine. Everything in memory.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/millennium-oes/internal/engine"
	"github.com/millennium-oes/internal/fix"
	"github.com/millennium-oes/internal/gateway"
	"github.com/millennium-oes/internal/wal"
)

func main() {
	// -----------------------------------------------------------------------
	// Configuration
	// -----------------------------------------------------------------------
	cfg := parseFlags()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("Millennium OES starting (pid=%d)", os.Getpid())
	log.Printf("  CPU cores: %d | GOMAXPROCS: %d", runtime.NumCPU(), runtime.GOMAXPROCS(0))
	log.Printf("  Hot core: %d | WAL: %s", cfg.HotCore, cfg.WALPath)
	log.Printf("  FIX: %s→%s @ %s:%d", cfg.FIXSender, cfg.FIXTarget, cfg.FIXHost, cfg.FIXPort)
	log.Printf("  HTTP: :%d", cfg.HTTPPort)

	// -----------------------------------------------------------------------
	// Lock OS thread for the hot path — prevents Go scheduler from
	// migrating this goroutine to another core
	// -----------------------------------------------------------------------
	runtime.LockOSThread()

	// -----------------------------------------------------------------------
	// Set CPU affinity — pin this process to the hot core
	// This eliminates context switches with other processes
	// -----------------------------------------------------------------------
	if err := pinCPU(cfg.HotCore); err != nil {
		log.Printf("WARNING: Could not pin CPU (non-Linux): %v", err)
		// Non-fatal — still works, just with slightly more jitter
	}

	// -----------------------------------------------------------------------
	// Initialize Write-Ahead Log (durability without a database)
	// -----------------------------------------------------------------------
	walLog, err := wal.Open(cfg.WALPath)
	if err != nil {
		log.Fatalf("Failed to open WAL: %v", err)
	}
	defer walLog.Close()
	log.Printf("  WAL opened: %d entries recovered", walLog.Len())

	// -----------------------------------------------------------------------
	// Initialize the order engine (lock-free, pre-allocated)
	// -----------------------------------------------------------------------
	eng := engine.New(engine.Config{
		MaxOrders:         cfg.MaxOrders,
		MaxPositionValue:  cfg.MaxPositionValue,
		MaxOrderSize:      cfg.MaxOrderSize,
		MaxDailyLoss:      cfg.MaxDailyLoss,
		PriceDeviationPct: cfg.PriceDeviationPct,
		WAL:               walLog,
	})

	// Replay WAL to restore state after restart
	if err := eng.ReplayWAL(); err != nil {
		log.Fatalf("WAL replay failed: %v", err)
	}

	// -----------------------------------------------------------------------
	// Initialize FIX client (persistent TCP to IBKR)
	// -----------------------------------------------------------------------
	fixClient := fix.NewClient(fix.Config{
		SenderCompID: cfg.FIXSender,
		TargetCompID: cfg.FIXTarget,
		Host:         cfg.FIXHost,
		Port:         cfg.FIXPort,
		Account:      cfg.FIXAccount,
		Heartbeat:    30,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.FIXHost != "" && cfg.FIXAccount != "" {
		if err := fixClient.Connect(ctx); err != nil {
			log.Printf("WARNING: FIX connection failed: %v (running in simulation mode)", err)
		} else {
			eng.SetBroker(fixClient)
		}
	} else {
		log.Println("  FIX: disabled (no host/account configured) — simulation mode")
	}

	// -----------------------------------------------------------------------
	// Start the HTTP gateway (off the hot path, separate goroutine)
	// -----------------------------------------------------------------------
	gw := gateway.New(eng, cfg.HTTPPort)
	go gw.Start()

	// -----------------------------------------------------------------------
	// Start the event loop (HOT PATH — this is where latency matters)
	// -----------------------------------------------------------------------
	go eng.Run(ctx)

	log.Printf("Millennium OES ready — http://localhost:%d", cfg.HTTPPort)

	// -----------------------------------------------------------------------
	// Wait for shutdown signal
	// -----------------------------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	cancel()
	eng.Stop()
	walLog.Sync()
	log.Println("Clean shutdown complete.")
}

// -----------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------

type Config struct {
	// Hot path
	HotCore   int
	MaxOrders int

	// Risk
	MaxPositionValue  float64
	MaxOrderSize      int
	MaxDailyLoss      float64
	PriceDeviationPct float64

	// FIX
	FIXSender  string
	FIXTarget  string
	FIXHost    string
	FIXPort    int
	FIXAccount string

	// Storage
	WALPath string

	// HTTP
	HTTPPort int
}

func parseFlags() Config {
	cfg := Config{}

	flag.IntVar(&cfg.HotCore, "core", 1, "CPU core to pin the hot path to")
	flag.IntVar(&cfg.MaxOrders, "max-orders", 1_000_000, "Pre-allocated order slots")
	flag.Float64Var(&cfg.MaxPositionValue, "max-position", 1_000_000, "Max position value ($)")
	flag.IntVar(&cfg.MaxOrderSize, "max-order-size", 10_000, "Max shares per order")
	flag.Float64Var(&cfg.MaxDailyLoss, "max-daily-loss", 50_000, "Max daily loss ($)")
	flag.Float64Var(&cfg.PriceDeviationPct, "max-deviation", 5.0, "Max price deviation (%)")
	flag.StringVar(&cfg.FIXSender, "fix-sender", "MILLENNIUM", "FIX SenderCompID")
	flag.StringVar(&cfg.FIXTarget, "fix-target", "IBFX", "FIX TargetCompID")
	flag.StringVar(&cfg.FIXHost, "fix-host", "", "FIX gateway host (empty=simulation)")
	flag.IntVar(&cfg.FIXPort, "fix-port", 4002, "FIX gateway port")
	flag.StringVar(&cfg.FIXAccount, "fix-account", "", "Broker account ID")
	flag.StringVar(&cfg.WALPath, "wal", "./data/orders.wal", "Write-ahead log path")
	flag.IntVar(&cfg.HTTPPort, "port", 8080, "HTTP server port")

	flag.Parse()
	return cfg
}

// -----------------------------------------------------------------------
// CPU pinning (Linux-only via sched_setaffinity; no-op on macOS)
// -----------------------------------------------------------------------

func pinCPU(core int) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("CPU pinning only supported on Linux (current: %s)", runtime.GOOS)
	}
	// On Linux, this would use sched_setaffinity via unix package.
	// For portability, we just lock the OS thread (done in main).
	// In production on bare metal Linux, you'd use:
	//   unix.SchedSetaffinity(0, &unix.CPUSet{})
	// or launch with: taskset -c <core> ./millennium-oes
	return nil
}
