// Millennium OES — Bare-metal order entry system with investor dashboard.
//
// Modes:
//   ./oes                          → simulation mode with web UI
//   ./oes -headless                → CLI mode, no UI (pure engine)
//   ./oes -broker=alpaca           → live paper trading with Alpaca
//   ./oes -broker=fix              → institutional FIX to IBKR

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

	"github.com/millennium-oes/internal/broker/alpaca"
	"github.com/millennium-oes/internal/engine"
	"github.com/millennium-oes/internal/fix"
	"github.com/millennium-oes/internal/gateway"
	"github.com/millennium-oes/internal/risk"
	mlsignal "github.com/millennium-oes/internal/signal"
	"github.com/millennium-oes/internal/strategy"
	"github.com/millennium-oes/internal/wal"
)

func main() {
	cfg := parseFlags()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("Millennium OES starting (pid=%d)", os.Getpid())
	log.Printf("  Mode: %s | Broker: %s | Headless: %v", modeStr(cfg), cfg.Broker, cfg.Headless)
	log.Printf("  CPU cores: %d | Hot core: %d", runtime.NumCPU(), cfg.HotCore)

	runtime.LockOSThread()
	if err := pinCPU(cfg.HotCore); err != nil {
		log.Printf("  CPU pin: %v (non-fatal)", err)
	}

	// -----------------------------------------------------------------------
	// WAL
	// -----------------------------------------------------------------------
	walLog, err := wal.Open(cfg.WALPath)
	if err != nil {
		log.Fatalf("WAL open failed: %v", err)
	}
	defer walLog.Close()
	log.Printf("  WAL: %d entries recovered", walLog.Len())

	// -----------------------------------------------------------------------
	// Engine
	// -----------------------------------------------------------------------
	eng := engine.New(engine.Config{
		MaxOrders:         cfg.MaxOrders,
		MaxPositionValue:  cfg.MaxPositionValue,
		MaxOrderSize:      cfg.MaxOrderSize,
		MaxDailyLoss:      cfg.MaxDailyLoss,
		PriceDeviationPct: cfg.PriceDeviationPct,
		WAL:               walLog,
	})
	eng.ReplayWAL()

	// -----------------------------------------------------------------------
	// Risk Tracker (portfolio-level metrics)
	// -----------------------------------------------------------------------
	riskTracker := risk.NewTracker(cfg.InitialEquity)

	// -----------------------------------------------------------------------
	// Strategy Manager (multi-strategy support)
	// -----------------------------------------------------------------------
	stratMgr := strategy.NewManager()

	// -----------------------------------------------------------------------
	// ML Signal Model
	// -----------------------------------------------------------------------
	signalModel := mlsignal.DefaultModel()
	_ = signalModel // available for strategy use

	// -----------------------------------------------------------------------
	// Broker
	// -----------------------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var alpacaClient *alpaca.Client

	switch cfg.Broker {
	case "alpaca":
		log.Printf("  Broker: Alpaca (%s)", cfg.AlpacaBaseURL)
		alpacaClient = alpaca.New(alpaca.Config{
			APIKey:    cfg.AlpacaKey,
			APISecret: cfg.AlpacaSecret,
			BaseURL:   cfg.AlpacaBaseURL,
			DataURL:   cfg.AlpacaDataURL,
		})
		alpacaClient.SetEngine(eng)
		eng.SetBroker(alpacaClient)
		go alpacaClient.PollFills(ctx)

	case "fix":
		log.Printf("  Broker: FIX %s→%s @ %s:%d", cfg.FIXSender, cfg.FIXTarget, cfg.FIXHost, cfg.FIXPort)
		fixClient := fix.NewClient(fix.Config{
			SenderCompID: cfg.FIXSender,
			TargetCompID: cfg.FIXTarget,
			Host:         cfg.FIXHost,
			Port:         cfg.FIXPort,
			Account:      cfg.FIXAccount,
			Heartbeat:    30,
		})
		if err := fixClient.Connect(ctx); err != nil {
			log.Printf("  FIX connect failed: %v (falling back to simulation)", err)
		} else {
			eng.SetBroker(fixClient)
		}

	default:
		log.Println("  Broker: simulation (no connection)")
	}

	// -----------------------------------------------------------------------
	// Start engine event loop
	// -----------------------------------------------------------------------
	go eng.Run(ctx)

	// -----------------------------------------------------------------------
	// HTTP Gateway (unless headless)
	// -----------------------------------------------------------------------
	if !cfg.Headless {
		gw := gateway.New(gateway.Config{
			Engine:       eng,
			RiskTracker:  riskTracker,
			SignalModel:  signalModel,
			Strategies:   stratMgr,
			Alpaca:       alpacaClient,
			Port:         cfg.HTTPPort,
		})
		go gw.Start()
		log.Printf("  UI: http://localhost:%d", cfg.HTTPPort)
	} else {
		log.Println("  UI: disabled (headless mode)")
	}

	log.Println("Millennium OES ready.")

	// -----------------------------------------------------------------------
	// Wait for shutdown
	// -----------------------------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	cancel()
	eng.Stop()
	walLog.Sync()
	log.Println("Done.")
}

