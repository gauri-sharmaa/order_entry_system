package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/millennium-oes/internal/api"
	"github.com/millennium-oes/internal/broker"
	fixbroker "github.com/millennium-oes/internal/broker/fix"
	"github.com/millennium-oes/internal/order"
	"github.com/millennium-oes/internal/risk"
	"github.com/millennium-oes/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Starting Millennium OES...")

	cfg := loadConfig()

	// -----------------------------------------------------------------------
	// Stores
	// -----------------------------------------------------------------------
	redisStore, err := store.NewRedisStore(cfg.RedisAddr)
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	defer redisStore.Close()

	dynamoStore, err := store.NewDynamoStore(cfg.AWSRegion, cfg.DynamoTable)
	if err != nil {
		log.Fatalf("Failed to connect to DynamoDB: %v", err)
	}

	// -----------------------------------------------------------------------
	// Order + Risk engines
	// -----------------------------------------------------------------------
	engine := order.NewEngine(redisStore, dynamoStore)

	riskEngine := risk.NewEngine(risk.Config{
		MaxOrderSize:      cfg.MaxOrderSize,
		MaxPositionValue:  cfg.MaxPositionValue,
		MaxDailyLoss:      cfg.MaxDailyLoss,
		PriceDeviationPct: cfg.PriceDeviationPct,
	})

	// -----------------------------------------------------------------------
	// Broker — select via BROKER_TYPE env var
	//   "fix"    → QuickFIX/Go + IBKR Gateway (institutional, low latency)
	//   "alpaca" → Alpaca REST/WebSocket (default, paper trading)
	// -----------------------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var brokerClient broker.Client

	switch cfg.BrokerType {
	case "fix":
		log.Println("[BROKER] Using FIX 4.2 / Interactive Brokers")
		fixClient := fixbroker.NewClient(fixbroker.Config{
			SenderCompID:      cfg.FIXSenderCompID,
			TargetCompID:      cfg.FIXTargetCompID,
			Host:              cfg.FIXHost,
			Port:              cfg.FIXPort,
			HeartbeatInterval: 30,
			ResetOnLogon:      cfg.FIXResetOnLogon,
			FileStorePath:     cfg.FIXStorePath,
			Account:           cfg.FIXAccount,
		})
		if err := fixClient.Connect(ctx); err != nil {
			log.Fatalf("Failed to connect FIX session: %v", err)
		}
		brokerClient = fixClient

	default:
		log.Println("[BROKER] Using Alpaca (paper trading)")
		alpacaClient := broker.NewAlpacaClient(broker.Config{
			APIKey:    cfg.AlpacaAPIKey,
			APISecret: cfg.AlpacaAPISecret,
			BaseURL:   cfg.AlpacaBaseURL,
			StreamURL: cfg.AlpacaStreamURL,
		})
		if err := alpacaClient.Connect(ctx); err != nil {
			log.Fatalf("Failed to connect to Alpaca: %v", err)
		}
		brokerClient = alpacaClient
	}

	// Wire fill events into the order engine
	go engine.ListenForFills(ctx, brokerClient.FillEvents())

	// -----------------------------------------------------------------------
	// HTTP server
	// -----------------------------------------------------------------------
	router := api.NewRouter(api.Deps{
		Engine:     engine,
		RiskEngine: riskEngine,
		Broker:     brokerClient,
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("Server listening on :%s (broker: %s)", cfg.Port, cfg.BrokerType)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)
}

// -----------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------

type Config struct {
	Port      string
	RedisAddr string
	AWSRegion string
	DynamoTable string

	// Broker selection
	BrokerType string // "fix" or "alpaca"

	// Alpaca
	AlpacaAPIKey    string
	AlpacaAPISecret string
	AlpacaBaseURL   string
	AlpacaStreamURL string

	// FIX / IBKR
	FIXSenderCompID string
	FIXTargetCompID string
	FIXHost         string
	FIXPort         int
	FIXAccount      string
	FIXResetOnLogon bool
	FIXStorePath    string

	// Risk limits
	MaxOrderSize      int64
	MaxPositionValue  float64
	MaxDailyLoss      float64
	PriceDeviationPct float64
}

func loadConfig() Config {
	fixPort, _ := strconv.Atoi(getEnv("FIX_PORT", "4002")) // 4002 = IBKR paper

	return Config{
		Port:        getEnv("PORT", "8080"),
		RedisAddr:   getEnv("REDIS_ADDR", "localhost:6379"),
		AWSRegion:   getEnv("AWS_REGION", "us-east-1"),
		DynamoTable: getEnv("DYNAMO_TABLE", "millennium-orders"),

		BrokerType: getEnv("BROKER_TYPE", "alpaca"),

		AlpacaAPIKey:    getEnv("ALPACA_API_KEY", ""),
		AlpacaAPISecret: getEnv("ALPACA_API_SECRET", ""),
		AlpacaBaseURL:   getEnv("ALPACA_BASE_URL", "https://paper-api.alpaca.markets"),
		AlpacaStreamURL: getEnv("ALPACA_STREAM_URL", "wss://paper-api.alpaca.markets/stream"),

		FIXSenderCompID: getEnv("FIX_SENDER_COMP_ID", "MILLENNIUM"),
		FIXTargetCompID: getEnv("FIX_TARGET_COMP_ID", "IBFX"),
		FIXHost:         getEnv("FIX_HOST", "127.0.0.1"),
		FIXPort:         fixPort,
		FIXAccount:      getEnv("FIX_ACCOUNT", ""),
		FIXResetOnLogon: getEnv("FIX_RESET_ON_LOGON", "Y") == "Y",
		FIXStorePath:    getEnv("FIX_STORE_PATH", "./fix-store"),

		MaxOrderSize:      10000,
		MaxPositionValue:  1_000_000,
		MaxDailyLoss:      50_000,
		PriceDeviationPct: 5.0,
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
