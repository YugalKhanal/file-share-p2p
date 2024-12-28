package main

import (
	// "encoding/gob"
	"flag"
	"fmt"
	"github.com/anthdm/foreverstore/p2p"
	"log"
	"time"
)

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
		log.Printf("Starting tracker on %s", *listenAddr)
		tracker.StartTracker(*listenAddr)
		return // Exit after starting the tracker
	}

	// Ensure tracker address is provided for peers
	if *trackerAddr == "" {
		log.Fatal("Tracker address is required. Use -tracker=<tracker_address>")
	}

	// Initialize and start the file server
	server := makeServer(*listenAddr, *bootstrapNode)
	server.SetTrackerAddress(*trackerAddr)

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
