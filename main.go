package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anthdm/foreverstore/p2p"
)

// In main.go
func main() {
	// Parse command-line arguments
	listenAddr := flag.String("listen", ":3000", "server listen address")
	trackerAddr := flag.String("tracker", "", "tracker address (optional)")
	bootstrapNode := flag.String("bootstrap", "", "bootstrap node address (optional)")
	filePath := flag.String("file", "", "file path to share")
	downloadFileID := flag.String("download", "", "file ID to download")
	startTracker := flag.Bool("start-tracker", false, "start a tracker server")
	flag.Parse()

	// Start the tracker if requested
	if *startTracker {
		tracker := p2p.NewTracker()
		http.HandleFunc("/files", tracker.HandleListFiles)
		http.HandleFunc("/announce", tracker.HandleAnnounce)
		http.HandleFunc("/peers", tracker.HandleGetPeers)
		http.HandleFunc("/metadata", tracker.HandleGetMetadata)

		tracker.StartCleanupLoop()
		log.Printf("Starting tracker on %s", *listenAddr)
		log.Fatal(http.ListenAndServe(*listenAddr, nil))
	}

	// Ensure tracker address is provided for peers
	if *trackerAddr == "" {
		log.Fatal("Tracker address is required. Use -tracker=<tracker_address>")
	}

	// Initialize and start the file server
	server := makeServer(*listenAddr, *bootstrapNode)
	server.SetTrackerAddress(*trackerAddr)

	// Setup cleanup on program exit
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Println("Shutting down server...")
		server.Close()
		os.Exit(0)
	}()

	go func() {
		if err := server.Start(); err != nil {
			log.Fatalf("server error: %v", err)
		}
	}()

	time.Sleep(1 * time.Second)

	// Share file if filePath is provided
	if *filePath != "" {
		err := server.ShareFile(*filePath)
		if err != nil {
			log.Fatalf("file sharing error: %v", err)
		}
	}

	// Download file if downloadFileID is provided
	if *downloadFileID != "" {
		err := server.DownloadFile(*downloadFileID)
		if err != nil {
			log.Fatalf("file download error: %v", err)
		} else {
			fmt.Printf("Successfully downloaded file with ID %s\n", *downloadFileID)
		}
	}

	select {} // Keep the server running
}
