package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	// "encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/anthdm/foreverstore/p2p"
	// "github.com/anthdm/foreverstore/shared"
)

type FileServerOpts struct {
	ListenAddr        string
	EncKey            []byte
	StorageRoot       string
	PathTransformFunc func(string) PathKey
	Transport         p2p.Transport
	TrackerAddr       string
	BootstrapNodes    []string
}

type FileServer struct {
	opts   FileServerOpts
	peers  map[string]p2p.Peer
	store  *Store
	quitch chan struct{}
	mu     sync.Mutex
}

func makeServer(listenAddr, bootstrapNode string) *FileServer {
	tcpOpts := p2p.TCPTransportOpts{
		ListenAddr:    listenAddr,
		HandshakeFunc: p2p.NOPHandshakeFunc,
		Decoder:       p2p.DefaultDecoder{},
	}
	transport := p2p.NewTCPTransport(tcpOpts)

	opts := FileServerOpts{
		ListenAddr: listenAddr,
		// ListenAddr:        "localhost:3000",
		EncKey:            newEncryptionKey(),
		StorageRoot:       "shared_files",
		PathTransformFunc: DefaultPathTransformFunc,
		Transport:         transport,
		BootstrapNodes:    []string{bootstrapNode},
	}

	server := &FileServer{
		opts:   opts,
		store:  NewStore(StoreOpts{Root: opts.StorageRoot, PathTransformFunc: opts.PathTransformFunc}),
		quitch: make(chan struct{}),
		peers:  make(map[string]p2p.Peer),
	}
	transport.SetOnPeer(server.onPeer)
	return server
}

// Fetches the public IP address of the server
func getPublicIP() (string, error) {
	resp, err := http.Get("http://checkip.amazonaws.com")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	ip, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(ip)), nil
}

// Fetches the local IP address of the server
func getLocalIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			return ipNet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no local IP found")
}

// Determines the peer's full address, combining the detected IP with the listening port
func getPeerAddress(port string) (string, error) {
	publicIP, err := getPublicIP()
	if err != nil {
		localIP, err := getLocalIP()
		if err != nil {
			return "", fmt.Errorf("failed to determine peer IP: %v", err)
		}
		return fmt.Sprintf("%s:%s", localIP, port), nil
	}
	return fmt.Sprintf("%s:%s", publicIP, port), nil
}

func (s *FileServer) Start() error {
	log.Printf("Starting server on %s", s.opts.ListenAddr)
	if err := s.opts.Transport.ListenAndAccept(); err != nil {
		return err
	}
	s.bootstrapNetwork()
	return nil
}

func (s *FileServer) onPeer(peer p2p.Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peers[peer.RemoteAddr().String()] = peer
	log.Printf("Connected to peer %s", peer.RemoteAddr())
	return nil
}

func (s *FileServer) ShareFile(filePath string) error {
	// fileID := generateID()
	fileID, err := s.generateFileID(filePath)
	if err != nil {
		return fmt.Errorf("failed to generate file ID: %v", err)
	}

	meta, err := s.store.ChunkAndStore(fileID, filePath)
	if err != nil {
		return err
	}

	if s.opts.TrackerAddr != "" {
		if err := announceToTracker(s.opts.TrackerAddr, fileID, s.opts.ListenAddr); err != nil {
			log.Printf("Failed to announce to tracker: %v", err)
		} else {
			log.Printf("Announced file %s to tracker at %s (peer: %s)", fileID, s.opts.TrackerAddr, s.opts.ListenAddr)
		}
	}

	fmt.Println("Peer address: ", s.opts.ListenAddr)
	fmt.Println("Tracker address: ", s.opts.TrackerAddr)
	log.Printf("File %s shared with ID %s and %d chunks", filePath, fileID, meta.NumChunks)
	return nil
}

