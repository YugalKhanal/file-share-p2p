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

		parts := strings.Split(message, ":")
		if len(parts) != 2 {
			log.Printf("Invalid message format (expected 2 parts separated by colon): %s", message)

			// Try to extract the remote port for a response
			if strings.HasPrefix(message, "PUNCH:") {
				// Special handling for potentially malformed PUNCH messages
				remoteParts := strings.Split(message, "PUNCH:")
				if len(remoteParts) > 1 && remoteParts[1] != "" {
					// Try to parse a port from the message
					portStr := ""
					if strings.Contains(remoteParts[1], ":") {
						hostPort := strings.Split(remoteParts[1], ":")
						if len(hostPort) > 1 {
							portStr = hostPort[len(hostPort)-1]
						}
					} else {
						// The message might just contain the port number
						portStr = remoteParts[1]
					}

					if portStr != "" {
						port, err := strconv.Atoi(portStr)
						if err == nil {
							// Send acknowledgment using the extracted port
							ackAddr := &net.UDPAddr{
								IP:   remoteAddr.IP,
								Port: port,
							}
							ackMsg := fmt.Sprintf("ACK:%s", t.ListenAddr)
							_, err = t.udpConn.WriteToUDP([]byte(ackMsg), ackAddr)
							if err != nil {
								log.Printf("Failed to send ACK to %s: %v", ackAddr, err)
							} else {
								log.Printf("Sent ACK to %s (recovered from malformed message)", ackAddr)
							}
						}
					}
				}
			}
			continue
		}

		messageType, tcpAddr := parts[0], parts[1]
		switch messageType {
		case "PUNCH":
			log.Printf("Received PUNCH from %s for TCP address %s", remoteAddr, tcpAddr)
			// Check if tcpAddr is valid
			if tcpAddr == "" {
				log.Printf("Empty TCP address in punch message")
				continue
			}
			// Extract port from punch message address
			_, portStr, err := net.SplitHostPort(tcpAddr)
			if err != nil {
				log.Printf("Invalid TCP address in punch message: %v", err)

				// Try to use the source port as a fallback
				portStr = strconv.Itoa(remoteAddr.Port)
				log.Printf("Using source port as fallback: %s", portStr)
			}

			punchPort, err := strconv.Atoi(portStr)
			if err != nil {
				log.Printf("Invalid port in punch message: %v", err)
				continue
			}

			// Send acknowledgment using the port from the message or fallback
			ackAddr := &net.UDPAddr{
				IP:   remoteAddr.IP,
				Port: punchPort,
			}

			ackMsg := fmt.Sprintf("ACK:%s", t.ListenAddr)
			_, err = t.udpConn.WriteToUDP([]byte(ackMsg), ackAddr)
			if err != nil {
				log.Printf("Failed to send ACK to %s: %v", ackAddr, err)
				continue
			}
			log.Printf("Sent ACK to %s", ackAddr)

			// Try TCP connection after small delay
			go func() {
				time.Sleep(100 * time.Millisecond)

				// Create a TCP address for connection attempt
				tcpTargetAddr := fmt.Sprintf("%s:%d", remoteAddr.IP.String(), punchPort)
				log.Printf("Attempting TCP connection to %s", tcpTargetAddr)

				if conn, err := net.DialTimeout("tcp", tcpTargetAddr, 5*time.Second); err == nil {
					log.Printf("Successfully established TCP connection to %s", tcpTargetAddr)
					go t.handleConn(conn, true)
				} else {
					log.Printf("Failed to establish TCP connection to %s: %v", tcpTargetAddr, err)
				}
			}()

		case "ACK":
			log.Printf("Received ACK from %s for TCP address %s", remoteAddr, tcpAddr)
			select {
			case t.connectedCh <- tcpAddr:
				log.Printf("Notified of successful connection to %s", tcpAddr)
			default:
				log.Printf("Failed to notify of connection to %s (channel full)", tcpAddr)
			}
		}
	}
}
