package server

import (
	"fmt"
	"net"
	"sync"

	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/applier"
)

// Server is the bare-metal TCP listener.
type Server struct {
	listenAddr string
	listener   net.Listener

	// The applier: durability boundary + 32-shard memory engine.
	app        *applier.Applier

	// wg (WaitGroup) tracks exactly how many clients are currently connected.
	// We use this to ensure we don't shut down the server while a client is talking.
	wg         sync.WaitGroup
	
	// A channel to signal that the server is shutting down
	quit       chan struct{}

	// conns tracks all active client connections so we can close them during Stop()
	connsMu    sync.Mutex
	conns      map[net.Conn]struct{}
}

// NewServer initializes the TCP wrapper.
func NewServer(listenAddr string, app *applier.Applier) *Server {
	return &Server{
		listenAddr: listenAddr,
		app:        app,
		quit:       make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
	}
}

func (s *Server) registerConn(conn net.Conn) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	s.conns[conn] = struct{}{}
}

func (s *Server) deregisterConn(conn net.Conn) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	delete(s.conns, conn)
}

// Start binds to the hardware port and begins accepting connections.
func (s *Server) Start() error {
	// 1. Launch the background TTL expiry worker.
	// It will sweep all shards every 100ms and is tied to s.quit so it
	// exits automatically when Stop() is called.
	s.app.StartExpiryWorker(s.quit)
	fmt.Println("[SYSTEM] TTL expiry worker started.")

	// 2. Tell the OS to open a TCP socket on our port
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to bind to port: %w", err)
	}
	s.listener = ln
	fmt.Printf("[SYSTEM] Powerhouse Cache listening on %s\n", s.listenAddr)


	// 2. The Accept Loop: This runs forever until the server is shut down.
	for {
		// Wait for a client to connect...
		conn, err := s.listener.Accept()
		if err != nil {
			// If the error is because we explicitly closed the listener, break the loop quietly.
			select {
			case <-s.quit:
				return nil
			default:
				fmt.Printf("[ERROR] Failed to accept connection: %v\n", err)
				continue
			}
		}

		// 3. A client connected!
		// Increment our WaitGroup so the server knows we have +1 active client.
		// Doing this here (synchronously, before the goroutine) is deliberate:
		// it guarantees every Add happens-before the accept loop returns, so
		// Stop()'s wg.Wait() can never race an Add against a zero counter.
		s.wg.Add(1)

		// 4. Hand the connection off to a dedicated Goroutine (Worker).
		// By putting 'go' in front, this function instantly detaches and runs in the background.
		go s.handleConnection(conn)
	}
}

// ServeConn handles a single client connection to completion on the calling
// goroutine, accounting for it in the graceful-shutdown WaitGroup. The accept
// loop inlines this same Add(1)+handle pairing; tests and alternative
// transports can call ServeConn directly with any net.Conn (e.g. a net.Pipe).
func (s *Server) ServeConn(conn net.Conn) {
	s.wg.Add(1)
	s.handleConnection(conn) // its deferred wg.Done() balances the Add above
}

// Stop initiates a graceful shutdown of the edge.
func (s *Server) Stop() {
	fmt.Println("\n[SYSTEM] Initiating graceful shutdown...")
	
	// 1. Signal all internal loops that we are quitting
	close(s.quit)
	
	// 2. Close the hardware listener. We stop accepting NEW connections.
	if s.listener != nil {
		s.listener.Close()
	}

	// Close all currently active client connections to unblock their read loops.
	s.connsMu.Lock()
	for conn := range s.conns {
		conn.Close()
	}
	s.connsMu.Unlock()

	// 3. Wait for all CURRENTLY active clients to finish their commands.
	// This prevents data corruption during deployments.
	fmt.Println("[SYSTEM] Waiting for active connections to drain...")
	s.wg.Wait()
	
	fmt.Println("[SYSTEM] Powerhouse Cache safely terminated.")
}