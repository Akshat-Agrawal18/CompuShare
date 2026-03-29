// Package discovery handles peer discovery and capability advertisement
// using GossipSub pub/sub messaging.
package discovery

import (
	"context"
	"encoding/json"
	"time"

	"github.com/compushare/compushare/internal/config"
	cshost "github.com/compushare/compushare/network/libp2p"
	"github.com/golang/glog"
	"github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

// CapabilityTopic is the GossipSub topic for capability advertisement
const CapabilityTopic = "compushare:providers"

// CapabilityManifest represents a node's advertised capabilities
type CapabilityManifest struct {
	PeerID        string `json:"peer_id"`
	Timestamp     int64  `json:"timestamp"`
	GPUModel      string `json:"gpu_model"`
	VRAMGB        int    `json:"vram_gb"`
	RAMGB         int    `json:"ram_gb"`
	BandwidthMbps int    `json:"bandwidth_mbps"`
	Region        string `json:"region"`
	IsProvider    bool   `json:"is_provider"`
	RPCPort       int    `json:"rpc_port"`
}

// CapabilityStore stores and manages discovered provider capabilities
type CapabilityStore struct {
	providers  map[peer.ID]*CapabilityManifest
	timestamps map[peer.ID]time.Time
}

// NewCapabilityStore creates a new capability store
func NewCapabilityStore() *CapabilityStore {
	return &CapabilityStore{
		providers:  make(map[peer.ID]*CapabilityManifest),
		timestamps: make(map[peer.ID]time.Time),
	}
}

// GetProviders returns all known providers
func (cs *CapabilityStore) GetProviders() []*CapabilityManifest {
	providers := make([]*CapabilityManifest, 0, len(cs.providers))
	for _, cap := range cs.providers {
		providers = append(providers, cap)
	}
	return providers
}

// UpdateProvider updates or adds a provider's capabilities
func (cs *CapabilityStore) UpdateProvider(peerID peer.ID, cap *CapabilityManifest) {
	cs.providers[peerID] = cap
	cs.timestamps[peerID] = time.Now()
}

// RemoveProvider removes a provider from the store
func (cs *CapabilityStore) RemoveProvider(peerID peer.ID) {
	delete(cs.providers, peerID)
	delete(cs.timestamps, peerID)
}

// CleanupStaleProviders removes providers that haven't advertised recently
func (cs *CapabilityStore) CleanupStaleProviders(maxAge time.Duration) {
	now := time.Now()
	for peerID, ts := range cs.timestamps {
		if now.Sub(ts) > maxAge {
			cs.RemoveProvider(peerID)
		}
	}
}

// FindProvidersForTask finds providers that meet resource requirements
func (cs *CapabilityStore) FindProvidersForTask(minVRAM, minRAM, count int) []*CapabilityManifest {
	var candidates []*CapabilityManifest
	for _, cap := range cs.providers {
		if !cap.IsProvider {
			continue
		}
		if cap.VRAMGB >= minVRAM && cap.RAMGB >= minRAM {
			candidates = append(candidates, cap)
		}
	}
	if len(candidates) > count {
		candidates = candidates[:count]
	}
	return candidates
}

// StartCapabilityAdvertisement starts publishing and subscribing to capability manifests.
// rpcPortFetcher is a callback to get the dynamically assigned RPC port (returns 0 if not a provider).
func StartCapabilityAdvertisement(ctx context.Context, host *cshost.Host, hardware config.HardwareInfo, store *CapabilityStore, rpcPortFetcher func() int) error {
	// Create GossipSub instance
	ps, err := pubsub.NewGossipSub(ctx, host.Underlying())
	if err != nil {
		return err
	}

	topic, err := ps.Join(CapabilityTopic)
	if err != nil {
		return err
	}

	// Build this node's manifest
	manifest := &CapabilityManifest{
		PeerID:        host.PeerID().String(),
		Timestamp:     time.Now().Unix(),
		GPUModel:      hardware.GPUModel,
		VRAMGB:        hardware.VRAMGB,
		RAMGB:         hardware.RAMGB,
		BandwidthMbps: 100,
		Region:        "unknown",
		IsProvider:    hardware.HasGPU || hardware.RAMGB >= 8,
	}

	// Publish our capabilities periodically
	go publishCapabilities(ctx, topic, manifest, rpcPortFetcher)

	// Subscribe to capability updates from others
	return subscribeCapabilities(ctx, topic, store)
}

func publishCapabilities(ctx context.Context, topic *pubsub.Topic, manifest *CapabilityManifest, getPort func() int) {
	// Publish immediately on start, then every 60 seconds
	publish := func() {
		manifest.Timestamp = time.Now().Unix()
		if getPort != nil {
			manifest.RPCPort = getPort()
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			glog.Errorf("Failed to marshal capability manifest: %v", err)
			return
		}
		if err := topic.Publish(ctx, data); err != nil {
			glog.V(4).Infof("Failed to publish capability: %v", err)
		}
	}

	publish()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

func subscribeCapabilities(ctx context.Context, topic *pubsub.Topic, store *CapabilityStore) error {
	sub, err := topic.Subscribe()
	if err != nil {
		return err
	}

	go func() {
		for {
			msg, err := sub.Next(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return // context cancelled
				}
				glog.Errorf("Subscription error: %v", err)
				continue
			}
			handleCapabilityMessage(msg, store)
		}
	}()

	return nil
}

func handleCapabilityMessage(msg *pubsub.Message, store *CapabilityStore) {
	var manifest CapabilityManifest
	if err := json.Unmarshal(msg.Data, &manifest); err != nil {
		glog.V(4).Infof("Failed to unmarshal capability manifest: %v", err)
		return
	}

	peerID, err := peer.Decode(manifest.PeerID)
	if err != nil {
		glog.V(4).Infof("Failed to decode peer ID %s: %v", manifest.PeerID, err)
		return
	}

	// Validate that the sender matches the claimed peer ID (prevents spoofing)
	if msg.ReceivedFrom != peerID {
		glog.V(4).Infof("Capability manifest peer ID mismatch: claimed=%s, received_from=%s",
			manifest.PeerID, msg.ReceivedFrom)
		return
	}

	// Sanity-check hardware claims to reject fraudulent manifests
	const maxReasonableVRAMGB = 10000  // no single machine has > 10TB VRAM
	const maxReasonableRAMGB = 100000  // no single machine has > 100TB RAM
	if manifest.VRAMGB < 0 || manifest.VRAMGB > maxReasonableVRAMGB {
		glog.V(4).Infof("Rejecting manifest from %s: implausible VRAM=%dGB", peerID, manifest.VRAMGB)
		return
	}
	if manifest.RAMGB < 0 || manifest.RAMGB > maxReasonableRAMGB {
		glog.V(4).Infof("Rejecting manifest from %s: implausible RAM=%dGB", peerID, manifest.RAMGB)
		return
	}
	if manifest.BandwidthMbps < 0 {
		manifest.BandwidthMbps = 0
	}

	store.UpdateProvider(peerID, &manifest)
	glog.V(4).Infof("Updated capability for peer %s: GPU=%s VRAM=%dGB RAM=%dGB",
		peerID.String()[:12]+"...", manifest.GPUModel, manifest.VRAMGB, manifest.RAMGB)
}
