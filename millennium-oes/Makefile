# Millennium OES — Build targets

.PHONY: build run clean bench

# Build with optimizations
build:
	go build -ldflags="-s -w" -o ./bin/oes ./cmd/oes

# Build for Linux (bare metal deployment)
build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ./bin/oes-linux ./cmd/oes

# Run in simulation mode
run: build
	./bin/oes -port=8080

# Run with IBKR
run-fix: build
	./bin/oes -port=8080 -fix-host=127.0.0.1 -fix-port=4002 -fix-account=$(FIX_ACCOUNT)

# Clean
clean:
	rm -rf ./bin ./data

# Benchmark the ring buffer and engine
bench:
	go test -bench=. -benchmem ./internal/engine/
