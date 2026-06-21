package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
)

// Replay reads every intact record from the log at path in order, invoking
// apply for each one. It returns the highest sequence number successfully
// applied, which the caller passes to Open so newly appended records continue
// the sequence.
//
// Crash safety: a process killed mid-Append can leave a torn tail — a partial
// header, a truncated payload, or a record whose CRC no longer matches. Replay
// stops at the first such record, treats everything before it as the durable
// prefix, and TRUNCATES the file to that boundary so the next Open starts from
// a clean, valid state. A torn tail is normal after a crash, not an error.
//
// A missing file simply means "nothing to recover yet".
func Replay(path string, apply func(seq uint64, payload []byte) error) (uint64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	r := bufio.NewReaderSize(f, 1<<20)

	var (
		lastSeq   uint64
		goodBytes int64 // byte length of the valid prefix
		hdr       [headerSize]byte
	)

	for {
		_, err := io.ReadFull(r, hdr[:])
		if errors.Is(err, io.EOF) {
			break // clean end: consumed exactly whole records
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			break // torn header → truncate at goodBytes
		}
		if err != nil {
			f.Close()
			return lastSeq, err
		}

		if hdr[0] != recordMagic {
			break // corruption → stop at the valid prefix
		}

		seq := binary.BigEndian.Uint64(hdr[1:9])
		crc := binary.BigEndian.Uint32(hdr[9:13])
		plen := binary.BigEndian.Uint32(hdr[13:17])
		if plen > maxPayload {
			break // implausible length → treat as corruption
		}

		payload := make([]byte, plen)
		if _, err := io.ReadFull(r, payload); err != nil {
			break // torn payload → truncate at goodBytes
		}
		if crc32.Checksum(payload, castagnoli) != crc {
			break // corrupt payload → truncate at goodBytes
		}

		if err := apply(seq, payload); err != nil {
			f.Close()
			return lastSeq, err
		}

		lastSeq = seq
		goodBytes += int64(headerSize) + int64(plen)
	}

	f.Close()

	// Drop any torn/corrupt tail. os.Truncate to the current size is a no-op,
	// so this is safe even on a perfectly clean log.
	if err := os.Truncate(path, goodBytes); err != nil {
		return lastSeq, err
	}

	return lastSeq, nil
}
