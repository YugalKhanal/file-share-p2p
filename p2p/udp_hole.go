package p2p

import (
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

func (t *TCPTransport) setupUDPListener() error {
	log.Printf("Setting up UDP listener on port %d", t.getPort())

	// Try binding to specific addresses
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

		// Set UDP socket options
		conn.SetReadBuffer(1024 * 1024)  // 1MB read buffer
		conn.SetWriteBuffer(1024 * 1024) // 1MB write buffer

		t.udpConn = conn
		log.Printf("UDP listener established successfully on %s", t.udpConn.LocalAddr())

		go t.handleUDPMessages()
		return nil
	}

	return fmt.Errorf("failed to setup UDP listener: %v", lastErr)
}

func (t *TCPTransport) handleUDPMessages() {
	log.Printf("Starting UDP message handler")
	buf := make([]byte, 1024)

	for {
		n, remoteAddr, err := t.udpConn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP read error: %v", err)
			continue
		}

		message := string(buf[:n])
		log.Printf("Received UDP message from %s: %s", remoteAddr, message)

		if strings.HasPrefix(message, "PUNCH") {
			tcpAddr := message[5:] // Skip "PUNCH" prefix
			log.Printf("Received PUNCH from %s for TCP address %s", remoteAddr, tcpAddr)

			// Send ACK
			ackMsg := fmt.Sprintf("ACK%s", t.ListenAddr)
			_, err = t.udpConn.WriteToUDP([]byte(ackMsg), remoteAddr)
			if err != nil {
				log.Printf("Failed to send ACK to %s: %v", remoteAddr, err)
				continue
			}
			log.Printf("Sent ACK to %s", remoteAddr)

			// Try both accepting and connecting simultaneously
			go func() {
				// Start a goroutine to try accepting
				acceptChan := make(chan net.Conn, 1)
				go func() {
					conn, err := t.listener.Accept()
					if err == nil {
						acceptChan <- conn
					}
				}()

				// Start a goroutine to try connecting
				dialChan := make(chan net.Conn, 1)
				go func() {
					conn, err := net.DialTimeout("tcp", tcpAddr, 3*time.Second)
					if err == nil {
						dialChan <- conn
					} else {
						log.Printf("Failed to establish TCP connection to %s: %v", tcpAddr, err)
					}
				}()

				// Wait for either connection to succeed
				select {
				case conn := <-acceptChan:
					log.Printf("Accepted TCP connection from %s", tcpAddr)
					go t.handleConn(conn, false)
				case conn := <-dialChan:
					log.Printf("Established outbound TCP connection to %s", tcpAddr)
					go t.handleConn(conn, true)
				case <-time.After(5 * time.Second):
					log.Printf("TCP connection timeout for %s", tcpAddr)
				}
			}()

		} else if strings.HasPrefix(message, "ACK") {
			tcpAddr := message[3:] // Skip "ACK" prefix
			log.Printf("Received ACK from %s for TCP address %s", remoteAddr, tcpAddr)
			select {
			case t.connectedCh <- tcpAddr:
				log.Printf("Notified of successful connection to %s", tcpAddr)
			default:
				log.Printf("Failed to notify of connection to %s (channel full)", tcpAddr)
			}

			// Try both accepting and connecting simultaneously here too
			go func() {
				// Similar connection attempt code as above
				acceptChan := make(chan net.Conn, 1)
				go func() {
					conn, err := t.listener.Accept()
					if err == nil {
						acceptChan <- conn
					}
				}()

				dialChan := make(chan net.Conn, 1)
				go func() {
					conn, err := net.DialTimeout("tcp", tcpAddr, 3*time.Second)
					if err == nil {
						dialChan <- conn
					}
				}()

				select {
				case conn := <-acceptChan:
					log.Printf("Accepted TCP connection after ACK from %s", tcpAddr)
					go t.handleConn(conn, false)
				case conn := <-dialChan:
					log.Printf("Established outbound TCP connection after ACK to %s", tcpAddr)
					go t.handleConn(conn, true)
				case <-time.After(5 * time.Second):
					log.Printf("TCP connection timeout after ACK for %s", tcpAddr)
				}
			}()
		}
	}
}
