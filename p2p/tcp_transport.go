package p2p

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
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
}

// In tcp_transport.go

type TCPTransport struct {
	TCPTransportOpts
	listener    net.Listener
	udpConn     *net.UDPConn
	rpcch       chan RPC
	punchingMap sync.Map
	connectedCh chan string
}

func NewTCPTransport(opts TCPTransportOpts) *TCPTransport {
	return &TCPTransport{
		TCPTransportOpts: opts,
		rpcch:            make(chan RPC, 1024),
		connectedCh:      make(chan string, 1),
	}
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
	return t.listener.Close()
}

func (t *TCPTransport) SetOnPeer(handler func(Peer) error) {
	t.OnPeer = handler
}

// Dial implements the Transport interface.
func (t *TCPTransport) Dial(addr string) error {
	if t.udpConn == nil {
		log.Printf("Initializing UDP listener for hole punching")
		if err := t.setupUDPListener(); err != nil {
			return fmt.Errorf("UDP setup failed: %v", err)
		}
	}

	addrs := strings.Split(addr, "|")
	log.Printf("Attempting to connect to addresses: %v", addrs)

	errCh := make(chan error, len(addrs))
	doneCh := make(chan struct{})

	for _, address := range addrs {
		go func(targetAddr string) {
			host, portStr, err := net.SplitHostPort(targetAddr)
			if err != nil {
				errCh <- fmt.Errorf("invalid address %s: %v", targetAddr, err)
				return
			}

			port, err := strconv.Atoi(portStr)
			if err != nil {
				errCh <- fmt.Errorf("invalid port in address %s: %v", targetAddr, err)
				return
			}

			// Create UDP address using the same port as the TCP service
			udpAddr := &net.UDPAddr{
				IP:   net.ParseIP(host),
				Port: port, // Use the port from the target address
			}

			// Ensure the listen address is properly formatted with a host
			listenHost := "localhost"
			listenPort := t.getPort()
			listenAddr := fmt.Sprintf("%s:%d", listenHost, listenPort)

			// Create a properly formatted PUNCH message
			punchMsg := fmt.Sprintf("PUNCH:%s", listenAddr)
			log.Printf("Starting hole punching to %s (UDP: %s) with message: %s",
				targetAddr, udpAddr, punchMsg)

			// Send punch messages with exponential backoff
			for i := range 5 {
				n, err := t.udpConn.WriteToUDP([]byte(punchMsg), udpAddr)
				if err != nil {
					log.Printf("Failed to send punch message to %s: %v", udpAddr, err)
				} else {
					log.Printf("Sent punch message to %s (%d bytes)", udpAddr, n)
				}

				// Exponential backoff: 100ms, 200ms, 400ms, 800ms, 1600ms
				backoff := time.Duration(100*(1<<uint(i))) * time.Millisecond
				time.Sleep(backoff)
			}

			// Try direct TCP connection as fallback
			log.Printf("Attempting direct TCP connection to %s", targetAddr)
			if conn, err := net.DialTimeout("tcp", targetAddr, 5*time.Second); err == nil {
				log.Printf("Direct TCP connection successful to %s", targetAddr)
				go t.handleConn(conn, true)
				close(doneCh)
				return
			} else {
				log.Printf("Direct TCP connection failed to %s: %v", targetAddr, err)
			}
		}(address)
	}

	// Wait longer for hole punching to work
	log.Printf("Waiting for connection success or timeout")
	select {
	case <-doneCh:
		log.Printf("Connection established via direct TCP")
		return nil
	case tcpAddr := <-t.connectedCh:
		log.Printf("Connection established via hole punching to %s", tcpAddr)

		go func(addr string) {
			time.Sleep(200 * time.Millisecond) // Brief pause to let the other side prepare
			log.Printf("Attempting TCP connection after ACK to %s", addr)
			if conn, err := net.DialTimeout("tcp", addr, 5*time.Second); err == nil {
				log.Printf("Successfully established TCP connection after ACK to %s", addr)
				go t.handleConn(conn, true)
			} else {
				log.Printf("Failed to establish TCP connection after ACK to %s: %v", addr, err)
			}
		}(tcpAddr)

		return nil
	case err := <-errCh:
		log.Printf("Connection error: %v", err)
		return err
	case <-time.After(20 * time.Second): // Increased timeout
		log.Printf("Connection attempt timed out")
		return fmt.Errorf("connection timeout")
	}
}

func (t *TCPTransport) getPort() int {
	parts := strings.Split(t.ListenAddr, ":")
	if len(parts) != 2 {
		return 0
	}
	port, _ := strconv.Atoi(parts[1])
	return port
}

// formatTCPAddress formats the given IP and port as a TCP address string,
// correctly handling both IPv4 and IPv6 addresses
func formatTCPAddress(ip net.IP, port int) string {
	if ip.To4() != nil {
		// IPv4 address
		return fmt.Sprintf("%s:%d", ip.String(), port)
	}
	// IPv6 address
	return fmt.Sprintf("[%s]:%d", ip.String(), port)
}

func (t *TCPTransport) ListenAndAccept() error {
	var err error
	t.listener, err = net.Listen("tcp", t.ListenAddr)
	if err != nil {
		return err
	}

	// Setup UDP listener
	if err := t.setupUDPListener(); err != nil {
		t.listener.Close()
		return err
	}

	go t.startAcceptLoop()
	return nil
}

func (t *TCPTransport) startAcceptLoop() {
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			log.Printf("Accept error: %v", err)
			continue
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
