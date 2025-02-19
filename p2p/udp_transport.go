package p2p

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

type UDPPeer struct {
	addr        *net.UDPAddr
	conn        *net.UDPConn
	wg          *sync.WaitGroup
	readBuf     []byte
	readBufSize int
	mu          sync.Mutex
}

func NewUDPPeer(conn *net.UDPConn, addr *net.UDPAddr) *UDPPeer {
	return &UDPPeer{
		addr:    addr,
		conn:    conn,
		wg:      &sync.WaitGroup{},
		readBuf: make([]byte, 64*1024), // 64KB buffer
	}
}

// Implement Read method to satisfy net.Conn interface
func (p *UDPPeer) Read(b []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// If we have data in the buffer, return it
	if p.readBufSize > 0 {
		n = copy(b, p.readBuf[:p.readBufSize])
		p.readBufSize = 0
		return n, nil
	}

	// Read new data
	n, remoteAddr, err := p.conn.ReadFromUDP(b)
	if err != nil {
		return 0, err
	}

	// Verify the sender
	if remoteAddr.String() != p.addr.String() {
		return 0, fmt.Errorf("received data from unexpected peer: %s", remoteAddr)
	}

	return n, nil
}

// Implement Write method (required by net.Conn)
func (p *UDPPeer) Write(b []byte) (n int, err error) {
	return p.conn.WriteToUDP(b, p.addr)
}

func (p *UDPPeer) Send(b []byte) error {
	_, err := p.Write(b)
	return err
}

func (p *UDPPeer) Close() error {
	return nil // UDP is connectionless
}

func (p *UDPPeer) CloseStream() {
	p.wg.Done()
}

func (p *UDPPeer) LocalAddr() net.Addr {
	return p.conn.LocalAddr()
}

func (p *UDPPeer) RemoteAddr() net.Addr {
	return p.addr
}

// Required by net.Conn interface
func (p *UDPPeer) SetDeadline(t time.Time) error {
	return p.conn.SetDeadline(t)
}

func (p *UDPPeer) SetReadDeadline(t time.Time) error {
	return p.conn.SetReadDeadline(t)
}

func (p *UDPPeer) SetWriteDeadline(t time.Time) error {
	return p.conn.SetWriteDeadline(t)
}

func (t *TCPTransport) setupUDPListener() error {
	log.Printf("Setting up UDP listener on port %d", t.getPort())

	ips := []net.IP{net.IPv4zero, net.IPv6zero}
	var lastErr error

	for _, ip := range ips {
		udpAddr := &net.UDPAddr{
			IP:   ip,
			Port: t.getPort(),
		}

		conn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			lastErr = err
			continue
		}

		conn.SetReadBuffer(1024 * 1024)
		conn.SetWriteBuffer(1024 * 1024)

		t.udpConn = conn
		log.Printf("UDP listener established successfully on %s", t.udpConn.LocalAddr())

		go t.handleUDPMessages()
		return nil
	}

	return fmt.Errorf("failed to setup UDP listener: %v", lastErr)
}

func (t *TCPTransport) handleUDPMessages() {
	log.Printf("Starting UDP message handler")
	buf := make([]byte, 64*1024)

	for {
		n, remoteAddr, err := t.udpConn.ReadFromUDP(buf)
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("UDP read error: %v", err)
			}
			continue
		}

		message := string(buf[:n])

		if strings.HasPrefix(message, "PUNCH") {
			parts := strings.Split(message[5:], "|")
			if len(parts) != 2 {
				continue
			}

			peerListenAddr := parts[0]
			msgID := parts[1]

			log.Printf("Received PUNCH from %s for address %s (ID: %s)",
				remoteAddr, peerListenAddr, msgID)

			go func() {
				ackMsg := fmt.Sprintf("ACK%s|%s", t.ListenAddr, msgID)
				for i := 0; i < 3; i++ {
					t.udpConn.WriteToUDP([]byte(ackMsg), remoteAddr)
					log.Printf("Sent ACK %d/3 to %s", i+1, remoteAddr)
					time.Sleep(100 * time.Millisecond)
				}
			}()

			peer := NewUDPPeer(t.udpConn, remoteAddr)
			peer.wg.Add(1)

			t.mu.Lock()
			t.peers[remoteAddr.String()] = peer
			t.mu.Unlock()

			if t.OnPeer != nil {
				if err := t.OnPeer(peer); err != nil {
					log.Printf("OnPeer callback failed: %v", err)
					continue
				}
			}

			select {
			case t.connectedCh <- peerListenAddr:
				log.Printf("UDP hole punch established with %s", peerListenAddr)
			default:
				log.Printf("Channel full, but UDP hole punch established with %s", peerListenAddr)
			}

		} else if strings.HasPrefix(message, "ACK") {
			parts := strings.Split(message[3:], "|")
			if len(parts) != 2 {
				log.Printf("Invalid ACK format: %s", message)
				continue
			}

			log.Printf("Received ACK from %s", remoteAddr)

			// Notify of successful punch
			if handler, ok := t.punchingMap.Load("punchSuccess"); ok {
				if ch, ok := handler.(chan *net.UDPAddr); ok {
					select {
					case ch <- remoteAddr:
						log.Printf("Notified punch success for %s", remoteAddr)
					default:
						log.Printf("Punch success channel full for %s", remoteAddr)
					}
				}
			}
		} else {
			rpc := RPC{
				From:    remoteAddr.String(),
				Payload: buf[:n],
			}

			select {
			case t.rpcch <- rpc:
			default:
				log.Printf("RPC channel full, dropping message")
			}
		}
	}
}
