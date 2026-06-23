package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/pool"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/resp"
)

// maxCommandSize caps how large a single pipelined command may grow before we
// give up and drop the client. It bounds the per-connection buffer so a
// hostile client can't make us allocate without limit, while still allowing
// large values (Redis permits values up to 512MB).
const maxCommandSize = 512 << 20

// writeBufSize is the per-connection response buffer. Responses for a whole
// pipelined batch accumulate here and are flushed in ONE syscall after the
// batch is drained, instead of one (or, for GET, three) syscalls per command.
const writeBufSize = 16 << 10 // 16KB

// Hot-path response literals, allocated once at startup instead of on every
// command. They are written but never mutated, so sharing them across all
// connection goroutines is safe.
var (
	respOK       = []byte("+OK\r\n")
	respNullBulk = []byte("$-1\r\n")
	respOne      = []byte(":1\r\n")
	respZero     = []byte(":0\r\n")
	respCRLF     = []byte("\r\n")
)

// upperInPlace upper-cases an ASCII byte slice in place (no allocation). The
// command/option slices alias the connection's read buffer and are fully
// consumed before the buffer is reused, so mutating them here is safe.
func upperInPlace(b []byte) {
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
}

// writeInt writes a RESP integer reply (":<n>\r\n") without allocating: the
// digits are formatted into a stack buffer that never escapes.
func writeInt(w *bufio.Writer, n int64) error {
	var b [24]byte
	buf := append(b[:0], ':')
	buf = strconv.AppendInt(buf, n, 10)
	buf = append(buf, '\r', '\n')
	_, err := w.Write(buf)
	return err
}

// handleConnection is the dedicated worker for a single client.
// It runs entirely in its own Goroutine, managed by the Netpoller.
func (s *Server) handleConnection(conn net.Conn) {
	// Register the connection for connection tracking and graceful shutdown
	s.registerConn(conn)

	// 1. Safety First: The Cleanup Stack
	defer s.wg.Done()        // Tell the main Server: "I am done, subtract 1 from active clients"
	defer s.deregisterConn(conn) // Deregister from active connection tracking
	defer conn.Close()        // Politely ask the OS to destroy the TCP socket

	// Isolate this client: a panic while parsing or executing one connection's
	// data must never take down the whole server (every other client's data
	// included). Recover here, log it, and let the deferred cleanup drop just
	// this connection.
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[PANIC] recovered on connection %s: %v\n", conn.RemoteAddr(), r)
		}
	}()

	// 2. Zero-Allocation Memory Lease
	bufferPtr := pool.Get()
	defer pool.Put(bufferPtr)

	buffer := *bufferPtr
	clientAddr := conn.RemoteAddr().String()

	// Buffered writer: responses are written here and flushed once per read
	// batch (see end of the loop), collapsing a pipeline of N commands from
	// up to 3N socket syscalls down to ~1.
	w := bufio.NewWriterSize(conn, writeBufSize)

	unread := 0

	// 3. The Continuous Read Loop
	for {
		// 4. The Hardware Watchdog (Protection against DoS / Dead clients)
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))

		// The buffer is full of unparsed bytes — a single command is larger than
		// the current buffer. Grow it (doubling) so large values are accepted,
		// up to maxCommandSize. The original pooled buffer is still referenced
		// by bufferPtr and returned to the pool on defer; the grown slice is a
		// separate allocation that simply gets GC'd.
		if unread == len(buffer) {
			if len(buffer) >= maxCommandSize {
				fmt.Printf("[NETWORK] Protocol error: command exceeds %d bytes from %s\n", maxCommandSize, clientAddr)
				return
			}
			newSize := len(buffer) * 2
			if newSize > maxCommandSize {
				newSize = maxCommandSize
			}
			grown := make([]byte, newSize)
			copy(grown, buffer[:unread])
			buffer = grown
		}

		// 5. The Syscall: Read bytes from the Kernel into the free part of our pooled buffer
		n, err := conn.Read(buffer[unread:])

		// 6. Handle Network Drops
		if err != nil {
			if errors.Is(err, io.EOF) {
				return // Polite Drop
			}
			// If connection was closed by Server.Stop(), return quietly
			select {
			case <-s.quit:
				return
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				fmt.Printf("[NETWORK] Disconnecting silent client: %s\n", clientAddr)
				return // Silent Drop
			}
			return // Violent Crash / Closed socket
		}

		limit := unread + n
		dataStream := buffer[:limit]
		cursor := 0

		// 7. Parse and execute as many complete commands as we can find
		for cursor < limit {
			cmdSlices, consumed, err := resp.Parse(dataStream[cursor:])
			if err != nil {
				if errors.Is(err, resp.ErrIncomplete) {
					// The packet is split, wait for the rest
					break
				}
				// Bad protocol data, drop the client
				fmt.Printf("[NETWORK] Protocol error from %s: %v\n", clientAddr, err)
				return
			}

			// Execute the parsed command (buffered; not yet on the wire)
			if err := s.executeCommand(w, cmdSlices); err != nil {
				// Client disconnected or connection error
				return
			}

			cursor += consumed
		}

		// Flush all buffered responses for this batch in one syscall before we
		// block on the next Read. A flush error means the client is gone.
		if err := w.Flush(); err != nil {
			return
		}

		// 8. Shift remaining unparsed bytes to the beginning of the buffer
		if cursor > 0 {
			if cursor < limit {
				copy(buffer, dataStream[cursor:limit])
				unread = limit - cursor
			} else {
				unread = 0
			}
		} else {
			unread = limit
		}
	}
}

