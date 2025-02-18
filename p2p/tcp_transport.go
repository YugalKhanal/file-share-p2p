package p2p

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// TCPPeer represents the remote node over a TCP established connection.
type TCPPeer struct {
	// The underlying connection of the peer. Which in this case
	// is a TCP connection.
	net.Conn
	// if we dial and retrieve a conn => outbound == true
	// if we accept and retrieve a conn => outbound == false
	outbound bool

	wg *sync.WaitGroup
}

const (
	heartbeatInterval = 10 * time.Second
	readTimeout       = 300 * time.Second
	writeTimeout      = 300 * time.Second
)

func NewTCPPeer(conn net.Conn, outbound bool) *TCPPeer {
	return &TCPPeer{
		Conn:     conn,
		outbound: outbound,
		wg:       &sync.WaitGroup{},
	}
}

func (p *TCPPeer) CloseStream() {
	p.wg.Done()
}

func (p *TCPPeer) Send(b []byte) error {
	if len(b) == 0 {
		return nil // Heartbeat message
	}

	p.SetWriteDeadline(time.Now().Add(writeTimeout))

	// Ensure message size is within limits
	if len(b) > 17*1024*1024 { // 17MB (16MB chunk + 1MB overhead)
		return fmt.Errorf("message too large: %d bytes", len(b))
	}

	// Write in a single atomic operation to prevent partial writes
	data := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(data[:4], uint32(len(b)))
	copy(data[4:], b)

	n, err := p.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write: %v", err)
	}
	if n != len(data) {
		return fmt.Errorf("incomplete write: wrote %d of %d bytes", n, len(data))
	}

	return nil
}

type TCPTransportOpts struct {
	ListenAddr    string
	HandshakeFunc HandshakeFunc
	Decoder       Decoder
	OnPeer        func(Peer) error
	TrackerAddr   string
}

type TCPTransport struct {
	TCPTransportOpts
	listener   net.Listener
	rpcch      chan RPC
	bufferSize int //adding buffer size for large downloads
	NATService *NATService
}

func NewTCPTransport(opts TCPTransportOpts) *TCPTransport {
	t := &TCPTransport{
		TCPTransportOpts: opts,
		rpcch:            make(chan RPC, 16384),
		bufferSize:       128 * 1024 * 1024,
	}

	// Initialize NAT service if tracker address is provided
	if opts.TrackerAddr != "" {
		natService, err := NewNATService(t, opts.TrackerAddr)
		if err != nil {
			log.Printf("Warning: Failed to initialize NAT service: %v", err)
		} else {
			t.NATService = natService
		}
	}

	return t
}

// Addr implements the Transport interface return the address
// the transport is accepting connections.
func (t *TCPTransport) Addr() string {
	return t.ListenAddr
}

// Consume implements the Tranport interface, which will return read-only channel
// for reading the incoming messages received from another peer in the network.
func (t *TCPTransport) Consume() <-chan RPC {
	return t.rpcch
}

// Close implements the Transport interface.
func (t *TCPTransport) Close() error {
	if t.NATService != nil {
		t.NATService.Close()
	}
	if t.listener != nil {
		return t.listener.Close()
	}
	return nil
}

func (t *TCPTransport) SetOnPeer(handler func(Peer) error) {
	t.OnPeer = handler
}

