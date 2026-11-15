package engine

import (
	"path/filepath"
	"testing"

	"github.com/millennium-oes/internal/wal"
)

// Run with: make bench   (go test -bench=. -benchmem ./internal/engine/)

func testEvent() Event {
	return Event{
		Type:    EventSubmit,
		Symbol:  SymbolFromString("AAPL"),
		Side:    1,
		OrdType: OrdLimit,
		Qty:     100,
		Price:   PriceToMicros(175.50),
	}
}

// Ring buffer publish + consume on one goroutine (no contention).
func BenchmarkRingPublishConsume(b *testing.B) {
	r := NewRingBuffer(65536)
	ev := testEvent()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.TryPublish(ev)
		r.TryConsume()
	}
}

// processSubmit in simulation mode (no broker), no WAL.
func BenchmarkProcessSubmitNoWAL(b *testing.B) {
	benchProcessSubmit(b, nil)
}

// processSubmit in simulation mode with the WAL enabled (write(), no fsync).
// This is the path the 26µs figure measures, minus SSE fan-out cost.
func BenchmarkProcessSubmitWithWAL(b *testing.B) {
	w, err := wal.Open(filepath.Join(b.TempDir(), "bench.wal"))
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	benchProcessSubmit(b, w)
}

func benchProcessSubmit(b *testing.B, w *wal.Log) {
	e := New(Config{
		MaxOrders:    b.N + 2,
		MaxOrderSize: 1_000_000,
		MaxDailyLoss: 1_000_000,
		WAL:          w,
	})
	ev := testEvent()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev.OrderIdx = uint32(i + 1)
		e.processSubmit(ev)
	}
}
