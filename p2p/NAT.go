package p2p

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
)

type NATType int

const (
	NATUnknown NATType = iota
	NATNone
	NATFull
	NATRestricted
	NATPortRestricted
	NATSymmetric
)

type NATService struct {
	publicIP        string
	publicPort      int
	localIP         string
	localPort       int
	natType         NATType
	stunServers     []string
	upnpEnabled     bool
	mutex           sync.RWMutex
	mappedPorts     map[int]bool
	mappedPortMutex sync.RWMutex
}

func NewNATService(stunServers []string) *NATService {
	if len(stunServers) == 0 {
		stunServers = []string{
			"stun.l.google.com:19302",
			"stun1.l.google.com:19302",
			"stun2.l.google.com:19302",
		}
	}

	return &NATService{
		stunServers: stunServers,
		upnpEnabled: true,
		mappedPorts: make(map[int]bool),
		natType:     NATUnknown,
	}
}

func (n *NATService) Initialize(localPort int) error {
	var err error

	// Try UPnP first
	if err = n.tryUPnP(localPort); err != nil {
		log.Printf("UPnP failed: %v, falling back to STUN", err)
	}

	// Try STUN if UPnP failed or we still don't have a public IP
	if n.publicIP == "" {
		if err = n.detectNATType(); err != nil {
			return fmt.Errorf("NAT detection failed: %v", err)
		}
	}

	n.localPort = localPort
	return nil
}

func (n *NATService) tryUPnP(port int) error {
	if !n.upnpEnabled {
		return fmt.Errorf("UPnP disabled")
	}

	clients, errors, err := internetgateway2.NewWANIPConnection1Clients()
	if err != nil {
		return err
	}

	for i, client := range clients {
		if errors[i] != nil {
			continue
		}

		// Get external IP
		ip, err := client.GetExternalIPAddress()
		if err != nil {
			continue
		}

		// Add port mapping
		err = client.AddPortMapping(
			"",
			uint16(port),
			"UDP",
			uint16(port),
			ip,
			true,
			"ForeverStore P2P",
			0,
		)
		if err == nil {
			n.mutex.Lock()
			n.publicIP = ip
			n.publicPort = port
			n.mutex.Unlock()

			n.mappedPortMutex.Lock()
			n.mappedPorts[port] = true
			n.mappedPortMutex.Unlock()

			return nil
		}
	}

	return fmt.Errorf("no working UPnP gateway found")
}

func (n *NATService) detectNATType() error {
	var lastErr error

	for _, server := range n.stunServers {
		if err := n.testSTUNServer(server); err != nil {
			lastErr = err
			continue
		}
		return nil
	}

	return fmt.Errorf("all STUN servers failed, last error: %v", lastErr)
}

func (n *NATService) testSTUNServer(serverAddr string) error {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	serverUDPAddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return err
	}

	// Send binding request
	if err := n.sendSTUNBindingRequest(conn, serverUDPAddr); err != nil {
		return err
	}

	// Wait for response with timeout
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	response := make([]byte, 1024)
	size, _, err := conn.ReadFromUDP(response)
	if err != nil {
		return err
	}

	// Parse STUN response
	mappedAddr, err := n.parseSTUNResponse(response[:size])
	if err != nil {
		return err
	}

	n.mutex.Lock()
	n.publicIP = mappedAddr.IP.String()
	n.publicPort = mappedAddr.Port
	n.mutex.Unlock()

	return nil
}

func (n *NATService) GetPublicAddress() (string, int) {
	n.mutex.RLock()
	defer n.mutex.RUnlock()
	return n.publicIP, n.publicPort
}

func (n *NATService) sendSTUNBindingRequest(conn *net.UDPConn, serverAddr *net.UDPAddr) error {
	// Simple STUN binding request
	request := []byte{0x00, 0x01, 0x00, 0x00, // Message type and length
		0x21, 0x12, 0xA4, 0x42, // Magic cookie
		0x00, 0x00, 0x00, 0x00, // Transaction ID
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00}

	_, err := conn.WriteToUDP(request, serverAddr)
	return err
}

func (t *TCPTransport) punchHole(remoteAddr string) error {
	// Parse the remote address
	udpAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return fmt.Errorf("invalid remote address: %v", err)
	}

	// Create local UDP socket if it doesn't exist
	if t.udpConn == nil {
		localAddr, err := net.ResolveUDPAddr("udp", t.ListenAddr)
		if err != nil {
			return fmt.Errorf("failed to resolve local address: %v", err)
		}

		t.udpConn, err = net.ListenUDP("udp", localAddr)
		if err != nil {
			return fmt.Errorf("failed to create UDP socket: %v", err)
		}
	}

	// Start hole punching
	_, punching := t.punchingMap.LoadOrStore(remoteAddr, true)
	if punching {
		return nil // Already attempting to punch hole
	}

	go func() {
		defer t.punchingMap.Delete(remoteAddr)

		// Send hole punching packets
		punchPacket := []byte("PUNCH")
		retries := 5
		for i := 0; i < retries; i++ {
			t.udpConn.WriteToUDP(punchPacket, udpAddr)
			time.Sleep(500 * time.Millisecond)
		}
	}()

	return nil
}

func (n *NATService) parseSTUNResponse(response []byte) (*net.UDPAddr, error) {
	if len(response) < 20 {
		return nil, fmt.Errorf("response too short")
	}

	// Skip header (20 bytes) and parse attributes
	pos := 20
	for pos+4 <= len(response) {
		attrType := uint16(response[pos])<<8 | uint16(response[pos+1])
		attrLen := uint16(response[pos+2])<<8 | uint16(response[pos+3])

		if attrType == 0x0001 { // XOR-MAPPED-ADDRESS
			if pos+8 > len(response) {
				return nil, fmt.Errorf("invalid XOR-MAPPED-ADDRESS")
			}

			port := uint16(response[pos+6])<<8 | uint16(response[pos+7])
			port ^= 0x2112 // XOR with magic cookie

			ip := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				ip[i] = response[pos+8+i] ^ response[4+i]
			}

			return &net.UDPAddr{
				IP:   ip,
				Port: int(port),
			}, nil
		}

		pos += 4 + int(attrLen)
	}

	return nil, fmt.Errorf("no XOR-MAPPED-ADDRESS found")
}

func (n *NATService) Cleanup() {
	if !n.upnpEnabled {
		return
	}

	clients, errors, err := internetgateway2.NewWANIPConnection1Clients()
	if err != nil {
		return
	}

	n.mappedPortMutex.RLock()
	ports := make([]int, 0, len(n.mappedPorts))
	for port := range n.mappedPorts {
		ports = append(ports, port)
	}
	n.mappedPortMutex.RUnlock()

	for i, client := range clients {
		if errors[i] != nil {
			continue
		}

		for _, port := range ports {
			client.DeletePortMapping("", uint16(port), "UDP")
		}
	}
}