// executeCommand dispatches a parsed RESP command to the correct handler.
// Responses are written to the buffered writer w; the caller flushes once per
// batch, so a single command never triggers its own socket syscall.
func (s *Server) executeCommand(w *bufio.Writer, cmdSlices [][]byte) error {
	if len(cmdSlices) == 0 || cmdSlices[0] == nil {
		return nil
	}

	// Upper-case the command verb in place and switch on it. `switch string(b)`
	// over a []byte is a compiler-optimized comparison that does not allocate.
	upperInPlace(cmdSlices[0])

	switch string(cmdSlices[0]) {

	// -----------------------------------------------------------------------
	// SET key value [EX seconds] [PX milliseconds]
	// -----------------------------------------------------------------------
	case "SET":
		if len(cmdSlices) < 3 {
			_, err := w.Write([]byte("-ERR wrong number of arguments for 'set' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := w.Write([]byte("-ERR key cannot be null\r\n"))
			return err
		}

		// Parse optional EX / PX arguments that follow the value.
		// Wire format example: SET mykey myval EX 60
		// cmdSlices:          [SET, mykey, myval, EX, 60]
		var ttl time.Duration
		i := 3
		for i < len(cmdSlices) {
			if cmdSlices[i] == nil {
				i++
				continue
			}
			upperInPlace(cmdSlices[i])
			switch string(cmdSlices[i]) {
			case "EX", "PX":
				if i+1 >= len(cmdSlices) || cmdSlices[i+1] == nil {
					_, err := w.Write([]byte("-ERR syntax error\r\n"))
					return err
				}
				val, parseErr := strconv.ParseInt(string(cmdSlices[i+1]), 10, 64)
				if parseErr != nil || val <= 0 {
					_, err := w.Write([]byte("-ERR invalid expire time in 'set' command\r\n"))
					return err
				}
				if string(cmdSlices[i]) == "EX" {
					ttl = time.Duration(val) * time.Second
				} else {
					ttl = time.Duration(val) * time.Millisecond
				}
				i += 2
			default:
				_, err := w.Write([]byte("-ERR syntax error\r\n"))
				return err
			}
		}

		if err := s.app.Set(cmdSlices[1], cmdSlices[2], ttl); err != nil {
			_, werr := w.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		_, err := w.Write(respOK)
		return err

	// -----------------------------------------------------------------------
	// DEL key
	// Removes a key. Returns :1 if the key handler ran (single-key form).
	// -----------------------------------------------------------------------
	case "DEL":
		if len(cmdSlices) < 2 {
			_, err := w.Write([]byte("-ERR wrong number of arguments for 'del' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := w.Write([]byte("-ERR key cannot be null\r\n"))
			return err
		}
		if err := s.app.Delete(cmdSlices[1]); err != nil {
			_, werr := w.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		_, err := w.Write(respOne)
		return err

	// -----------------------------------------------------------------------
	// GET key
	// -----------------------------------------------------------------------
	case "GET":
		if len(cmdSlices) < 2 {
			_, err := w.Write([]byte("-ERR wrong number of arguments for 'get' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := w.Write([]byte("-ERR key cannot be null\r\n"))
			return err
		}
		val, exists := s.app.Get(cmdSlices[1])
		if !exists {
			_, err := w.Write(respNullBulk)
			return err
		}
		if val == nil {
			_, err := w.Write(respNullBulk)
			return err
		}
		// RESP Bulk String: $<len>\r\n<data>\r\n. Build the header without
		// allocating: a stack buffer holds "$<len>\r\n".
		var hdr [24]byte
		h := append(hdr[:0], '$')
		h = strconv.AppendInt(h, int64(len(val)), 10)
		h = append(h, '\r', '\n')
		if _, err := w.Write(h); err != nil {
			return err
		}
		if _, err := w.Write(val); err != nil {
			return err
		}
		_, err := w.Write(respCRLF)
		return err

	// -----------------------------------------------------------------------
	// EXPIRE key seconds
	// Sets a TTL (in seconds) on an existing key.
	// Returns :1 if set, :0 if key does not exist.
	// -----------------------------------------------------------------------
	case "EXPIRE":
		if len(cmdSlices) < 3 {
			_, err := w.Write([]byte("-ERR wrong number of arguments for 'expire' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil || cmdSlices[2] == nil {
			_, err := w.Write([]byte("-ERR invalid arguments\r\n"))
			return err
		}
		secs, parseErr := strconv.ParseInt(string(cmdSlices[2]), 10, 64)
		if parseErr != nil || secs <= 0 {
			_, err := w.Write([]byte("-ERR invalid expire time in 'expire' command\r\n"))
			return err
		}
		ok, aerr := s.app.Expire(cmdSlices[1], time.Duration(secs)*time.Second)
		if aerr != nil {
			_, werr := w.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		if ok {
			_, err := w.Write(respOne)
			return err
		}
		_, err := w.Write(respZero)
		return err

	// -----------------------------------------------------------------------
	// PEXPIRE key milliseconds
	// Same as EXPIRE but accepts milliseconds.
	// -----------------------------------------------------------------------
	case "PEXPIRE":
		if len(cmdSlices) < 3 {
			_, err := w.Write([]byte("-ERR wrong number of arguments for 'pexpire' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil || cmdSlices[2] == nil {
			_, err := w.Write([]byte("-ERR invalid arguments\r\n"))
			return err
		}
		ms, parseErr := strconv.ParseInt(string(cmdSlices[2]), 10, 64)
		if parseErr != nil || ms <= 0 {
			_, err := w.Write([]byte("-ERR invalid expire time in 'pexpire' command\r\n"))
			return err
		}
		ok, aerr := s.app.Expire(cmdSlices[1], time.Duration(ms)*time.Millisecond)
		if aerr != nil {
			_, werr := w.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		if ok {
			_, err := w.Write(respOne)
			return err
		}
		_, err := w.Write(respZero)
		return err

	// -----------------------------------------------------------------------
	// TTL key
	// Returns remaining TTL in seconds.
	// -1 = no expiry, -2 = key does not exist.
	// -----------------------------------------------------------------------
	case "TTL":
		if len(cmdSlices) < 2 {
			_, err := w.Write([]byte("-ERR wrong number of arguments for 'ttl' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := w.Write([]byte("-ERR key cannot be null\r\n"))
			return err
		}
		remaining := s.app.TTL(cmdSlices[1])
		var secs int64
		switch remaining {
		case -2 * time.Second:
			secs = -2
		case -1 * time.Second:
			secs = -1
		default:
			// Round up: even 1ns remaining reports as 1 second (matches Redis behaviour)
			secs = int64(remaining.Seconds())
			if secs == 0 {
				secs = 1
			}
		}
		return writeInt(w, secs)

	// -----------------------------------------------------------------------
	// PTTL key
	// Returns remaining TTL in milliseconds.
	// -1 = no expiry, -2 = key does not exist.
	// -----------------------------------------------------------------------
	case "PTTL":
		if len(cmdSlices) < 2 {
			_, err := w.Write([]byte("-ERR wrong number of arguments for 'pttl' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := w.Write([]byte("-ERR key cannot be null\r\n"))
			return err
		}
		remaining := s.app.TTL(cmdSlices[1])
		var ms int64
		switch remaining {
		case -2 * time.Second:
			ms = -2
		case -1 * time.Second:
			ms = -1
		default:
			ms = remaining.Milliseconds()
			if ms == 0 {
				ms = 1
			}
		}
		return writeInt(w, ms)

	// -----------------------------------------------------------------------
	// PERSIST key
	// Removes the TTL from a key, making it live forever.
	// Returns :1 if TTL was removed, :0 if key had no TTL or does not exist.
	// -----------------------------------------------------------------------
	case "PERSIST":
		if len(cmdSlices) < 2 {
			_, err := w.Write([]byte("-ERR wrong number of arguments for 'persist' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := w.Write([]byte("-ERR key cannot be null\r\n"))
			return err
		}
		ok, aerr := s.app.Persist(cmdSlices[1])
		if aerr != nil {
			_, werr := w.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		if ok {
			_, err := w.Write(respOne)
			return err
		}
		_, err := w.Write(respZero)
		return err

	// -----------------------------------------------------------------------
	// Unknown command
	// -----------------------------------------------------------------------
	default:
		// Off the hot path; cmdSlices[0] holds the (now upper-cased) verb.
		_, err := fmt.Fprintf(w, "-ERR unknown command '%s'\r\n", cmdSlices[0])
		return err
	}
}