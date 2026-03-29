// Package main is the entry point for the CompuShare node binary.
// All node types (consumer, provider, seeder) share this same binary
// and auto-detect their role based on hardware and network state.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"github.com/compushare/compushare/execution/llamacpp"
	"github.com/compushare/compushare/internal/config"
	"github.com/compushare/compushare/internal/hardware"
	"github.com/compushare/compushare/karma"
	"github.com/compushare/compushare/network/discovery"
	"github.com/compushare/compushare/network/libp2p"
	"github.com/compushare/compushare/network/tunnel"
	"github.com/compushare/compushare/pipeline"
	"github.com/compushare/compushare/storage"
	"go.uber.org/zap"
)

func main() {
	// Initialize structured logging
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	sugar := logger.Sugar()

	// Parse command line flags
	cfg := config.Parse()

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		sugar.Info("Shutdown signal received, stopping node...")
		cancel()
	}()

	// Detect available hardware
	hw := hardware.DetectHardware()

	// Initialize libp2p host
	sugar.Info("Initializing P2P networking...")
	host, err := libp2p.NewHost(ctx, cfg)
	if err != nil {
		sugar.Fatalf("Failed to create libp2p host: %v", err)
	}
	defer host.Close()

	// Initialize testing variable to store sidecar port for provider/combined modes
	var sidecar *llamacpp.Sidecar
	
	// Initialize capability discovery
	capStore := discovery.NewCapabilityStore()
	if err := discovery.StartCapabilityAdvertisement(ctx, host, hw, capStore, func() int {
		if sidecar != nil {
			return sidecar.GetPort()
		}
		return 0
	}); err != nil {
		sugar.Fatalf("Failed to start capability advertisement: %v", err)
	}

	// Initialize storage layer (model chunks)
	store := storage.NewContentStore(cfg.DataDir)
	if err := store.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start content store: %v", err)
	}
	defer store.Stop()

	// Initialize karma reputer
	reputer := karma.NewReputer(host, cfg.DataDir)

	// Determine node role and start appropriate services
	switch determineRole(hw, cfg) {
	case "provider":
		sugar.Info("Starting as Provider node",
			zap.String("gpu", hw.GPUModel),
			zap.Int("vram_gb", hw.VRAMGB),
			zap.Int("ram_gb", hw.RAMGB),
			zap.String("peer_id", host.PeerID().String()),
		)
		sidecar = startProvider(ctx, cfg, host, store, reputer, sugar)

	case "consumer":
		sugar.Info("Starting as Consumer node",
			zap.String("peer_id", host.PeerID().String()),
		)
		startConsumer(ctx, host, store, reputer, sugar)

	default:
		sugar.Info("Starting as Combined node (Consumer + Provider)",
			zap.String("gpu", hw.GPUModel),
			zap.Int("vram_gb", hw.VRAMGB),
			zap.Int("ram_gb", hw.RAMGB),
			zap.String("peer_id", host.PeerID().String()),
		)
		sidecar = startCombined(ctx, cfg, host, store, reputer, sugar)
	}

	sugar.Info("CompuShare node running. Press Ctrl+C to stop.")
	<-ctx.Done()
	sugar.Info("Node shutdown complete")
}

// startProvider starts the provider services (seeding + inference execution)
func startProvider(ctx context.Context, cfg *config.Config, host *libp2p.Host, store *storage.ContentStore, reputer *karma.Reputer, sugar *zap.SugaredLogger) *llamacpp.Sidecar {
	// Start the inference executor (provider-side)
	engine := pipeline.NewHEEngine(cfg.DataDir)
	exec := pipeline.NewExecutor(host, store, reputer, engine)
	if err := exec.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start executor: %v", err)
	}
	
	sidecar := llamacpp.NewSidecar(cfg.DataDir)
	if err := sidecar.DownloadBinary(); err != nil {
		sugar.Warnf("Failed to download llama-rpc-server MVP binary: %v", err)
	}
	if err := sidecar.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start llama-rpc-server sidecar: %v", err)
	}
	defer sidecar.Stop()

	tunnel.RegisterTunnelHandler(host, sidecar.GetPort())

	<-ctx.Done()
	return sidecar
}

// startConsumer starts the consumer services (input encryption + result decryption + inference request)
func startConsumer(ctx context.Context, host *libp2p.Host, store *storage.ContentStore, reputer *karma.Reputer, sugar *zap.SugaredLogger) {
	// Start the consumer orchestrator
	orch := pipeline.NewOrchestrator(host, store, reputer)
	if err := orch.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start orchestrator: %v", err)
	}
	<-ctx.Done()
}

// startCombined starts both consumer and provider services on the same node
// This is the default mode - like BitTorrent where you both download and seed
func startCombined(ctx context.Context, cfg *config.Config, host *libp2p.Host, store *storage.ContentStore, reputer *karma.Reputer, sugar *zap.SugaredLogger) *llamacpp.Sidecar {
	engine := pipeline.NewHEEngine(cfg.DataDir)
	exec := pipeline.NewExecutor(host, store, reputer, engine)
	orch := pipeline.NewOrchestrator(host, store, reputer)

	if err := exec.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start executor: %v", err)
	}
	if err := orch.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start orchestrator: %v", err)
	}

	sidecar := llamacpp.NewSidecar(cfg.DataDir)
	if err := sidecar.DownloadBinary(); err != nil {
		sugar.Warnf("Failed to download llama-rpc-server MVP binary: %v", err)
	}
	if err := sidecar.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start llama-rpc-server sidecar: %v", err)
	}
	defer sidecar.Stop()

	tunnel.RegisterTunnelHandler(host, sidecar.GetPort())

	<-ctx.Done()
	return sidecar
}

// determineRole decides the node's role based on hardware and config
func determineRole(hw config.HardwareInfo, cfg *config.Config) string {
	if cfg.ForceMode != "" {
		return cfg.ForceMode
	}
	// Default: if GPU is available, be a combined node
	// CPU-only machines run as consumer only (can't serve inference)
	if !hw.HasGPU && hw.RAMGB < 8 {
		return "consumer"
	}
	return "combined"
}
