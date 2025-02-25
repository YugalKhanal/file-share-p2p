package p2p

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

func (t *TCPTransport) setupUDPListener() error {
	log.Printf("Setting up UDP listener on port %d", t.getPort())

	udpAddr := &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: t.getPort(),
	}

	var err error
	t.udpConn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("UDP listen failed: %v", err)
	}

	log.Printf("UDP listener established successfully on %s", t.udpConn.LocalAddr())
	go t.handleUDPMessages()
	return nil
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

		// Use a more flexible parsing approach, not just simple split
		if strings.HasPrefix(message, "PUNCH:") {
			// Extract the TCP address from the PUNCH message
			tcpAddr := strings.TrimPrefix(message, "PUNCH:")
			log.Printf("Received PUNCH from %s for TCP address %s", remoteAddr, tcpAddr)

			// Extract port from the address
			var punchPort int

			if tcpAddr == "" {
				log.Printf("Empty TCP address in punch message")
				continue
			}

			// Try to extract port from address
			_, portStr, err := net.SplitHostPort(tcpAddr)
			if err != nil {
				log.Printf("Invalid TCP address format in punch message: %v", err)
				// Fallback to using the remote port
				punchPort = remoteAddr.Port
				log.Printf("Using source port as fallback: %d", punchPort)
			} else {
				punchPort, err = strconv.Atoi(portStr)
				if err != nil {
					log.Printf("Invalid port in punch message: %v", err)
					continue
				}
			}

			// Send acknowledgment using the determined port
			ackAddr := &net.UDPAddr{
				IP:   remoteAddr.IP,
				Port: punchPort,
			}

			// Format the ACK message correctly
			ackMsg := fmt.Sprintf("ACK:%s", t.ListenAddr)
			log.Printf("Sending ACK message: %s to %s", ackMsg, ackAddr)

			_, err = t.udpConn.WriteToUDP([]byte(ackMsg), ackAddr)
			if err != nil {
				log.Printf("Failed to send ACK to %s: %v", ackAddr, err)
				continue
			}
			log.Printf("Sent ACK to %s", ackAddr)

			// Try TCP connection after small delay
			go func() {
				time.Sleep(100 * time.Millisecond)

				// Create a TCP address for connection attempt - Using formatTCPAddress here
				tcpTargetAddr := formatTCPAddress(remoteAddr.IP, punchPort)
				log.Printf("Attempting TCP connection to %s", tcpTargetAddr)

				if conn, err := net.DialTimeout("tcp", tcpTargetAddr, 5*time.Second); err == nil {
					log.Printf("Successfully established TCP connection to %s", tcpTargetAddr)
					go t.handleConn(conn, true)
				} else {
					log.Printf("Failed to establish TCP connection to %s: %v", tcpTargetAddr, err)
				}
			}()

		} else if strings.HasPrefix(message, "ACK:") {
			// Extract the TCP address from the ACK message
			tcpAddr := strings.TrimPrefix(message, "ACK:")
			log.Printf("Received ACK from %s for TCP address %s", remoteAddr, tcpAddr)

			select {
			case t.connectedCh <- tcpAddr:
				log.Printf("Notified of successful connection to %s", tcpAddr)
			default:
				log.Printf("Failed to notify of connection to %s (channel full)", tcpAddr)
			}
		} else {
			log.Printf("Received unknown message type: %s", message)
		}
	}
}