func (s *FileServer) generateFileID(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha1.New() // or use sha256.New() for SHA-256
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *FileServer) DownloadFile(fileID string) error {
	log.Printf("Attempting to download file with ID: %s", fileID)

	// Check if metadata is available locally
	meta, err := s.store.GetMetadata(fileID)
	fmt.Println("got metadata", meta)
	if err != nil {
		log.Printf("Metadata not found locally for file ID %s. Attempting to fetch from peers...", fileID)

		// Attempt to fetch metadata from peers
		for _, peer := range s.peers {
			log.Printf("Requesting metadata from peer %s", peer.RemoteAddr())

			req := p2p.MessageMetadataRequest{FileID: fileID}
			var buf bytes.Buffer
			enc := gob.NewEncoder(&buf)
			if err := enc.Encode(req); err != nil {
				log.Printf("Failed to encode metadata request: %v", err)
				continue
			}

			if err := peer.Send(buf.Bytes()); err != nil {
				log.Printf("Failed to send metadata request to peer %s: %v", peer.RemoteAddr(), err)
				continue
			}

			// Read the response
			var resp p2p.MessageMetadataResponse
			dec := gob.NewDecoder(peer)
			if err := dec.Decode(&resp); err != nil {
				log.Printf("Failed to decode metadata response from peer %s: %v", peer.RemoteAddr(), err)
				continue
			}

			// Save the received metadata locally
			if err := s.store.saveMetadata(&resp.Metadata); err != nil {
				log.Printf("Failed to save received metadata: %v", err)
				continue
			}

			log.Printf("Successfully fetched metadata for file ID %s from peer %s", fileID, peer.RemoteAddr())
			meta = &resp.Metadata
			break
		}

		if meta == nil {
			return fmt.Errorf("failed to fetch metadata for file ID %s from peers", fileID)
		}
	}

	// Construct the output file name with the correct extension
	outputFileName := fmt.Sprintf("downloaded_%s%s", fileID, meta.FileExtension)
	outputFile, err := os.Create(outputFileName)
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer outputFile.Close()

	// Loop through each chunk to download and write to the output file
	for chunk := 0; chunk < meta.NumChunks; chunk++ {
		var downloaded bool

		// doesnt enter the below loop for some reason because there's no peers in the map
		fmt.Println("list of peers", s.peers)

		for _, peer := range s.peers {
			req := p2p.MessageChunkRequest{
				FileID: fileID,
				Chunk:  chunk,
			}

			var buf bytes.Buffer
			enc := gob.NewEncoder(&buf)
			if err := enc.Encode(req); err != nil {
				log.Printf("Failed to encode chunk request: %v", err)
				continue
			}

			if err := peer.Send(buf.Bytes()); err == nil {
				log.Printf("Requested chunk %d from peer %s", chunk, peer.RemoteAddr())

				// Read the chunk data
				chunkData := make([]byte, meta.ChunkSize)
				if _, err := io.ReadFull(peer, chunkData); err != nil {
					log.Printf("Failed to read chunk %d data from peer %s: %v", chunk, peer.RemoteAddr(), err)
					continue
				}

				// Write the chunk to the output file
				if _, err := outputFile.Write(chunkData); err != nil {
					return fmt.Errorf("failed to write chunk %d to file: %v", chunk, err)
				}

				downloaded = true
				break
			}
		}

		if !downloaded {
			return fmt.Errorf("failed to download chunk %d for file %s", chunk, fileID)
		}
	}

	log.Printf("File with ID %s successfully downloaded and saved as %s", fileID, outputFileName)
	return nil
}

func (s *FileServer) handleMessage(peer p2p.Peer, msg *p2p.Message) error {
	switch msg.Type {
	case "chunk_request":
		payload, ok := msg.Payload.(p2p.MessageChunkRequest)
		if !ok {
			return fmt.Errorf("invalid payload type for chunk_request")
		}
		return s.handleChunkRequest(peer, payload)
	case "metadata_request":
		payload, ok := msg.Payload.(p2p.MessageMetadataRequest)
		if !ok {
			return fmt.Errorf("invalid payload type for metadata_request")
		}
		return s.handleMetadataRequest(peer, payload)
	default:
		log.Printf("Received unknown message type %s from %s", msg.Type, peer.RemoteAddr())
		return nil
	}
}

func (s *FileServer) handleMetadataRequest(peer p2p.Peer, req p2p.MessageMetadataRequest) error {
	log.Printf("Received metadata request for file ID %s from peer %s", req.FileID, peer.RemoteAddr())

	// Load metadata
	meta, err := s.store.GetMetadata(req.FileID)
	if err != nil {
		log.Printf("Metadata not found for file ID %s: %v", req.FileID, err)
		return err
	}

	// Send metadata back to the requesting peer
	resp := p2p.MessageMetadataResponse{Metadata: *meta}
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(resp); err != nil {
		log.Printf("Failed to encode metadata response: %v", err)
		return err
	}

	if _, err := peer.Write(buf.Bytes()); err != nil {
		log.Printf("Failed to send metadata response to peer %s: %v", peer.RemoteAddr(), err)
		return err
	}

	log.Printf("Sent metadata for file ID %s to peer %s", req.FileID, peer.RemoteAddr())
	return nil
}

func (s *FileServer) SetTrackerAddress(addr string) {
	s.opts.TrackerAddr = addr
}

func (s *FileServer) bootstrapNetwork() {
	for _, node := range s.opts.BootstrapNodes {
		if node != "" {
			go func(node string) {
				log.Printf("Connecting to bootstrap node %s", node)
				if err := s.opts.Transport.Dial(node); err != nil {
					log.Printf("Failed to connect to %s: %v", node, err)
				}
			}(node)
		}
	}
}

// announceToTracker sends a request to the tracker to announce this peer's file availability.
func announceToTracker(trackerAddr, fileID, listenAddr string) error {
	// Use dynamic detection for the peer address
	peerAddr, err := getPeerAddress(listenAddr[1:]) // Strip the leading ":" in ":3000"
	if err != nil {
		return fmt.Errorf("failed to determine peer address: %v", err)
	}

	url := fmt.Sprintf("%s/announce?file_id=%s&peer_addr=%s", trackerAddr, fileID, peerAddr)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to announce to tracker, status code: %d", resp.StatusCode)
	}
	return nil
}
