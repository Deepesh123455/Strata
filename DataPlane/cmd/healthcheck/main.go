// Command healthcheck is a tiny, dependency-free liveness probe for the
// Powerhouse Cache. It is baked into the distroless production image, which has
// no shell, curl, nc, or redis-cli — so a normal shell-based HEALTHCHECK is
// impossible. This binary opens a TCP connection, speaks one RESP command, and
// verifies the server answers with a well-formed RESP reply.
//
// Exit code 0 = healthy, non-zero = unhealthy, which is exactly the contract
// Docker HEALTHCHECK, ECS container health checks, and Kubernetes exec probes
// expect. Keep it allocation-cheap and free of third-party imports so it adds
// no attack surface and no go.sum entries to the build.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "host:port of the cache to probe")
	timeout := flag.Duration("timeout", 2*time.Second, "overall probe timeout")
	flag.Parse()

	if err := probe(*addr, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: UNHEALTHY: %v\n", err)
		os.Exit(1)
	}
}

// probe dials addr, sends a RESP PING, and confirms the server replies with a
// syntactically valid RESP frame within the timeout.
//
// We accept "+PONG" (once a PING command exists) OR any valid RESP type byte
// (+ simple string, - error, : integer, $ bulk, * array). An answer of ANY
// shape proves the full edge path is alive: the accept loop handed off the
// connection, the parser read our command, the dispatcher ran, and the
// buffered writer flushed a reply. The only states we treat as unhealthy are
// "cannot connect" and "no / non-RESP reply before the deadline" — i.e. the
// process is down, wedged, or not speaking the protocol. This keeps the probe
// correct even as the command set evolves.
func probe(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}

	// RESP-encoded "PING": *1\r\n$4\r\nPING\r\n
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if n == 0 {
		return errors.New("empty reply")
	}

	switch buf[0] {
	case '+', '-', ':', '$', '*':
		return nil // a valid RESP reply of any kind ⇒ the server is serving
	default:
		return fmt.Errorf("unexpected first reply byte %q (not RESP)", buf[0])
	}
}
