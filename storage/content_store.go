// Package storage provides content-addressed storage for model chunks.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/compushare/compushare/network/libp2p"
	"github.com/golang/glog"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// ContentStore provides content-addressed storage for model chunks.
// Similar to IPFS, each chunk is identified by its SHA-256 hash.
type ContentStore struct {
	dataDir string

	// chunks maps content hash to file path
	chunks map[string]string

	// mutex for thread-safe access
	mu sync.RWMutex
}

// ModelInfo contains metadata about a model
type ModelInfo struct {
	ModelID      string
	TotalSizeGB  float64
	TotalLayers  int
	Quantization string
	ChunkSizeMB  int
}

// NewContentStore creates a new content store
func NewContentStore(dataDir string) *ContentStore {
	return &ContentStore{
		dataDir: dataDir,
		chunks:  make(map[string]string),
	}
}

// GetDataDir returns the underlying data directory.
func (cs *ContentStore) GetDataDir() string {
	return cs.dataDir
}

// Start initializes the content store
func (cs *ContentStore) Start(ctx context.Context) error {
	// Ensure directories exist
	if err := os.MkdirAll(filepath.Join(cs.dataDir, "chunks"), 0755); err != nil {
		return fmt.Errorf("failed to create chunks dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cs.dataDir, "models"), 0755); err != nil {
		return fmt.Errorf("failed to create models dir: %w", err)
	}

	// Index existing chunks
	return cs.indexChunks()
}

// Stop stops the content store
func (cs *ContentStore) Stop() {
	// Nothing to clean up for now
}

// indexChunks scans the chunks directory and indexes all existing chunks
func (cs *ContentStore) indexChunks() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	chunksDir := filepath.Join(cs.dataDir, "chunks")
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Chunk files are named by their content hash
		hash := entry.Name()
		if len(hash) == 64 { // SHA-256 hex length
			cs.chunks[hash] = filepath.Join(chunksDir, hash)
		}
	}

	return nil
}

// AddChunk adds a chunk to the store and returns its content address
func (cs *ContentStore) AddChunk(data []byte) (string, error) {
	// Compute SHA-256 hash
	hash := sha256.Sum256(data)
	hashHex := hex.EncodeToString(hash[:])

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Check if already exists
	if path, ok := cs.chunks[hashHex]; ok {
		return path, nil // Already stored
	}

	// Write to chunks directory with user-only permissions (data dir is private)
	path := filepath.Join(cs.dataDir, "chunks", hashHex)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", fmt.Errorf("failed to write chunk: %w", err)
	}

	cs.chunks[hashHex] = path
	return path, nil
}

// GetChunk retrieves a chunk by its content address
func (cs *ContentStore) GetChunk(hash string) ([]byte, error) {
	cs.mu.RLock()
	path, ok := cs.chunks[hash]
	cs.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("chunk not found: %s", hash)
	}

	return os.ReadFile(path)
}

// HasChunk checks if a chunk exists in the store
func (cs *ContentStore) HasChunk(hash string) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	_, ok := cs.chunks[hash]
	return ok
}

// ListChunks returns all stored chunk hashes
func (cs *ContentStore) ListChunks() []string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	hashes := make([]string, 0, len(cs.chunks))
	for hash := range cs.chunks {
		hashes = append(hashes, hash)
	}
	return hashes
}

// GetModelInfo returns metadata about a model
func (cs *ContentStore) GetModelInfo(modelID string) (*ModelInfo, error) {
	// TODO: Load from model manifest file
	// For now, return defaults
	switch modelID {
	case "tinyllama-1b":
		return &ModelInfo{
			ModelID:      "tinyllama-1b",
			TotalSizeGB:  1.0,
			TotalLayers:  22,
			Quantization: "Q4_0",
			ChunkSizeMB:  1,
		}, nil
	case "llama-7b":
		return &ModelInfo{
			ModelID:      "llama-7b",
			TotalSizeGB:  3.5,
			TotalLayers:  32,
			Quantization: "Q4_0",
			ChunkSizeMB:  1,
		}, nil
	case "llama-13b":
		return &ModelInfo{
			ModelID:      "llama-13b",
			TotalSizeGB:  6.5,
			TotalLayers:  40,
			Quantization: "Q4_0",
			ChunkSizeMB:  1,
		}, nil
	default:
		return nil, fmt.Errorf("unknown model: %s", modelID)
	}
}

// GetModelPath returns the local path to a model directory
func (cs *ContentStore) GetModelPath(modelID string) (string, error) {
	path := filepath.Join(cs.dataDir, "models", modelID)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", fmt.Errorf("model not found: %s", modelID)
	}
	return path, nil
}

