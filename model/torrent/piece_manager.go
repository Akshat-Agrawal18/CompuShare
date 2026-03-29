// Package torrent provides BitTorrent-style model distribution for CompuShare.
// Model chunks are distributed across providers like torrents, with rarest-first piece selection.
package torrent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/compushare/compushare/network/libp2p"
	"github.com/compushare/compushare/storage"
	"github.com/golang/glog"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// PieceManager handles the download and seeding of model pieces across the P2P network.
type PieceManager struct {
	host  *libp2p.Host
	store *storage.ContentStore

	activeDownloads map[string]*Download
	peers           map[string][]string // piece hash -> peer addresses

	mu sync.RWMutex
}

// Seeder implements the seeder side of BitTorrent-style model distribution
type Seeder struct {
	pm *PieceManager
}

// NewPieceManager creates a new piece manager
func NewPieceManager(host *libp2p.Host, store *storage.ContentStore) *PieceManager {
	return &PieceManager{
		host:            host,
		store:           store,
		activeDownloads: make(map[string]*Download),
		peers:           make(map[string][]string),
	}
}

// NewSeeder creates a new seeder
func NewSeeder(pm *PieceManager) *Seeder {
	return &Seeder{pm: pm}
}

// RegisterStreamHandler registers the torrent protocol handler
func (s *Seeder) RegisterStreamHandler(host *libp2p.Host) {
	host.Underlying().SetStreamHandler(protocol.ID("/compushare/torrent/1.0"), s.handleStream)
}

func (s *Seeder) handleStream(stream network.Stream) {
	defer stream.Close()

	// Read requested piece hash
	buf := make([]byte, 64) // SHA-256 hash length
	n, err := stream.Read(buf)
	if err != nil {
		glog.Errorf("Failed to read piece hash from stream: %v", err)
		return
	}
	pieceHash := string(buf[:n])

	// Handle request
	data, err := s.HandlePieceRequest(pieceHash)
	if err != nil {
		glog.Errorf("Failed to handle piece request: %v", err)
		return
	}

	// Send data
	if _, err := stream.Write(data); err != nil {
		glog.Errorf("Failed to write piece data to stream: %v", err)
	}
}

// HandlePieceRequest handles an incoming request for a piece
func (s *Seeder) HandlePieceRequest(pieceHash string) ([]byte, error) {
	// Retrieve piece from storage
	data, err := s.pm.store.GetChunk(pieceHash)
	if err != nil {
		return nil, fmt.Errorf("piece not found: %w", err)
	}

	return data, nil
}

// AnnouncePiece announces that this node has a piece available for seeding.
// Calling with an empty pieceHash is a no-op to prevent registering invalid entries.
func (s *Seeder) AnnouncePiece(pieceHash string) {
	if pieceHash == "" {
		glog.V(4).Infof("AnnouncePiece: skipping empty pieceHash")
		return
	}
	// Register self as a provider of this piece so downloaders can find us
	selfAddr := s.pm.host.PeerID().String()
	s.pm.RegisterPeerPiece(selfAddr, pieceHash)
	glog.V(4).Infof("Announced piece %s as peer %s", pieceHash[:min(16, len(pieceHash))]+"...", selfAddr)
}

// Download represents an in-progress model download
type Download struct {
	ModelID     string
	Pieces      map[int]*Piece // piece index -> piece
	TotalPieces int
	Downloaded  int
	StartedAt   time.Time
	Peers       []string
	Done        chan struct{} // closed when all pieces are downloaded
}

// Piece represents a single piece of a model
type Piece struct {
	Index      int
	Hash       string // SHA-256 of piece data
	Size       int64
	Data       []byte
	Downloaded bool
	Peers      []string // peers that have this piece
}

// Manifest represents the torrent manifest for a model
type Manifest struct {
	ModelID     string   `json:"model_id"`
	Version     string   `json:"version"`
	PieceSize   int64    `json:"piece_size"`
	PieceHashes []string `json:"piece_hashes"`
	TotalSize   int64    `json:"total_size"`
	Signature   []byte   `json:"signature"`
}

// CreateManifest creates a torrent manifest for a model
func (pm *PieceManager) CreateManifest(modelID string, pieceSize int64, data io.Reader) (*Manifest, error) {
	manifest := &Manifest{
		ModelID:   modelID,
		PieceSize: pieceSize,
	}

	buf := make([]byte, pieceSize)
	for {
		n, err := io.ReadFull(data, buf)
		if err == io.EOF {
			break
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("failed to read model data: %w", err)
		}

		pieceData := buf[:n]
		hash := sha256.Sum256(pieceData)
		hashHex := hex.EncodeToString(hash[:])

		pm.store.AddChunk(pieceData)
		manifest.PieceHashes = append(manifest.PieceHashes, hashHex)
		buf = make([]byte, pieceSize)
	}

	manifest.TotalSize = int64(len(manifest.PieceHashes)) * pieceSize
	return manifest, nil
}

