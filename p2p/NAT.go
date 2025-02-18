package p2p

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
)

// NATType represents different NAT configurations
type NATType int

const (
	NATUnknown NATType = iota
	NATNone
	NATFull
	NATRestricted
	NATPortRestricted
	NATSymmetric
)

// STUN message types and constants
const (
	stunBindingRequest  = 0x0001
	stunBindingResponse = 0x0101
	stunMagicCookie     = 0x2112A442
)

type stunMessage struct {
	Type   uint16
	Length uint16
	Cookie uint32
	TxID   [12]byte
}

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

func (n *NATService) testSTUNServer(serverAddr string) error {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return fmt.Errorf("UDP listen failed: %v", err)
	}
	defer conn.Close()

	serverUDPAddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve STUN server: %v", err)
	}

	// Create transaction ID
	txID := make([]byte, 12)
	if _, err := rand.Read(txID); err != nil {
		return fmt.Errorf("failed to generate transaction ID: %v", err)
	}

	// Create STUN message
	msg := &stunMessage{
		Type:   stunBindingRequest,
		Length: 0,
		Cookie: stunMagicCookie,
	}
	copy(msg.TxID[:], txID)

	// Encode message
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.BigEndian, msg); err != nil {
		return fmt.Errorf("failed to encode STUN message: %v", err)
	}

	// Send request
	if _, err := conn.WriteToUDP(buf.Bytes(), serverUDPAddr); err != nil {
		return fmt.Errorf("failed to send STUN request: %v", err)
	}

	// Set read deadline
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Read response
	response := make([]byte, 1024)
	size, _, err := conn.ReadFromUDP(response)
	if err != nil {
		return fmt.Errorf("failed to read STUN response: %v", err)
	}

	// Parse response
	if size < 20 {
		return fmt.Errorf("STUN response too short")
	}

	// Verify response type
	respType := binary.BigEndian.Uint16(response[0:2])
	if respType != stunBindingResponse {
		return fmt.Errorf("unexpected STUN response type: %x", respType)
	}

	// Find XOR-MAPPED-ADDRESS attribute
	// Find XOR-MAPPED-ADDRESS attribute
	pos := 20
	for pos+4 <= size {
		attrType := binary.BigEndian.Uint16(response[pos : pos+2])
		attrLen := binary.BigEndian.Uint16(response[pos+2 : pos+4])

		if attrType == 0x0020 { // XOR-MAPPED-ADDRESS
			if pos+8 > size {
				return fmt.Errorf("truncated XOR-MAPPED-ADDRESS")
			}

			family := binary.BigEndian.Uint16(response[pos+4 : pos+6])
			if family != 0x01 {
				return fmt.Errorf("unsupported address family: %d", family)
			}

			// Extract and XOR port
			port := binary.BigEndian.Uint16(response[pos+6 : pos+8])
			port ^= uint16(stunMagicCookie >> 16)

			// Extract and XOR IP
			ip := make(net.IP, 4)
			cookie := make([]byte, 4)
			binary.BigEndian.PutUint32(cookie, stunMagicCookie)
			for i := 0; i < 4; i++ {
				ip[i] = response[pos+8+i] ^ cookie[i]
			}

			n.mutex.Lock()
			n.publicIP = ip.String()
			n.publicPort = int(port)
			n.mutex.Unlock()

			log.Printf("STUN detected public endpoint: %s:%d", ip.String(), port)
			return nil
		}

		pos += 4 + int(attrLen)
	}

	return fmt.Errorf("no XOR-MAPPED-ADDRESS found in STUN response")
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
	n.localPort = localPort

	// Try UPnP first
	if err := n.tryUPnP(localPort); err != nil {
		log.Printf("UPnP failed: %v, falling back to STUN", err)
	} else {
		log.Printf("UPnP succeeded, public endpoint: %s:%d", n.publicIP, n.publicPort)
		return nil
	}

	// Try STUN servers
	for _, server := range n.stunServers {
		log.Printf("Trying STUN server: %s", server)
		if err := n.testSTUNServer(server); err != nil {
			log.Printf("STUN server %s failed: %v", server, err)
			continue
		}
		return nil
	}

	// If both UPnP and STUN fail, use local address as fallback
	if n.publicIP == "" {
		localIP, err := getLocalIP()
		if err != nil {
			return fmt.Errorf("failed to get local IP: %v", err)
		}
		n.mutex.Lock()
		n.publicIP = localIP
		n.publicPort = localPort
		n.mutex.Unlock()
		log.Printf("NAT traversal failed, using local endpoint: %s:%d", localIP, localPort)
	}

	return nil
}

// Helper function to get local IP
func getLocalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					return ipnet.IP.String(), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no suitable local IP found")
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
