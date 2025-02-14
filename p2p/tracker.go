package p2p

import (
	// "context"
	"encoding/json"
	"fmt"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// stores metadata and peer information for each file
type FileInfo struct {
	FileID      string    `json:"file_id"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	UploadedAt  time.Time `json:"uploaded_at"`
	Description string    `json:"description"`
	Categories  []string  `json:"categories"`
	NumPeers    int       `json:"num_peers"`
	Extension   string    `json:"extension"`
	NumChunks   int       `json:"num_chunks"`
	ChunkSize   int       `json:"chunk_size"`
	ChunkHashes []string  `json:"chunk_hashes"`
	TotalHash   string    `json:"total_hash"`
	TotalSize   int64     `json:"total_size"`
}

// Tracker stores metadata and peer information for each file
type Tracker struct {
	fileIndex    map[string]*FileInfo       // fileID -> file metadata
	peerIndex    map[string]map[string]bool // fileID -> peer addresses
	peerLastSeen map[string]time.Time       // peer address -> last heartbeat
	mu           sync.RWMutex
}

func NewTracker() *Tracker {
	return &Tracker{
		fileIndex:    make(map[string]*FileInfo),
		peerIndex:    make(map[string]map[string]bool),
		peerLastSeen: make(map[string]time.Time),
	}
}

// StartTracker starts the tracker server on the specified address
func (t *Tracker) StartTracker(address string) {
	http.HandleFunc("/announce", t.HandleAnnounce)
	http.HandleFunc("/peers", t.HandleGetPeers)

	log.Printf("Tracker listening on %s", address)
	if err := http.ListenAndServe(address, nil); err != nil {
		log.Fatalf("Failed to start tracker: %v", err)
	}
}

// handleAnnounce handles peers announcing the files they have
// Endpoint: /announce?file_id=<fileID>&peer_addr=<peerAddr>
func (t *Tracker) HandleAnnounce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var announce struct {
		FileID      string   `json:"file_id"`
		PeerAddr    string   `json:"peer_addr"`
		Name        string   `json:"name"`
		Size        int64    `json:"size"`
		Description string   `json:"description"`
		Categories  []string `json:"categories"`
		Extension   string   `json:"extension"`
		NumChunks   int      `json:"num_chunks"`
		ChunkSize   int      `json:"chunk_size"`
		ChunkHashes []string `json:"chunk_hashes"`
		TotalHash   string   `json:"total_hash"`
		TotalSize   int64    `json:"total_size"`
	}

	if err := json.NewDecoder(r.Body).Decode(&announce); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Clean up stale peers
	t.cleanupStalePeers()

	// Update file info
	if _, exists := t.fileIndex[announce.FileID]; !exists {
		t.fileIndex[announce.FileID] = &FileInfo{
			FileID:      announce.FileID,
			Name:        announce.Name,
			Size:        announce.Size,
			UploadedAt:  time.Now(),
			Description: announce.Description,
			Categories:  announce.Categories,
			Extension:   announce.Extension,
			NumChunks:   announce.NumChunks,
			ChunkSize:   announce.ChunkSize,
			ChunkHashes: announce.ChunkHashes,
			TotalHash:   announce.TotalHash,
			TotalSize:   announce.TotalSize,
		}
	}

	// Update peer info
	if _, exists := t.peerIndex[announce.FileID]; !exists {
		t.peerIndex[announce.FileID] = make(map[string]bool)
	}

	t.peerIndex[announce.FileID][announce.PeerAddr] = true
	t.peerLastSeen[announce.PeerAddr] = time.Now()

	// Update peer count
	t.fileIndex[announce.FileID].NumPeers = len(t.peerIndex[announce.FileID])

	w.WriteHeader(http.StatusOK)
}

// Helper function to check if a slice contains a string
func contains(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

// HandleGetMetadata handles requests by peers for file metadata
func (t *Tracker) HandleGetMetadata(w http.ResponseWriter, r *http.Request) {
	fileID := r.URL.Query().Get("file_id")
	//no fileID provided
	if fileID == "" {
		http.Error(w, "file_id is required", http.StatusBadRequest)
		return
	}

	//lock the tracker
	t.mu.RLock()
	fileInfo, exists := t.fileIndex[fileID]
	t.mu.RUnlock()

	if !exists {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(fileInfo)
}

// HandleListFiles handles requests by peers to list all available files
func (t *Tracker) HandleListFiles(w http.ResponseWriter, r *http.Request) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Support filtering and pagination
	category := r.URL.Query().Get("category")
	search := r.URL.Query().Get("search")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage := 50

	var files []*FileInfo
	for _, info := range t.fileIndex {
		// Apply filters
		if category != "" && !contains(info.Categories, category) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(info.Name), strings.ToLower(search)) {
			continue
		}
		files = append(files, info)
	}

	// Sort by number of peers (popularity) by default
	sort.Slice(files, func(i, j int) bool {
		return files[i].NumPeers > files[j].NumPeers
	})

	// Apply pagination
	start := (page - 1) * perPage
	end := start + perPage
	if start >= len(files) {
		files = []*FileInfo{}
	} else if end > len(files) {
		files = files[start:]
	} else {
		files = files[start:end]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"files": files,
		"total": len(files),
		"page":  page,
	})
}

// StartCleanupLoop starts a background goroutine to clean up inactive peers
func (t *Tracker) StartCleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for range ticker.C {
			t.cleanupInactivePeers()
		}
	}()
}

// cleanupInactivePeers removes peers that have not sent a heartbeat in the last 30 minutes
func (t *Tracker) cleanupInactivePeers() {
	t.mu.Lock()
	defer t.mu.Unlock()

	threshold := time.Now().Add(-30 * time.Minute)

	for peerAddr, lastSeen := range t.peerLastSeen {
		if lastSeen.Before(threshold) {
			// Remove inactive peer
			for fileID, peers := range t.peerIndex {
				if peers[peerAddr] {
					delete(peers, peerAddr)
					// Update peer count
					if info, exists := t.fileIndex[fileID]; exists {
						info.NumPeers = len(peers)
						// Remove file if no peers are sharing it
						if info.NumPeers == 0 {
							delete(t.fileIndex, fileID)
							delete(t.peerIndex, fileID)
						}
					}
				}
			}
			delete(t.peerLastSeen, peerAddr)
		}
	}
}

// SetupUPnP sets up port forwarding using UPnP
func setupUPnP(port int) error {
	clients, errors, err := internetgateway2.NewWANIPConnection1Clients()
	if err != nil {
		return fmt.Errorf("failed to discover UPnP clients: %v", err)
	}

	// Try each client until we succeed
	for i, client := range clients {
		if err := errors[i]; err != nil {
			continue
		}

		// Get external IP
		ip, err := client.GetExternalIPAddress()
		if err != nil {
			continue
		}

		// Add port mapping
		// The correct order is:
		// NewRemoteHost string
		// NewExternalPort uint16
		// NewProtocol string
		// NewInternalPort uint16
		// NewInternalClient string
		// NewEnabled bool
		// NewPortMappingDescription string
		// NewLeaseDuration uint32
		err = client.AddPortMapping(
			"",                 // NewRemoteHost
			uint16(port),       // NewExternalPort
			"TCP",              // NewProtocol
			uint16(port),       // NewInternalPort
			ip,                 // NewInternalClient
			true,               // NewEnabled
			"ForeverStore P2P", // NewPortMappingDescription
			3600,               // NewLeaseDuration
		)
		if err == nil {
			log.Printf("Successfully mapped port %d via UPnP (External IP: %s)", port, ip)
			return nil
		}
	}

	return fmt.Errorf("failed to set up UPnP port mapping on port %d", port)
}

// HandleGetPeers handles requests by peers to get a list of peers for a file
func (t *Tracker) HandleGetPeers(w http.ResponseWriter, r *http.Request) {
	fileID := r.URL.Query().Get("file_id")

	if fileID == "" {
		http.Error(w, "file_id is required", http.StatusBadRequest)
		return
	}

	t.mu.Lock()
	// Clean up stale peers before returning the list
	t.cleanupStalePeers()

	// Get the list of peers for this fileID
	peers, exists := t.peerIndex[fileID]
	t.mu.Unlock()

	// Always return a JSON response, even when there are no peers
	w.Header().Set("Content-Type", "application/json")

	if !exists || len(peers) == 0 {
		log.Printf("No peers found for file %s", fileID)
		// Return empty array instead of error
		json.NewEncoder(w).Encode([]string{})
		return
	}

	// Convert the peer map to a list for easier JSON encoding
	peerList := make([]string, 0, len(peers))
	for peer := range peers {
		peerList = append(peerList, peer)
	}

	log.Printf("Peers for file %s: %v", fileID, peerList)
	if err := json.NewEncoder(w).Encode(peerList); err != nil {
		http.Error(w, "Failed to encode peer list", http.StatusInternalServerError)
	}
}

// cleanupStalePeers removes peers that have not sent a heartbeat in the last 2 minutes
func (t *Tracker) cleanupStalePeers() {
	threshold := time.Now().Add(-2 * time.Minute) // Consider peers stale after 2 minute

	for peerAddr, lastSeen := range t.peerLastSeen {
		if lastSeen.Before(threshold) {
			// Remove stale peer from all files
			for fileID, peers := range t.peerIndex {
				if peers[peerAddr] {
					delete(peers, peerAddr)
					// Update file info
					if info, exists := t.fileIndex[fileID]; exists {
						info.NumPeers = len(peers)
						// Remove file if no peers are sharing it
						if info.NumPeers == 0 {
							delete(t.fileIndex, fileID)
							delete(t.peerIndex, fileID)
						}
					}
				}
			}
			delete(t.peerLastSeen, peerAddr)
		}
	}
}

// RemovePeer allows a peer to be removed from all file listings when it disconnects
func (t *Tracker) RemovePeer(peerAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for fileID, peers := range t.peerIndex {
		if peers[peerAddr] {
			delete(peers, peerAddr)
			log.Printf("Removed peer %s from file %s", peerAddr, fileID)

			// Update peer count in file info
			if info, exists := t.fileIndex[fileID]; exists {
				info.NumPeers = len(peers)
				// Clean up empty entries
				if info.NumPeers == 0 {
					delete(t.fileIndex, fileID)
					delete(t.peerIndex, fileID)
				}
			}
		}
	}

	// Remove from last seen
	delete(t.peerLastSeen, peerAddr)
}
