package tests

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/applier"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/engine"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/server"
)

// newConnPair wires a client end to a server worker over an in-memory pipe and
// returns the client side. The worker runs until the client closes the pipe.
func newConnPair(t *testing.T) (net.Conn, func()) {
	t.Helper()
	app := applier.New(engine.NewPowerhouseCache(0), nil) // nil WAL = pure in-memory
	s := server.NewServer(":0", app)

	client, srv := net.Pipe()
	done := make(chan struct{})
	go func() {
		s.ServeConn(srv)
		close(done)
	}()

	cleanup := func() {
		client.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server worker did not exit after client close")
		}
	}
	return client, cleanup
}

// readExactly blocks until exactly len(want) bytes arrive (or the deadline), so
// the assertion isn't fooled by a short read from the buffered flush.
func readExactly(t *testing.T, r *bufio.Reader, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("read: %v (got so far %q, want %q)", err, got, want)
	}
	if string(got) != want {
		t.Fatalf("response mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// TestServer_PipelinedBatchFlush sends a whole pipeline of commands in ONE write
// and verifies every response comes back correct and in order. This exercises
// the per-connection buffered writer and its once-per-batch flush.
func TestServer_PipelinedBatchFlush(t *testing.T) {
	client, cleanup := newConnPair(t)
	defer cleanup()
	client.SetDeadline(time.Now().Add(3 * time.Second))

	// SET foo bar ; GET foo ; TTL foo ; DEL foo ; GET foo ; DEL foo (again) — one write.
	// The second DEL targets a now-missing key and must report :0, not :1.
	batch := "*3\r\n$3\r\nSET\r\n$3\r\nfoo\r\n$3\r\nbar\r\n" +
		"*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n" +
		"*2\r\n$3\r\nTTL\r\n$3\r\nfoo\r\n" +
		"*2\r\n$3\r\nDEL\r\n$3\r\nfoo\r\n" +
		"*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n" +
		"*2\r\n$3\r\nDEL\r\n$3\r\nfoo\r\n"
	if _, err := client.Write([]byte(batch)); err != nil {
		t.Fatalf("write batch: %v", err)
	}

	r := bufio.NewReader(client)
	// +OK (set), $3 bar (get), :-1 (ttl, no expiry), :1 (del hit),
	// $-1 (get missing), :0 (del miss)
	readExactly(t, r, "+OK\r\n$3\r\nbar\r\n:-1\r\n:1\r\n$-1\r\n:0\r\n")
}

// TestServer_SetWithExpiryAndPersist checks the SET EX / TTL / PERSIST path
// across separate round-trips on one connection.
func TestServer_SetWithExpiryAndPersist(t *testing.T) {
	client, cleanup := newConnPair(t)
	defer cleanup()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReader(client)

	mustWrite(t, client, "*5\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n$2\r\nEX\r\n$3\r\n100\r\n")
	readExactly(t, r, "+OK\r\n")

	// TTL should be positive (<=100s).
	mustWrite(t, client, "*2\r\n$3\r\nTTL\r\n$1\r\nk\r\n")
	readTTLPositive(t, r)

	// PERSIST removes the TTL → :1.
	mustWrite(t, client, "*2\r\n$7\r\nPERSIST\r\n$1\r\nk\r\n")
	readExactly(t, r, ":1\r\n")

	// TTL now reports -1 (no expiry).
	mustWrite(t, client, "*2\r\n$3\r\nTTL\r\n$1\r\nk\r\n")
	readExactly(t, r, ":-1\r\n")
}

// TestServer_ProtocolErrorDropsClient verifies a malformed frame closes the
// connection (the worker returns, the pipe end goes to EOF).
func TestServer_ProtocolErrorDropsClient(t *testing.T) {
	client, cleanup := newConnPair(t)
	defer cleanup()
	client.SetDeadline(time.Now().Add(3 * time.Second))

	// Not a RESP array — protocol error, client must be dropped.
	mustWrite(t, client, "garbage not resp\r\n")

	r := bufio.NewReader(client)
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("expected the connection to be closed after a protocol error")
	}
}

// TestServer_OversizedBulkLenRejected is the regression guard for the parser
// integer-overflow crash: a near-MaxInt64 bulk length must be rejected (and the
// connection dropped) instead of panicking the server.
func TestServer_OversizedBulkLenRejected(t *testing.T) {
	client, cleanup := newConnPair(t)
	defer cleanup()
	client.SetDeadline(time.Now().Add(3 * time.Second))

	mustWrite(t, client, "*1\r\n$9223372036854775806\r\n")

	r := bufio.NewReader(client)
	// Worker should drop the connection (no panic, no hang) → read hits EOF.
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("expected connection drop on oversized bulk length")
	}
}

func mustWrite(t *testing.T, c net.Conn, s string) {
	t.Helper()
	if _, err := c.Write([]byte(s)); err != nil {
		t.Fatalf("write %q: %v", s, err)
	}
}

// readTTLPositive reads an integer reply ":<n>\r\n" and asserts n > 0.
func readTTLPositive(t *testing.T, r *bufio.Reader) {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read ttl: %v", err)
	}
	if len(line) < 4 || line[0] != ':' {
		t.Fatalf("expected integer reply, got %q", line)
	}
	if line[1] == '-' {
		t.Fatalf("expected positive TTL, got %q", line)
	}
}