// Dial implements the Transport interface.
func (t *TCPTransport) Dial(addr string) error {
	if t.NATService != nil {
		// Attempt NAT traversal
		if err := t.NATService.InitiateConnection(addr); err != nil {
			log.Printf("NAT traversal failed for %s: %v", addr, err)
		}
	}

	// Get all possible addresses to try
	addresses := []string{addr}
	if t.NATService != nil {
		t.NATService.mu.RLock()
		if peer, ok := t.NATService.peers[addr]; ok {
			addresses = append(addresses,
				peer.PublicAddr.String(),
				peer.PrivateAddr.String())
		}
		t.NATService.mu.RUnlock()
	}

	// Try each address
	var lastErr error
	for _, address := range addresses {
		log.Printf("Attempting TCP connection to %s", address)
		conn, err := net.DialTimeout("tcp", address, 5*time.Second)
		if err == nil {
			go t.handleConn(conn, true)
			return nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("failed to establish connection to any address: %v", lastErr)
}

func (t *TCPTransport) ListenAndAccept() error {
	var err error

	t.listener, err = net.Listen("tcp", t.ListenAddr)
	if err != nil {
		return err
	}

	go t.startAcceptLoop()

	log.Printf("TCP transport listening on port: %s\n", t.ListenAddr)

	return nil
}

func (t *TCPTransport) startAcceptLoop() {
	for {
		conn, err := t.listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}

		if err != nil {
			fmt.Printf("TCP accept error: %s\n", err)
		}

		go t.handleConn(conn, false)
	}
}

// Add this function to tcp_transport.go in the p2p package
func NormalizeAddress(addr string) string {
	// Handle IPv6 localhost address
	if strings.HasPrefix(addr, "[::1]:") {
		portIndex := strings.LastIndex(addr, ":")
		if portIndex > 0 {
			port := addr[portIndex+1:]
			return fmt.Sprintf("localhost:%s", port)
		}
	}
	return addr
}

func (t *TCPTransport) handleConn(conn net.Conn, outbound bool) {
	var err error

	defer func() {
		if err != nil {
			log.Printf("Closing connection to %s due to error: %v",
				conn.RemoteAddr(), err)
		}
		conn.Close()
	}()

	// Set TCP keepalive
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(heartbeatInterval)
		// Set TCP buffer sizes
		tcpConn.SetReadBuffer(1024 * 1024)  // 1MB read buffer
		tcpConn.SetWriteBuffer(1024 * 1024) // 1MB write buffer
		// Enable TCP no delay
		tcpConn.SetNoDelay(true)
	}

	peer := NewTCPPeer(conn, outbound)
	log.Printf("New peer connection established: remote=%s outbound=%v",
		conn.RemoteAddr(), outbound)

	if err = t.HandshakeFunc(peer); err != nil {
		log.Printf("Handshake failed with peer %s: %v",
			conn.RemoteAddr(), err)
		return
	}

	normalizedAddr := NormalizeAddress(conn.RemoteAddr().String())

	if t.OnPeer != nil {
		if err = t.OnPeer(peer); err != nil {
			log.Printf("OnPeer callback failed for %s: %v",
				conn.RemoteAddr(), err)
			return
		}
	}

	// Start heartbeat goroutine with backoff retry
	heartbeatCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		failures := 0

		for {
			select {
			case <-heartbeatCh:
				return
			case <-ticker.C:
				if err := peer.Send([]byte{}); err != nil {
					failures++
					if failures > 3 {
						log.Printf("Too many heartbeat failures for %s, closing connection", conn.RemoteAddr())
						conn.Close()
						return
					}
					log.Printf("Heartbeat failed for %s (attempt %d/3): %v", conn.RemoteAddr(), failures, err)
				} else {
					failures = 0
				}
			}
		}
	}()

	// Message handling loop with error recovery
	retries := 0
	for {
		conn.SetReadDeadline(time.Now().Add(readTimeout))

		rpc := RPC{}
		err = t.Decoder.Decode(conn, &rpc)
		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("Connection closed by peer %s", conn.RemoteAddr())
				break
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				retries++
				if retries > 3 {
					log.Printf("Too many consecutive timeouts from %s", conn.RemoteAddr())
					break
				}
				continue
			}
			log.Printf("Error decoding message from %s: %v", conn.RemoteAddr(), err)
			break
		}
		retries = 0

		conn.SetWriteDeadline(time.Now().Add(writeTimeout))

		rpc.From = normalizedAddr
		select {
		case t.rpcch <- rpc:
			log.Printf("Successfully forwarded message from %s to RPC channel", conn.RemoteAddr())
		case <-time.After(time.Second):
			log.Printf("Warning: RPC channel full, dropping message from %s", conn.RemoteAddr())
		}
	}

	close(heartbeatCh)
}

// Update the Send method in TCPPeer