// -----------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------

type Config struct {
	Headless bool
	Broker   string // "alpaca", "fix", "" (simulation)

	HotCore   int
	MaxOrders int
	HTTPPort  int

	// Risk
	MaxPositionValue  float64
	MaxOrderSize      int
	MaxDailyLoss      float64
	PriceDeviationPct float64
	InitialEquity     float64

	// Alpaca
	AlpacaKey     string
	AlpacaSecret  string
	AlpacaBaseURL string
	AlpacaDataURL string

	// FIX
	FIXSender  string
	FIXTarget  string
	FIXHost    string
	FIXPort    int
	FIXAccount string

	// Storage
	WALPath string
}

func parseFlags() Config {
	cfg := Config{}

	flag.BoolVar(&cfg.Headless, "headless", false, "Run without web UI (CLI mode)")
	flag.StringVar(&cfg.Broker, "broker", "", "Broker: alpaca, fix, or empty for simulation")
	flag.IntVar(&cfg.HotCore, "core", 1, "CPU core for hot path")
	flag.IntVar(&cfg.MaxOrders, "max-orders", 1_000_000, "Pre-allocated order slots")
	flag.IntVar(&cfg.HTTPPort, "port", 8080, "HTTP port")
	flag.Float64Var(&cfg.MaxPositionValue, "max-position", 1_000_000, "Max position value ($)")
	flag.IntVar(&cfg.MaxOrderSize, "max-order-size", 10_000, "Max shares per order")
	flag.Float64Var(&cfg.MaxDailyLoss, "max-daily-loss", 50_000, "Max daily loss ($)")
	flag.Float64Var(&cfg.PriceDeviationPct, "max-deviation", 5.0, "Max price deviation (%)")
	flag.Float64Var(&cfg.InitialEquity, "equity", 100_000, "Initial portfolio equity ($)")

	flag.StringVar(&cfg.AlpacaKey, "alpaca-key", os.Getenv("ALPACA_API_KEY"), "Alpaca API key")
	flag.StringVar(&cfg.AlpacaSecret, "alpaca-secret", os.Getenv("ALPACA_API_SECRET"), "Alpaca API secret")
	flag.StringVar(&cfg.AlpacaBaseURL, "alpaca-url", "https://paper-api.alpaca.markets", "Alpaca base URL")
	flag.StringVar(&cfg.AlpacaDataURL, "alpaca-data-url", "https://data.alpaca.markets", "Alpaca data URL")

	flag.StringVar(&cfg.FIXSender, "fix-sender", "MILLENNIUM", "FIX SenderCompID")
	flag.StringVar(&cfg.FIXTarget, "fix-target", "IBFX", "FIX TargetCompID")
	flag.StringVar(&cfg.FIXHost, "fix-host", "", "FIX host")
	flag.IntVar(&cfg.FIXPort, "fix-port", 4002, "FIX port")
	flag.StringVar(&cfg.FIXAccount, "fix-account", "", "FIX account")
	flag.StringVar(&cfg.WALPath, "wal", "./data/orders.wal", "WAL path")

	flag.Parse()
	return cfg
}

func modeStr(cfg Config) string {
	if cfg.Headless {
		return "headless"
	}
	return "ui"
}

func pinCPU(core int) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("CPU pinning requires Linux (current: %s)", runtime.GOOS)
	}
	return nil
}
