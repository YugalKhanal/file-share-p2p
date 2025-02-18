package p2p

import (
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

func (t *TCPTransport) setupUDPListener() error {
	// Create UDP listener on same port as TCP
	udpAddr := &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: t.getPort(),
	}

	var err error
	t.udpConn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("UDP listen failed: %v", err)
	}

	// Start UDP message handler
	go t.handleUDPMessages()
	return nil
}

func (t *TCPTransport) handleUDPMessages() {
	buf := make([]byte, 1024)
	for {
		n, remoteAddr, err := t.udpConn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP read error: %v", err)
			continue
		}

		message := string(buf[:n])
		parts := strings.Split(message, ":")
		if len(parts) != 2 {
			continue
		}

		messageType, tcpAddr := parts[0], parts[1]
		switch messageType {
		case "PUNCH":
			// Send acknowledgment back
			ackMsg := fmt.Sprintf("ACK:%s", t.ListenAddr)
			t.udpConn.WriteToUDP([]byte(ackMsg), remoteAddr)

			// Try TCP connection after small delay
			go func() {
				time.Sleep(100 * time.Millisecond)
				if conn, err := net.DialTimeout("tcp", tcpAddr, 5*time.Second); err == nil {
					go t.handleConn(conn, true)
				}
			}()

		case "ACK":
			select {
			case t.connectedCh <- tcpAddr:
			default:
			}
		}
	}
}