// ModelManifest represents the manifest for a distributed model
type ModelManifest struct {
	ModelID      string       `json:"model_id"`
	Version      string       `json:"version"`
	TotalSizeGB  float64      `json:"total_size_gb"`
	TotalLayers  int          `json:"total_layers"`
	Quantization string       `json:"quantization"`
	Chunks       []ModelChunk `json:"chunks"`
	Signature    []byte       `json:"signature"`
}

// ModelChunk represents a single chunk of a model
type ModelChunk struct {
	ChunkID     string `json:"chunk_id"`
	ContentHash string `json:"content_hash"` // SHA-256 of chunk data
	SizeMB      int    `json:"size_mb"`
	Layers      []int  `json:"layers"` // Which layers this chunk contains
	StartToken  int    `json:"start_token"`
	EndToken    int    `json:"end_token"`
}

// LoadManifest loads a model manifest from storage
func (cs *ContentStore) LoadManifest(modelID string) (*ModelManifest, error) {
	manifestPath := filepath.Join(cs.dataDir, "models", modelID, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}

	var manifest ModelManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to unmarshal manifest: %w", err)
	}
	return &manifest, nil
}

// SaveManifest saves a model manifest to storage
func (cs *ContentStore) SaveManifest(manifest *ModelManifest) error {
	modelDir := filepath.Join(cs.dataDir, "models", manifest.ModelID)
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		return fmt.Errorf("failed to create model dir: %w", err)
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	manifestPath := filepath.Join(modelDir, "manifest.json")
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	return nil
}

// ProvideModel advertises this node has a model available for seeding
// In a full implementation, this would integrate with the DHT for provider discovery
func (cs *ContentStore) ProvideModel(modelID string) error {
	modelPath, err := cs.GetModelPath(modelID)
	if err != nil {
		return err
	}

	glog.Infof("Advertising model %s from %s", modelID, modelPath)

	// TODO: Register with DHT as provider for this model
	// key := []byte("model:" + modelID)
	// return h.Provide(ctx, key)

	return nil
}

// FetchModel downloads a model from the network
func (cs *ContentStore) FetchModel(ctx context.Context, modelID string, peers []string) error {
	glog.Infof("Fetching model %s from %d peers", modelID, len(peers))

	// 1. Load manifest to know which chunks we need
	manifest, err := cs.LoadManifest(modelID)
	if err != nil {
		return fmt.Errorf("failed to load manifest: %w", err)
	}

	// 2. Identify missing chunks
	var missingHashes []string
	for _, chunk := range manifest.Chunks {
		if !cs.HasChunk(chunk.ContentHash) {
			missingHashes = append(missingHashes, chunk.ContentHash)
		}
	}

	if len(missingHashes) == 0 {
		glog.Infof("All chunks already available locally for model %s", modelID)
		return nil
	}

	glog.Infof("Need to fetch %d missing chunks for model %s", len(missingHashes), modelID)
	return fmt.Errorf("missing chunks found but no PieceManager available; use StreamingModelLoader instead")
}

// Seeder manages the seeding of model chunks to other peers
type Seeder struct {
	store *ContentStore
}

// NewSeeder creates a new seeder
func NewSeeder(store *ContentStore) *Seeder {
	return &Seeder{store: store}
}

// StartSeeding starts seeding all local model chunks
func (s *Seeder) StartSeeding() {
	// TODO: Register all chunks with DHT provider system
	// This makes chunks available to other peers who need them
	chunks := s.store.ListChunks()
	glog.Infof("Seeding %d chunks", len(chunks))

	for _, chunk := range chunks {
		// TODO: h.Provide(ctx, []byte("chunk:"+chunk))
		glog.V(4).Infof("Seeding chunk: %s", chunk[:16]+"...")
	}
}

// ReadFromSeeder reads a chunk from a remote seeder via streaming.
// The host parameter provides the libp2p host for opening streams.
func ReadFromSeeder(ctx context.Context, host *libp2p.Host, seederAddr string, chunkHash string, w io.Writer) error {
	hashPrefix := chunkHash
	if len(hashPrefix) > 16 {
		hashPrefix = hashPrefix[:16]
	}
	glog.Infof("Reading chunk %s from %s", hashPrefix+"...", seederAddr)

	pid, err := peer.Decode(seederAddr)
	if err != nil {
		return fmt.Errorf("invalid seeder peer ID: %w", err)
	}

	stream, err := host.Underlying().NewStream(ctx, pid, protocol.ID("/compushare/torrent/1.0"))
	if err != nil {
		return fmt.Errorf("failed to open stream to seeder: %w", err)
	}
	defer stream.Close()

	if _, err := stream.Write([]byte(chunkHash)); err != nil {
		return fmt.Errorf("failed to write chunk hash to stream: %w", err)
	}

	if _, err := io.Copy(w, stream); err != nil {
		return fmt.Errorf("failed to read chunk data from stream: %w", err)
	}

	return nil
}
