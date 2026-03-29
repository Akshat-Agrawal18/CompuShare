// Package libp2p provides the P2P networking layer for CompuShare.
// Uses libp2p for NAT traversal, peer identity, and encrypted communication.
package libp2p

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"github.com/compushare/compushare/internal/config"
	goLibp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/multiformats/go-multiaddr"
)

// Host wraps a libp2p host with CompuShare-specific functionality
type Host struct {
	host   host.Host
	config *config.Config
}

// NewHost creates a new libp2p host with a persistent Ed25519 identity.
// The key is stored in the data directory so that the peer ID remains stable
// across restarts — matching the README spec (Identity: Ed25519 PeerID).
func NewHost(ctx context.Context, cfg *config.Config) (*Host, error) {
	// Ensure data directory exists
	if err := cfg.EnsureDataDir(); err != nil {
		return nil, fmt.Errorf("failed to create data dir: %w", err)
	}

	// Load or generate a persistent Ed25519 identity key
	privKey, err := loadOrCreateIdentity(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load/create identity key: %w", err)
	}

	// Connection manager for rate limiting
	connMgr, err := connmgr.NewConnManager(
		100,                // Low watermark
		cfg.MaxConnections, // High watermark
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create conn manager: %w", err)
	}

	// Build libp2p node with explicit Ed25519 identity
	node, err := goLibp2p.New(
		goLibp2p.Identity(privKey),
		goLibp2p.ListenAddrStrings(cfg.ListenAddresses...),
		goLibp2p.ConnectionManager(connMgr),
		goLibp2p.NATPortMap(),  // NAT port mapping
		goLibp2p.EnableRelay(), // Circuit relay for NAT traversal
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create libp2p host: %w", err)
	}

	return &Host{host: node, config: cfg}, nil
}

// loadOrCreateIdentity loads an existing Ed25519 private key from disk,
// or generates a new one and persists it. This gives the node a stable
// peer ID across restarts.
func loadOrCreateIdentity(dataDir string) (crypto.PrivKey, error) {
	keyPath := filepath.Join(dataDir, "node.key")

	// Try to load existing key
	if data, err := os.ReadFile(keyPath); err == nil {
		privKey, err := crypto.UnmarshalPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal stored key: %w", err)
		}
		return privKey, nil
	}

	// Generate a new Ed25519 identity (as specified in the README)
	privKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate Ed25519 key: %w", err)
	}

	// Persist the key with user-only permissions
	data, err := crypto.MarshalPrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}
	if err := os.WriteFile(keyPath, data, 0600); err != nil {
		return nil, fmt.Errorf("failed to write identity key: %w", err)
	}

	return privKey, nil
}

// PeerID returns the node's peer ID
func (h *Host) PeerID() peer.ID {
	return h.host.ID()
}

// Underlying returns the underlying libp2p host
func (h *Host) Underlying() host.Host {
	return h.host
}

// Close closes the host and all its subsystems
func (h *Host) Close() error {
	return h.host.Close()
}

// ConnectToBootstrap connects to the configured bootstrap peers.
// Bootstrap addresses come from Config to avoid duplication with config.go.
func (h *Host) ConnectToBootstrap(ctx context.Context) error {
	bootstrapPeers := h.config.BootstrapPeers
	if len(bootstrapPeers) == 0 {
		return nil // No bootstrap peers configured (e.g. local testing)
	}

	var lastErr error
	for _, addr := range bootstrapPeers {
		if err := h.ConnectToPeer(ctx, addr); err != nil {
			lastErr = err
			continue
		}
		return nil // Connected to at least one bootstrap peer
	}
	if lastErr != nil {
		return fmt.Errorf("failed to connect to bootstrap peers: %v", lastErr)
	}
	return nil
}

// ConnectToPeer connects to a specific peer by multiaddress string
func (h *Host) ConnectToPeer(ctx context.Context, addr string) error {
	ma, err := multiaddr.NewMultiaddr(addr)
	if err != nil {
		return fmt.Errorf("failed to parse multiaddr %s: %w", addr, err)
	}

	info, err := peer.AddrInfoFromP2pAddr(ma)
	if err != nil {
		return fmt.Errorf("failed to extract peer info from %s: %w", addr, err)
	}

	h.host.Peerstore().AddAddrs(info.ID, info.Addrs, peerstore.TempAddrTTL)
	return h.host.Connect(ctx, *info)
}

// Addrs returns the list of listen addresses for this host
func (h *Host) Addrs() []multiaddr.Multiaddr {
	return h.host.Addrs()
}

// AddrInfo returns this host's AddrInfo (ID + listen addresses)
func (h *Host) AddrInfo() peer.AddrInfo {
	return peer.AddrInfo{
		ID:    h.host.ID(),
		Addrs: h.host.Addrs(),
	}
}

// NewStream opens a new stream to the given peer using the given protocol
func (h *Host) NewStream(ctx context.Context, p peer.ID, proto string) (interface{}, error) {
	stream, err := h.host.NewStream(ctx, p, protocol.ID(proto))
	if err != nil {
		return nil, fmt.Errorf("failed to create stream to %s: %w", p, err)
	}
	return stream, nil
}
