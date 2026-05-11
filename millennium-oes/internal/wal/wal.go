// Package wal implements a Write-Ahead Log for order durability.
//
// Why a WAL instead of a database:
//   - Sequential writes only (fastest possible I/O pattern)
//   - No random reads on the hot path
//   - Append-only (no fragmentation, no compaction)
//   - Can be replayed on startup to restore state
//   - ~1μs per write (vs ~1ms for a database)
//
// Format:
//   Each entry is: [type:1][idx:4][timestamp:8][len:2][data:N][checksum:4]
//   Total header: 19 bytes + data
//
// This is the same pattern used by:
//   - Kafka (append-only commit log)
//   - LevelDB/RocksDB (WAL before memtable)
//   - Every database engine internally

package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

const (
	headerSize = 15 // type(1) + idx(4) + timestamp(8) + dataLen(2)
	crcSize    = 4
)

type EntryType uint8

const (
	EntryNewOrder EntryType = 1
	EntryCancel   EntryType = 2
	EntryFill     EntryType = 3
	EntryReplace  EntryType = 4
)

// Entry is a single WAL record
type Entry struct {
	Type      EntryType
	OrderIdx  uint32
	Timestamp int64
	Data      []byte
}

// Log is the write-ahead log
type Log struct {
	mu   sync.Mutex // only used for writes (off hot path if buffered)
	file *os.File
	path string
	size int64
	len  int // number of entries
}

// Open opens or creates a WAL file
func Open(path string) (*Log, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create WAL directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open WAL: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	l := &Log{
		file: f,
		path: path,
		size: info.Size(),
	}

	// Count entries for Len()
	entries := l.ReadAll()
	l.len = len(entries)

	return l, nil
}

// Append writes an entry to the WAL. This is the only I/O on the hot path.
// In production, you'd batch writes or use io_uring for async I/O.
func (l *Log) Append(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	dataLen := len(e.Data)
	buf := make([]byte, headerSize+dataLen+crcSize)

	// Header
	buf[0] = byte(e.Type)
	binary.LittleEndian.PutUint32(buf[1:5], e.OrderIdx)
	binary.LittleEndian.PutUint64(buf[5:13], uint64(e.Timestamp))
	binary.LittleEndian.PutUint16(buf[13:15], uint16(dataLen))

	// Data
	copy(buf[headerSize:], e.Data)

	// CRC32 checksum (detects corruption)
	crc := crc32.ChecksumIEEE(buf[:headerSize+dataLen])
	binary.LittleEndian.PutUint32(buf[headerSize+dataLen:], crc)

	_, err := l.file.Write(buf)
	if err != nil {
		return err
	}

	l.len++
	l.size += int64(len(buf))
	return nil
}

// ReadAll reads all entries from the WAL (used for replay on startup)
func (l *Log) ReadAll() []Entry {
	f, err := os.Open(l.path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []Entry
	header := make([]byte, headerSize)

	for {
		_, err := io.ReadFull(f, header)
		if err != nil {
			break
		}

		entryType := EntryType(header[0])
		orderIdx := binary.LittleEndian.Uint32(header[1:5])
		timestamp := int64(binary.LittleEndian.Uint64(header[5:13]))
		dataLen := binary.LittleEndian.Uint16(header[13:15])

		data := make([]byte, dataLen)
		if dataLen > 0 {
			if _, err := io.ReadFull(f, data); err != nil {
				break
			}
		}

		// Read and verify CRC
		crcBuf := make([]byte, crcSize)
		if _, err := io.ReadFull(f, crcBuf); err != nil {
			break
		}

		expectedCRC := binary.LittleEndian.Uint32(crcBuf)
		fullRecord := make([]byte, headerSize+int(dataLen))
		copy(fullRecord, header)
		copy(fullRecord[headerSize:], data)
		actualCRC := crc32.ChecksumIEEE(fullRecord)

		if expectedCRC != actualCRC {
			log.Printf("[WAL] CRC mismatch at entry %d — truncating", len(entries))
			break
		}

		entries = append(entries, Entry{
			Type:      entryType,
			OrderIdx:  orderIdx,
			Timestamp: timestamp,
			Data:      data,
		})
	}

	return entries
}

// Sync flushes the WAL to disk (fsync)
func (l *Log) Sync() error {
	return l.file.Sync()
}

// Close closes the WAL file
func (l *Log) Close() error {
	l.Sync()
	return l.file.Close()
}

// Len returns the number of entries
func (l *Log) Len() int {
	return l.len
}

// Truncate removes all entries (used for testing or manual reset)
func (l *Log) Truncate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	l.file.Seek(0, 0)
	l.size = 0
	l.len = 0
	return nil
}
