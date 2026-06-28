// Package wal implements a single, totally-ordered Write-Ahead Log.
//
// On a single node this gives durability: every mutation is appended here
// before the client is told it succeeded (see the applier package), so a crash
// or restart can rebuild the in-memory keyspace by replaying this file.
//
// The log is deliberately designed as ONE global, sequence-numbered stream
// rather than one log per shard. A single ordered stream is what distributed
// consensus (replication / Raft) needs later: a follower can tail this file
// from a known sequence number, and the same record framing becomes the
// replication entry. Per-shard logs would parallelize writes but destroy the
// total ordering, so we keep the writer off the hot path instead (the 32-shard
// concurrency still applies to reads and in-memory applies).
//
// Record framing (big-endian):
//
//	[ magic u8 ][ seq u64 ][ crc32c u32 ][ payloadLen u32 ][ payload ... ]
//
//   - seq        — monotonic sequence number, the future replication offset.
//   - crc32c     — Castagnoli CRC over the payload; detects torn/partial
//                  writes left behind by a crash mid-append.
//   - payload    — a RESP-encoded canonical command (reuses the resp parser
//                  on replay).
package wal

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/logger"
)

const (
	recordMagic = 0xA7
	headerSize  = 1 + 8 + 4 + 4 // magic + seq + crc + len
	// maxPayload caps a single record so a corrupted length field can't make
	// recovery attempt a multi-gigabyte allocation. 512MB mirrors the Redis
	// maximum value size.
	maxPayload = 512 << 20
)

// castagnoli is the CRC table shared by the writer and the replayer.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// SyncPolicy controls how aggressively buffered data is flushed to stable storage.
type SyncPolicy int

const (
	// SyncEverySec flushes + fsyncs roughly once per second (Redis default).
	// At most ~1s of acknowledged writes can be lost on a crash; throughput is
	// high because fsync is amortized across many appends. This is the default.
	SyncEverySec SyncPolicy = iota
	// SyncAlways fsyncs on every append. Maximum durability, lowest throughput.
	SyncAlways
	// SyncNo never fsyncs explicitly; durability is left to the OS page cache.
	SyncNo
)

// Log is an append-only, sequence-numbered write-ahead log.
type Log struct {
	// mu guards the buffered writer and the sequence counter. The critical
	// section is just a memory copy into the bufio buffer, so it is short even
	// under heavy concurrency; the expensive fsync happens on the ticker.
	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	seq    uint64
	policy SyncPolicy

	quit chan struct{}
	wg   sync.WaitGroup
}

// Open opens (creating if needed) the log at path for appending. startSeq is the
// last sequence number observed during recovery, so newly appended records
// continue the monotonic sequence.
func Open(path string, startSeq uint64, policy SyncPolicy) (*Log, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	l := &Log{
		f:      f,
		w:      bufio.NewWriterSize(f, 1<<20), // 1MB write buffer
		seq:    startSeq,
		policy: policy,
		quit:   make(chan struct{}),
	}

	if policy == SyncEverySec {
		l.wg.Add(1)
		go l.syncLoop()
	}

	return l, nil
}

// Append frames and writes a single payload, returning its sequence number.
// The payload is fully copied into the log's buffer before returning, so the
// caller may reuse or recycle the source slice immediately.
func (l *Log) Append(payload []byte) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	seq := l.seq

	var hdr [headerSize]byte
	hdr[0] = recordMagic
	binary.BigEndian.PutUint64(hdr[1:9], seq)
	binary.BigEndian.PutUint32(hdr[9:13], crc32.Checksum(payload, castagnoli))
	binary.BigEndian.PutUint32(hdr[13:17], uint32(len(payload)))

	if _, err := l.w.Write(hdr[:]); err != nil {
		return 0, err
	}
	if _, err := l.w.Write(payload); err != nil {
		return 0, err
	}

	if l.policy == SyncAlways {
		if err := l.w.Flush(); err != nil {
			return 0, err
		}
		if err := l.f.Sync(); err != nil {
			return 0, err
		}
	}

	return seq, nil
}

// syncLoop performs group-commit fsync on the SyncEverySec schedule.
func (l *Log) syncLoop() {
	defer l.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-l.quit:
			return
		case <-ticker.C:
			if err := l.flushAndSync(); err != nil {
				logger.Warn("wal fsync failed", "err", err)
			}
		}
	}
}

// flushAndSync drains the buffer to the OS and forces it to stable storage.
func (l *Log) flushAndSync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Flush(); err != nil {
		return err
	}
	return l.f.Sync()
}

// Close stops the background syncer, flushes everything durably, and closes the
// file. Safe to call exactly once during graceful shutdown.
func (l *Log) Close() error {
	if l.policy == SyncEverySec {
		close(l.quit)
		l.wg.Wait()
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Flush(); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	return l.f.Close()
}
