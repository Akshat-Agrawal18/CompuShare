// Package main is the CompuShare provider service.
// Runs in the background, contributing hardware to the network like BitTorrent seeding.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/compushare/compushare/execution/llamacpp"
	"github.com/compushare/compushare/internal/config"
	"github.com/compushare/compushare/internal/hardware"
	"github.com/compushare/compushare/karma"
	"github.com/compushare/compushare/model/torrent"
	"github.com/compushare/compushare/network/discovery"
	"github.com/compushare/compushare/network/libp2p"
	"github.com/compushare/compushare/network/tunnel"
	"github.com/compushare/compushare/pipeline"
	"github.com/compushare/compushare/storage"
	"go.uber.org/zap"
)

func main() {
	// Setup logging
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	sugar := logger.Sugar()

	// Parse flags
	flag.Parse()

	// Ensure data directory
	cfg := config.Parse()
	if err := cfg.EnsureDataDir(); err != nil {
		sugar.Fatalf("Failed to create data dir: %v", err)
	}

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		sugar.Info("Shutdown signal received...")
		cancel()
	}()

	// Detect hardware
	hw := hardware.DetectHardware()
	sugar.Info("Hardware detected",
		zap.String("gpu", hw.GPUModel),
		zap.Int("vram_gb", hw.VRAMGB),
		zap.Int("ram_gb", hw.RAMGB),
		zap.Bool("has_gpu", hw.HasGPU),
	)

	// Initialize P2P host
	sugar.Info("Initializing P2P networking...")
	host, err := libp2p.NewHost(ctx, cfg)
	if err != nil {
		sugar.Fatalf("Failed to create host: %v", err)
	}
	defer host.Close()

	// Initialize storage
	store := storage.NewContentStore(cfg.DataDir)
	if err := store.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start store: %v", err)
	}

	// Initialize karma reputer
	reputer := karma.NewReputer(host, cfg.DataDir)
	if err := reputer.Load(); err != nil {
		sugar.Warnf("Failed to load karma: %v", err)
	}

	// Initialize capability store and start advertisement
	var sidecar *llamacpp.Sidecar
	caps := discovery.NewCapabilityStore()
	if err := discovery.StartCapabilityAdvertisement(ctx, host, hw, caps, func() int {
		if sidecar != nil {
			return sidecar.GetPort()
		}
		return 0
	}); err != nil {
		sugar.Fatalf("Failed to start capability advertisement: %v", err)
	}

	// Connect to bootstrap peers
	if err := host.ConnectToBootstrap(ctx); err != nil {
		sugar.Warnf("Failed to connect to bootstrap peers: %v", err)
	}

	// Initialize executor (runs inference for other consumers)
	engine := pipeline.NewHEEngine(cfg.DataDir)
	executor := pipeline.NewExecutor(host, store, reputer, engine)
	if err := executor.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start executor: %v", err)
	}

	// Initialize and start llama.cpp sidecar
	sidecar = llamacpp.NewSidecar(cfg.DataDir)
	if err := sidecar.DownloadBinary(); err != nil {
		sugar.Warnf("Failed to download llama-rpc-server MVP binary: %v", err)
	}
	if err := sidecar.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start llama-rpc-server sidecar: %v", err)
	}
	defer sidecar.Stop()

	// Register port tunnel handler so consumers can reach the sidecar
	tunnel.RegisterTunnelHandler(host, sidecar.GetPort())

	// Initialize torrent seeder for model distribution
	pieceManager := torrent.NewPieceManager(host, store)
	seeder := torrent.NewSeeder(pieceManager)
	seeder.RegisterStreamHandler(host) // Register handler for torrent protocol
	seeder.AnnouncePiece("")           // Announce all local pieces

	// Propagate karma scores periodically
	go reputer.PropagateScores(ctx)

	// Periodically save karma (every 5 minutes)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Save on clean shutdown
				if err := reputer.Save(); err != nil {
					sugar.Warnf("Failed to save karma on shutdown: %v", err)
				}
				return
			case <-ticker.C:
				if err := reputer.Save(); err != nil {
					sugar.Warnf("Failed to save karma: %v", err)
				}
			}
		}
	}()

	sugar.Info("CompuShare provider running",
		zap.String("peer_id", host.PeerID().String()),
		zap.String("gpu", hw.GPUModel),
		zap.Int("vram_gb", hw.VRAMGB),
	)

	score := reputer.GetScore(host.PeerID())
	sugar.Infof("Your karma score: %.2f (total contributions: %d ms)",
		score.Karma, score.TotalContribution)

	sugar.Info("Provider is running in background. Press Ctrl+C to stop.")

	// Block until shutdown
	<-ctx.Done()
	sugar.Info("Provider shutdown complete")
}

// detectHardware detects available GPU and RAM


// validateHardware checks if this machine meets minimum requirements to be a provider
func validateHardware(hw config.HardwareInfo) error {
	// Must have either GPU or sufficient RAM
	if !hw.HasGPU && hw.RAMGB < 8 {
		return fmt.Errorf("insufficient hardware: need GPU or 8GB+ RAM")
	}
	return nil
}