// StartDownload starts downloading a model from multiple peers
func (pm *PieceManager) StartDownload(ctx context.Context, manifest *Manifest, peers []string) (*Download, error) {
	download := &Download{
		ModelID:     manifest.ModelID,
		Pieces:      make(map[int]*Piece),
		TotalPieces: len(manifest.PieceHashes),
		StartedAt:   time.Now(),
		Peers:       peers,
		Done:        make(chan struct{}),
	}

	for i, hash := range manifest.PieceHashes {
		download.Pieces[i] = &Piece{Index: i, Hash: hash}
	}

	pm.mu.Lock()
	pm.activeDownloads[manifest.ModelID] = download
	pm.mu.Unlock()

	go pm.downloadLoop(ctx, download)
	return download, nil
}

func (pm *PieceManager) downloadLoop(ctx context.Context, download *Download) {
	defer close(download.Done)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			piece := pm.selectRarestPiece(download)
			if piece == nil {
				return
			}
			if err := pm.downloadPiece(ctx, download, piece); err != nil {
				glog.Errorf("Failed to download piece %d: %v", piece.Index, err)
			}
		}
	}
}

func (pm *PieceManager) selectRarestPiece(download *Download) *Piece {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	var rarest *Piece
	minPeers := int(^uint(0) >> 1)

	for _, piece := range download.Pieces {
		if piece.Downloaded {
			continue
		}
		peerCount := len(pm.peers[piece.Hash])
		if peerCount < minPeers {
			minPeers = peerCount
			rarest = piece
		}
	}
	return rarest
}

func (pm *PieceManager) downloadPiece(ctx context.Context, download *Download, piece *Piece) error {
	pm.mu.RLock()
	peerList := pm.peers[piece.Hash]
	pm.mu.RUnlock()

	if len(peerList) == 0 {
		return fmt.Errorf("no peers have piece %d", piece.Index)
	}

	peerAddr := peerList[rand.Intn(len(peerList))]
	data, err := pm.fetchPieceFromPeer(ctx, peerAddr, piece.Hash)
	if err != nil {
		return err
	}

	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != piece.Hash {
		return fmt.Errorf("piece hash mismatch")
	}

	pm.mu.Lock()
	piece.Data = data
	piece.Downloaded = true
	download.Downloaded++
	pm.mu.Unlock()

	glog.V(3).Infof("Downloaded piece %d/%d", download.Downloaded, download.TotalPieces)
	return nil
}

func (pm *PieceManager) fetchPieceFromPeer(ctx context.Context, peerAddr, pieceHash string) ([]byte, error) {
	pid, err := peer.Decode(peerAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid peer ID: %w", err)
	}

	stream, err := pm.host.Underlying().NewStream(ctx, pid, protocol.ID("/compushare/torrent/1.0"))
	if err != nil {
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}
	defer stream.Close()

	if _, err := stream.Write([]byte(pieceHash)); err != nil {
		return nil, fmt.Errorf("failed to write hash to stream: %w", err)
	}

	// Read piece data — limit to 512 MB to prevent OOM from malicious peers
	const maxPieceBytes = 512 << 20 // 512 MB
	data, err := io.ReadAll(io.LimitReader(stream, maxPieceBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read piece data: %w", err)
	}
	return data, nil
}

func (pm *PieceManager) RegisterPeerPiece(peerAddr, pieceHash string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.peers[pieceHash] = append(pm.peers[pieceHash], peerAddr)
}

// AssemblePieces concatenates the downloaded pieces in order to reconstruct the original GGUF model file.
func (pm *PieceManager) AssemblePieces(modelID, outputPath string) error {
	pm.mu.RLock()
	download, ok := pm.activeDownloads[modelID]
	pm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("no active download found for model %s", modelID)
	}

	if download.Downloaded != len(download.Pieces) {
		return fmt.Errorf("download not completed for model %s", modelID)
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer out.Close()

	glog.Infof("Assembling model %s from %d pieces...", modelID, len(download.Pieces))
	for i, piece := range download.Pieces {
		// Assuming piece.Data holds the actual bytes. In a real highly-scaled BitTorrent
		// implementation, piece.Data would just point to disk chunks to save RAM.
		if len(piece.Data) == 0 {
			// fallback to loading from store if cleared from RAM
			data, err := pm.store.GetChunk(piece.Hash)
			if err != nil {
				return fmt.Errorf("missing piece data for piece %d: %w", i, err)
			}
			piece.Data = data
		}
		
		if _, err := out.Write(piece.Data); err != nil {
			return fmt.Errorf("failed writing piece %d: %w", i, err)
		}
	}

	return nil
}

// VerifyGGUF checks the magic bytes of the assembled file to ensure it's a valid GGUF.
func VerifyGGUF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	magic := make([]byte, 4)
	if _, err := f.Read(magic); err != nil {
		return err
	}

	// GGUF magic bytes are "GGUF" in ASCII format
	if string(magic) != "GGUF" {
		return fmt.Errorf("invalid GGUF magic bytes: expected 'GGUF', got %q", string(magic))
	}
	return nil
}
