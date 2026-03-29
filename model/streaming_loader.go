// Package model provides model loading and management functionality.
package model

import (
	"context"
	"fmt"
	"sync"

	"github.com/compushare/compushare/model/torrent"
	"github.com/compushare/compushare/network/discovery"
	"github.com/compushare/compushare/storage"
	"github.com/golang/glog"
)

// StreamingModelLoader handles loading model layers, potentially fetching missing chunks from peers.
type StreamingModelLoader struct {
	store        *storage.ContentStore
	pieceManager *torrent.PieceManager
	caps         *discovery.CapabilityStore
	cache        map[string]*ModelLayers
	mu           sync.RWMutex
}

// ModelLayers represents loaded model layers
type ModelLayers struct {
	Weights map[int][]byte // layer index -> serialized weights
}

// NewStreamingModelLoader creates a new model loader
func NewStreamingModelLoader(store *storage.ContentStore, pm *torrent.PieceManager, caps *discovery.CapabilityStore) *StreamingModelLoader {
	return &StreamingModelLoader{
		store:        store,
		pieceManager: pm,
		caps:         caps,
		cache:        make(map[string]*ModelLayers),
	}
}

// EnsureLocalModel loads the specified model from the network if missing, assembles the GGUF, and returns its file path.
func (sml *StreamingModelLoader) EnsureLocalModel(ctx context.Context, modelID string) (string, error) {
	// Destination path
	expectedPath, err := sml.store.GetModelPath(modelID)
	if err == nil && expectedPath != "" {
		// Verify if it's a valid GGUF already
		if err := torrent.VerifyGGUF(expectedPath); err == nil {
			glog.Infof("Model %s is already fully downloaded and valid at %s", modelID, expectedPath)
			return expectedPath, nil
		}
	}

	// 1. Get manifest
	manifest, err := sml.store.LoadManifest(modelID)
	if err != nil {
		return "", fmt.Errorf("failed to load manifest: %w", err)
	}

	// 2. Start download
	var missingHashes []string
	for _, chunk := range manifest.Chunks {
		missingHashes = append(missingHashes, chunk.ContentHash)
	}

	glog.Infof("Fetching chunks for model %s", modelID)

	tm := &torrent.Manifest{
		ModelID:     modelID,
		PieceHashes: missingHashes,
	}

	var peers []string
	if sml.caps != nil {
		for _, p := range sml.caps.GetProviders() {
			if p.IsProvider {
				peers = append(peers, p.PeerID)
			}
		}
	}

	download, err := sml.pieceManager.StartDownload(ctx, tm, peers)
	if err != nil {
		return "", fmt.Errorf("failed to start download: %w", err)
	}

	// Wait for download to complete
	select {
	case <-download.Done:
		if download.Downloaded < download.TotalPieces {
			return "", fmt.Errorf("download incomplete: got %d/%d pieces", download.Downloaded, download.TotalPieces)
		}
	case <-ctx.Done():
		return "", fmt.Errorf("download cancelled: %w", ctx.Err())
	}

	// 3. Assemble pieces into final GGUF
	glog.Infof("Download complete. Assembling GGUF...")
	if expectedPath == "" {
		expectedPath = fmt.Sprintf("%s/%s.gguf", sml.store.GetDataDir(), modelID)
	}
	
	if err := sml.pieceManager.AssemblePieces(modelID, expectedPath); err != nil {
		return "", fmt.Errorf("failed to assemble pieces: %w", err)
	}

	if err := torrent.VerifyGGUF(expectedPath); err != nil {
		return "", fmt.Errorf("assembled file is not a valid GGUF: %w", err)
	}

	return expectedPath, nil
}
