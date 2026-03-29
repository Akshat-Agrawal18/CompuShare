// Package pipeline handles distributed inference pipeline execution.
package pipeline

import (
	"github.com/compushare/compushare/network/discovery"
	"github.com/compushare/compushare/storage"
	"github.com/golang/glog"
	"github.com/libp2p/go-libp2p/core/peer"
)

// LayerAllocator handles splitting model layers across providers based on hardware resources.
type LayerAllocator struct {
	store *storage.ContentStore
}

// LayerAllocation represents how layers are split across providers
type LayerAllocation struct {
	Stages []PipelineStage
}

// PipelineStage represents a single stage in the pipeline
type PipelineStage struct {
	ProviderID peer.ID
	StartLayer int
	EndLayer   int
	RPCPort    int
}

// NewLayerAllocator creates a new layer allocator
func NewLayerAllocator(store *storage.ContentStore) *LayerAllocator {
	return &LayerAllocator{store: store}
}

// AllocateLayers splits model layers across providers based on VRAM/RAM capabilities.
func (la *LayerAllocator) AllocateLayers(providers []*discovery.CapabilityManifest, modelID string) *LayerAllocation {
	modelInfo, err := la.store.GetModelInfo(modelID)
	if err != nil {
		glog.Warningf("Model info not found (%v), using defaults", err)
		modelInfo = &storage.ModelInfo{TotalLayers: 32}
	}

	totalLayers := modelInfo.TotalLayers

	// Calculate total resource capacity (VRAM + RAM for now)
	totalCapacity := 0
	for _, p := range providers {
		// Use a simple heuristic: VRAM is 4x more valuable than RAM
		totalCapacity += (p.VRAMGB * 4) + p.RAMGB
	}

	alloc := &LayerAllocation{Stages: make([]PipelineStage, 0, len(providers))}

	currentLayer := 0
	for _, p := range providers {
		capacity := (p.VRAMGB * 4) + p.RAMGB
		// Percentage of total capacity
		ratio := float64(capacity) / float64(totalCapacity)
		layersForThisProvider := int(float64(totalLayers) * ratio)

		if layersForThisProvider == 0 {
			layersForThisProvider = 1 // Minimum 1 layer per provider if they are selected
		}

		endLayer := currentLayer + layersForThisProvider
		if endLayer > totalLayers {
			endLayer = totalLayers
		}

		peerID, _ := peer.Decode(p.PeerID)
		alloc.Stages = append(alloc.Stages, PipelineStage{
			ProviderID: peerID,
			StartLayer: currentLayer,
			EndLayer:   endLayer,
			RPCPort:    p.RPCPort,
		})

		currentLayer = endLayer
		if currentLayer >= totalLayers {
			break
		}
	}

	// Ensure all layers are covered
	if len(alloc.Stages) > 0 && alloc.Stages[len(alloc.Stages)-1].EndLayer < totalLayers {
		alloc.Stages[len(alloc.Stages)-1].EndLayer = totalLayers
	}

	return alloc
}
