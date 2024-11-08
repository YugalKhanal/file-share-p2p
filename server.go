package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/anthdm/foreverstore/p2p"
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
		ListenAddr:        listenAddr,
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
		}
	}

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

// func (s *FileServer) DownloadFile(fileID string) error {
// 	log.Printf("Attempting to download file with ID: %s", fileID)
//
// 	meta, err := s.store.GetMetadata(fileID)
// 	if err != nil {
// 		return fmt.Errorf("metadata not found for file ID %s: %v", fileID, err)
// 	}
//
// 	for chunk := 0; chunk < meta.NumChunks; chunk++ {
// 		var downloaded bool
//
// 		for _, peer := range s.peers {
// 			// Create request message and serialize it to []byte
// 			req := p2p.MessageChunkRequest{
// 				FileID: fileID,
// 				Chunk:  chunk,
// 			}
//
// 			var buf bytes.Buffer
// 			enc := gob.NewEncoder(&buf)
// 			if err := enc.Encode(req); err != nil {
// 				log.Printf("Failed to encode request: %v", err)
// 				continue
// 			}
//
// 			// Send serialized request
// 			if err := peer.Send(buf.Bytes()); err == nil {
// 				log.Printf("Requested chunk %d from peer %s", chunk, peer.RemoteAddr())
// 				downloaded = true
// 				break
// 			}
// 		}
//
// 		if !downloaded {
// 			return fmt.Errorf("failed to download chunk %d for file %s", chunk, fileID)
// 		}
// 	}
//
// 	log.Printf("File with ID %s successfully downloaded", fileID)
// 	return nil
// }

func (s *FileServer) DownloadFile(fileID string) error {
	log.Printf("Attempting to download file with ID: %s", fileID)

	meta, err := s.store.GetMetadata(fileID)
	if err != nil {
		return fmt.Errorf("metadata not found for file ID %s: %v", fileID, err)
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

		for _, peer := range s.peers {
			req := p2p.MessageChunkRequest{
				FileID: fileID,
				Chunk:  chunk,
			}

			var buf bytes.Buffer
			enc := gob.NewEncoder(&buf)
			if err := enc.Encode(req); err != nil {
				log.Printf("Failed to encode request: %v", err)
				continue
			}

			if err := peer.Send(buf.Bytes()); err == nil {
				log.Printf("Requested chunk %d from peer %s", chunk, peer.RemoteAddr())
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
func announceToTracker(trackerAddr, fileID, peerAddr string) error {
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
