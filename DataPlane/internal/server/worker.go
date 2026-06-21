package server

import (
	"bytes"
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

// handleConnection is the dedicated worker for a single client.
// It runs entirely in its own Goroutine, managed by the Netpoller.
func (s *Server) handleConnection(conn net.Conn) {
	// Register the connection for connection tracking and graceful shutdown
	s.registerConn(conn)

	// 1. Safety First: The Cleanup Stack
	defer s.wg.Done()        // Tell the main Server: "I am done, subtract 1 from active clients"
	defer s.deregisterConn(conn) // Deregister from active connection tracking
	defer conn.Close()        // Politely ask the OS to destroy the TCP socket

	// 2. Zero-Allocation Memory Lease
	bufferPtr := pool.Get()
	defer pool.Put(bufferPtr)

	buffer := *bufferPtr
	clientAddr := conn.RemoteAddr().String()

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

			// Execute the parsed command
			if err := s.executeCommand(conn, cmdSlices); err != nil {
				// Client disconnected or connection error
				return
			}

			cursor += consumed
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
func (s *Server) executeCommand(conn net.Conn, cmdSlices [][]byte) error {
	if len(cmdSlices) == 0 || cmdSlices[0] == nil {
		return nil
	}

	commandName := string(bytes.ToUpper(cmdSlices[0]))

	switch commandName {

	// -----------------------------------------------------------------------
	// SET key value [EX seconds] [PX milliseconds]
	// -----------------------------------------------------------------------
	case "SET":
		if len(cmdSlices) < 3 {
			_, err := conn.Write([]byte("-ERR wrong number of arguments for 'set' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := conn.Write([]byte("-ERR key cannot be null\r\n"))
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
			option := string(bytes.ToUpper(cmdSlices[i]))
			switch option {
			case "EX", "PX":
				if i+1 >= len(cmdSlices) || cmdSlices[i+1] == nil {
					_, err := conn.Write([]byte("-ERR syntax error\r\n"))
					return err
				}
				val, parseErr := strconv.ParseInt(string(cmdSlices[i+1]), 10, 64)
				if parseErr != nil || val <= 0 {
					_, err := conn.Write([]byte("-ERR invalid expire time in 'set' command\r\n"))
					return err
				}
				if option == "EX" {
					ttl = time.Duration(val) * time.Second
				} else {
					ttl = time.Duration(val) * time.Millisecond
				}
				i += 2
			default:
				_, err := conn.Write([]byte("-ERR syntax error\r\n"))
				return err
			}
		}

		if err := s.app.Set(cmdSlices[1], cmdSlices[2], ttl); err != nil {
			_, werr := conn.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		_, err := conn.Write([]byte("+OK\r\n"))
		return err

	// -----------------------------------------------------------------------
	// DEL key
	// Removes a key. Returns :1 if the key handler ran (single-key form).
	// -----------------------------------------------------------------------
	case "DEL":
		if len(cmdSlices) < 2 {
			_, err := conn.Write([]byte("-ERR wrong number of arguments for 'del' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := conn.Write([]byte("-ERR key cannot be null\r\n"))
			return err
		}
		if err := s.app.Delete(cmdSlices[1]); err != nil {
			_, werr := conn.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		_, err := conn.Write([]byte(":1\r\n"))
		return err

	// -----------------------------------------------------------------------
	// GET key
	// -----------------------------------------------------------------------
	case "GET":
		if len(cmdSlices) < 2 {
			_, err := conn.Write([]byte("-ERR wrong number of arguments for 'get' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := conn.Write([]byte("-ERR key cannot be null\r\n"))
			return err
		}
		val, exists := s.app.Get(cmdSlices[1])
		if !exists {
			_, err := conn.Write([]byte("$-1\r\n"))
			return err
		}
		if val == nil {
			_, err := conn.Write([]byte("$-1\r\n"))
			return err
		}
		// RESP Bulk String: $<len>\r\n<data>\r\n
		header := fmt.Sprintf("$%d\r\n", len(val))
		if _, err := conn.Write([]byte(header)); err != nil {
			return err
		}
		if _, err := conn.Write(val); err != nil {
			return err
		}
		_, err := conn.Write([]byte("\r\n"))
		return err

	// -----------------------------------------------------------------------
	// EXPIRE key seconds
	// Sets a TTL (in seconds) on an existing key.
	// Returns :1 if set, :0 if key does not exist.
	// -----------------------------------------------------------------------
	case "EXPIRE":
		if len(cmdSlices) < 3 {
			_, err := conn.Write([]byte("-ERR wrong number of arguments for 'expire' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil || cmdSlices[2] == nil {
			_, err := conn.Write([]byte("-ERR invalid arguments\r\n"))
			return err
		}
		secs, parseErr := strconv.ParseInt(string(cmdSlices[2]), 10, 64)
		if parseErr != nil || secs <= 0 {
			_, err := conn.Write([]byte("-ERR invalid expire time in 'expire' command\r\n"))
			return err
		}
		ok, aerr := s.app.Expire(cmdSlices[1], time.Duration(secs)*time.Second)
		if aerr != nil {
			_, werr := conn.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		if ok {
			_, err := conn.Write([]byte(":1\r\n"))
			return err
		}
		_, err := conn.Write([]byte(":0\r\n"))
		return err

	// -----------------------------------------------------------------------
	// PEXPIRE key milliseconds
	// Same as EXPIRE but accepts milliseconds.
	// -----------------------------------------------------------------------
	case "PEXPIRE":
		if len(cmdSlices) < 3 {
			_, err := conn.Write([]byte("-ERR wrong number of arguments for 'pexpire' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil || cmdSlices[2] == nil {
			_, err := conn.Write([]byte("-ERR invalid arguments\r\n"))
			return err
		}
		ms, parseErr := strconv.ParseInt(string(cmdSlices[2]), 10, 64)
		if parseErr != nil || ms <= 0 {
			_, err := conn.Write([]byte("-ERR invalid expire time in 'pexpire' command\r\n"))
			return err
		}
		ok, aerr := s.app.Expire(cmdSlices[1], time.Duration(ms)*time.Millisecond)
		if aerr != nil {
			_, werr := conn.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		if ok {
			_, err := conn.Write([]byte(":1\r\n"))
			return err
		}
		_, err := conn.Write([]byte(":0\r\n"))
		return err

	// -----------------------------------------------------------------------
	// TTL key
	// Returns remaining TTL in seconds.
	// -1 = no expiry, -2 = key does not exist.
	// -----------------------------------------------------------------------
	case "TTL":
		if len(cmdSlices) < 2 {
			_, err := conn.Write([]byte("-ERR wrong number of arguments for 'ttl' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := conn.Write([]byte("-ERR key cannot be null\r\n"))
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
		_, err := conn.Write([]byte(fmt.Sprintf(":%d\r\n", secs)))
		return err

	// -----------------------------------------------------------------------
	// PTTL key
	// Returns remaining TTL in milliseconds.
	// -1 = no expiry, -2 = key does not exist.
	// -----------------------------------------------------------------------
	case "PTTL":
		if len(cmdSlices) < 2 {
			_, err := conn.Write([]byte("-ERR wrong number of arguments for 'pttl' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := conn.Write([]byte("-ERR key cannot be null\r\n"))
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
		_, err := conn.Write([]byte(fmt.Sprintf(":%d\r\n", ms)))
		return err

	// -----------------------------------------------------------------------
	// PERSIST key
	// Removes the TTL from a key, making it live forever.
	// Returns :1 if TTL was removed, :0 if key had no TTL or does not exist.
	// -----------------------------------------------------------------------
	case "PERSIST":
		if len(cmdSlices) < 2 {
			_, err := conn.Write([]byte("-ERR wrong number of arguments for 'persist' command\r\n"))
			return err
		}
		if cmdSlices[1] == nil {
			_, err := conn.Write([]byte("-ERR key cannot be null\r\n"))
			return err
		}
		ok, aerr := s.app.Persist(cmdSlices[1])
		if aerr != nil {
			_, werr := conn.Write([]byte("-ERR persistence error\r\n"))
			return werr
		}
		if ok {
			_, err := conn.Write([]byte(":1\r\n"))
			return err
		}
		_, err := conn.Write([]byte(":0\r\n"))
		return err

	// -----------------------------------------------------------------------
	// Unknown command
	// -----------------------------------------------------------------------
	default:
		_, err := conn.Write([]byte(fmt.Sprintf("-ERR unknown command '%s'\r\n", commandName)))
		return err
	}
}