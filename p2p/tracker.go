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

	// Update file info if it doesn't exist or if it's changed
	existingFile, exists := t.fileIndex[announce.FileID]
	if !exists || existingFile.TotalHash != announce.TotalHash {
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

	// Initialize peer list for this file if it doesn't exist
	if _, exists := t.peerIndex[announce.FileID]; !exists {
		t.peerIndex[announce.FileID] = make(map[string]bool)
	}

	// Add peer to the file's peer list
	t.peerIndex[announce.FileID][announce.PeerAddr] = true

	// Update peer's last seen timestamp
	t.peerLastSeen[announce.PeerAddr] = time.Now()

	// Update peer count
	t.fileIndex[announce.FileID].NumPeers = len(t.peerIndex[announce.FileID])

	log.Printf("Peer %s announced file %s (Total peers: %d)",
		announce.PeerAddr, announce.FileID, t.fileIndex[announce.FileID].NumPeers)

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

func (t *Tracker) HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var heartbeat struct {
		PeerAddr string   `json:"peer_addr"`
		FileIDs  []string `json:"file_ids"`
	}

	if err := json.NewDecoder(r.Body).Decode(&heartbeat); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Update peer's last seen timestamp
	t.peerLastSeen[heartbeat.PeerAddr] = time.Now()

	// Track which files the peer is currently sharing
	currentlySharing := make(map[string]bool)
	for _, fileID := range heartbeat.FileIDs {
		currentlySharing[fileID] = true

		// Initialize peer list for this file if it doesn't exist
		if _, exists := t.peerIndex[fileID]; !exists {
			t.peerIndex[fileID] = make(map[string]bool)
		}

		// Add peer to the file's peer list
		t.peerIndex[fileID][heartbeat.PeerAddr] = true

		// Update peer count in file info
		if info, exists := t.fileIndex[fileID]; exists {
			info.NumPeers = len(t.peerIndex[fileID])
			log.Printf("Updated peer count for file %s: %d peers", fileID, info.NumPeers)
		}
	}

	// Remove peer from files it's no longer sharing
	for fileID, peers := range t.peerIndex {
		if peers[heartbeat.PeerAddr] && !currentlySharing[fileID] {
			delete(peers, heartbeat.PeerAddr)
			log.Printf("Peer %s stopped sharing file %s", heartbeat.PeerAddr, fileID)

			// Update file info
			if info, exists := t.fileIndex[fileID]; exists {
				info.NumPeers = len(peers)
				// Remove file if no peers are sharing it
				if info.NumPeers == 0 {
					delete(t.fileIndex, fileID)
					delete(t.peerIndex, fileID)
					log.Printf("Removed file %s as it has no active peers", fileID)
				} else {
					log.Printf("Updated peer count for file %s: %d peers", fileID, info.NumPeers)
				}
			}
		}
	}

	w.WriteHeader(http.StatusOK)
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

// CleanupConfig holds configuration for the cleanup process
type CleanupConfig struct {
	InactivityThreshold time.Duration
	CleanupInterval     time.Duration
}

// DefaultCleanupConfig returns the default cleanup configuration
func DefaultCleanupConfig() CleanupConfig {
	return CleanupConfig{
		InactivityThreshold: 120 * time.Second, // Reduced from 2 minutes
		CleanupInterval:     15 * time.Second,  // Reduced from 30 seconds
	}
}

// StartCleanupLoop starts the periodic cleanup of inactive peers
func (t *Tracker) StartCleanupLoop() {
	config := DefaultCleanupConfig()
	ticker := time.NewTicker(config.CleanupInterval)
	go func() {
		for range ticker.C {
			t.cleanupInactivePeers(config.InactivityThreshold)
		}
	}()
}

// cleanupInactivePeers removes peers that haven't announced within the threshold
func (t *Tracker) cleanupInactivePeers(threshold time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	var removedPeers []string

	// Find inactive peers
	for peerAddr, lastSeen := range t.peerLastSeen {
		if now.Sub(lastSeen) > threshold {
			removedPeers = append(removedPeers, peerAddr)
		}
	}

	// Process each removed peer
	for _, peerAddr := range removedPeers {
		// Remove from lastSeen tracker
		delete(t.peerLastSeen, peerAddr)

		// Remove from all file peer lists and update metadata
		for fileID, peers := range t.peerIndex {
			if peers[peerAddr] {
				delete(peers, peerAddr)
				log.Printf("Removed inactive peer %s from file %s", peerAddr, fileID)

				// Update file info
				if info, exists := t.fileIndex[fileID]; exists {
					info.NumPeers = len(peers)
					// Clean up files with no peers
					if info.NumPeers == 0 {
						delete(t.fileIndex, fileID)
						delete(t.peerIndex, fileID)
						log.Printf("Removed file %s as it has no active peers", fileID)
					} else {
						log.Printf("Updated peer count for file %s: %d peers", fileID, info.NumPeers)
					}
				}
			}
		}

		// Clean up empty peer lists
		for fileID, peers := range t.peerIndex {
			if len(peers) == 0 {
				delete(t.peerIndex, fileID)
				log.Printf("Removed empty peer list for file %s", fileID)
			}
		}

		log.Printf("Removed inactive peer: %s", peerAddr)
	}
}

// UpdatePeerActivity updates the last seen timestamp for a peer
func (t *Tracker) UpdatePeerActivity(peerAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.peerLastSeen[peerAddr] = time.Now()
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

func (t *Tracker) HandleRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		FileID   string `json:"file_id"`
		PeerAddr string `json:"peer_addr"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Remove peer from file's peer list
	if peers, exists := t.peerIndex[req.FileID]; exists {
		delete(peers, req.PeerAddr)

		// Update file info
		if info, exists := t.fileIndex[req.FileID]; exists {
			info.NumPeers = len(peers)
			// Remove file if no peers are sharing it
			if info.NumPeers == 0 {
				delete(t.fileIndex, req.FileID)
				delete(t.peerIndex, req.FileID)
				log.Printf("Removed file %s as it has no active peers", req.FileID)
			}
		}
	}

	w.WriteHeader(http.StatusOK)
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
