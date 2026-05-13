// ringbuffer.go — Lock-free Single-Producer Single-Consumer (SPSC) ring buffer.
//
// This is the backbone of the hot path. Orders flow through this ring
// from the HTTP gateway (producer) to the event loop (consumer) with
// ZERO locks, ZERO allocations, and ZERO syscalls.
//
// How it works:
//   - Pre-allocated fixed-size array (no GC pressure)
//   - Producer writes to `writePos` (atomic increment)
//   - Consumer reads from `readPos` (atomic increment)
//   - Cache-line padding prevents false sharing between cores
//
// This is the same pattern used by LMAX Disruptor (the architecture
// behind the London Stock Exchange matching engine).

package engine

import (
	"sync/atomic"
	"unsafe"
)

// CacheLineSize is 64 bytes on x86/ARM — padding prevents false sharing
const CacheLineSize = 64

// RingBuffer is a lock-free SPSC queue for order events.
// The producer (HTTP handler) and consumer (event loop) never contend.
type RingBuffer struct {
	// Write position — only modified by producer
	writePos atomic.Uint64
	_pad1    [CacheLineSize - unsafe.Sizeof(atomic.Uint64{})]byte

	// Read position — only modified by consumer
	readPos atomic.Uint64
	_pad2   [CacheLineSize - unsafe.Sizeof(atomic.Uint64{})]byte

	// Buffer
	mask uint64 // size - 1 (size must be power of 2)
	buf  []Event
}

// Event is what flows through the ring buffer.
// Fixed size, no pointers (avoids GC scanning).
type Event struct {
	Type      EventType
	OrderIdx  uint32  // index into the order store
	Timestamp int64   // unix nanoseconds
	// Inline payload — no heap allocation
	Symbol    [8]byte // ticker symbol, null-padded
	Strategy  [8]byte // strategy ID, null-padded
	Side      Side
	OrdType   OrderType
	TIF       TimeInForce
	Qty       int32
	Price     int64 // price in microdollars (price * 1_000_000) — avoids float
	StopPrice int64
	TrailVal  int64
	TrailType TrailType
}

type EventType uint8

const (
	EventSubmit  EventType = iota + 1
	EventCancel
	EventReplace
	EventFill
	EventKillSwitch
)

// NewRingBuffer creates a ring buffer with the given capacity (must be power of 2).
func NewRingBuffer(size int) *RingBuffer {
	if size&(size-1) != 0 {
		panic("ring buffer size must be power of 2")
	}
	return &RingBuffer{
		mask: uint64(size - 1),
		buf:  make([]Event, size),
	}
}

// TryPublish attempts to write an event. Returns false if the buffer is full.
// Called by the producer (HTTP handler). Never blocks.
func (r *RingBuffer) TryPublish(e Event) bool {
	wp := r.writePos.Load()
	rp := r.readPos.Load()

	// Full check: if write has lapped read by one full cycle
	if wp-rp > r.mask {
		return false // buffer full — back-pressure
	}

	r.buf[wp&r.mask] = e
	r.writePos.Store(wp + 1)
	return true
}

// TryConsume attempts to read an event. Returns false if empty.
// Called by the consumer (event loop). Never blocks.
func (r *RingBuffer) TryConsume() (Event, bool) {
	rp := r.readPos.Load()
	wp := r.writePos.Load()

	if rp >= wp {
		return Event{}, false // empty
	}

	e := r.buf[rp&r.mask]
	r.readPos.Store(rp + 1)
	return e, true
}

// Len returns the number of unconsumed events.
func (r *RingBuffer) Len() int {
	return int(r.writePos.Load() - r.readPos.Load())
}

// Cap returns the buffer capacity.
func (r *RingBuffer) Cap() int {
	return int(r.mask + 1)
}
