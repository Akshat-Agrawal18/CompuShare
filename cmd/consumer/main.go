// Package main is the CompuShare consumer CLI.
// Allows users to request AI inference from the distributed network.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"github.com/compushare/compushare/internal/config"
	"github.com/compushare/compushare/karma"
	"github.com/compushare/compushare/network/libp2p"
	"github.com/compushare/compushare/pipeline"
	"github.com/compushare/compushare/storage"
	"go.uber.org/zap"
)

func main() {
	modelID := flag.String("model", "tinyllama-1b", "Model to use (tinyllama-1b, llama-7b, llama-13b)")
	maxTokens := flag.Int("max-tokens", 256, "Maximum tokens to generate")
	temperature := flag.Float64("temperature", 0.7, "Sampling temperature (0.0-1.0)")
	interactive := flag.Bool("interactive", false, "Run in interactive chat mode")
	flag.Parse()

	logger, _ := zap.NewProduction()
	defer logger.Sync()
	sugar := logger.Sugar()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigChan; cancel() }()

	// HE keys temporarily disabled for Phase 1 MVP
	sugar.Info("Not generating HE keys - running Phase 1 plaintext model pipeline")

	// Initialize P2P host
	cfg := config.Parse()
	host, err := libp2p.NewHost(ctx, cfg)
	if err != nil {
		sugar.Fatalf("Failed to create P2P host: %v", err)
	}
	defer host.Close()
	sugar.Infow("P2P host started", "peer_id", host.PeerID().String())

	// Initialize storage and karma
	store := storage.NewContentStore(cfg.DataDir)
	if err := store.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start content store: %v", err)
	}
	reputer := karma.NewReputer(host, cfg.DataDir)

	// Initialize orchestrator
	orch := pipeline.NewOrchestrator(host, store, reputer)
	if err := orch.Start(ctx); err != nil {
		sugar.Fatalf("Failed to start orchestrator: %v", err)
	}

	sugar.Info("CompuShare consumer ready")


	if *interactive {
		runInteractive(ctx, sugar, orch, *modelID, *maxTokens, *temperature)
	} else {
		prompt := strings.Join(flag.Args(), " ")
		if prompt == "" {
			sugar.Fatal("Provide a prompt as argument, or use --interactive")
		}
		runInference(ctx, sugar, orch, prompt, *modelID, *maxTokens, *temperature)
	}
}

func runInteractive(ctx context.Context, sugar *zap.SugaredLogger, orch *pipeline.Orchestrator, modelID string, maxTokens int, temperature float64) {

	fmt.Println("CompuShare Interactive Mode — type 'exit' to quit")
	fmt.Println("---")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("You: ")
		if !scanner.Scan() {
			break
		}
		prompt := strings.TrimSpace(scanner.Text())
		if prompt == "" {
			continue
		}
		if prompt == "exit" || prompt == "quit" {
			break
		}
		runInference(ctx, sugar, orch, prompt, modelID, maxTokens, temperature)
	}
}

func runInference(ctx context.Context, sugar *zap.SugaredLogger, orch *pipeline.Orchestrator, prompt, modelID string, maxTokens int, temperature float64) {

	sugar.Infof("Requesting inference: %q", truncate(prompt, 60))

	req := &pipeline.InferenceRequest{
		ModelID:      modelID,
		InputText:    prompt,
		MaxTokens:    maxTokens,
		Temperature:  temperature,
		NumProviders: 3,
		RequestID:    fmt.Sprintf("req-%d", time.Now().UnixNano()),
		StreamCallback: func(token string) {
			fmt.Print(token)
		},
	}

	start := time.Now()
	fmt.Print("\nAI: ")
	result, err := orch.RequestInference(ctx, req)
	if err != nil {
		fmt.Printf("\n")
		sugar.Errorf("Inference failed: %v", err)
		return
	}

	fmt.Printf("\n\n")
	sugar.Infof("Done in %dms | %d providers", 
		time.Since(start).Milliseconds(), len(result.ProvidersUsed))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
