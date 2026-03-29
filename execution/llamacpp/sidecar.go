package llamacpp

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/golang/glog"
)

// Sidecar manages the llama-rpc-server subprocess.
type Sidecar struct {
	binaryPath string
	port       int
	cmd        *exec.Cmd
	cancelFunc context.CancelFunc
}

// NewSidecar creates a new Sidecar instance.
func NewSidecar(dataDir string) *Sidecar {
	return &Sidecar{
		binaryPath: filepath.Join(dataDir, "bin", getBinaryName()),
	}
}

// FindFreePort finds an available TCP port on localhost.
func FindFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// DownloadBinary downloads the llama-rpc-server binary from GitHub.
// For the MVP, it downloads a known working release.
func (s *Sidecar) DownloadBinary() error {
	if _, err := os.Stat(s.binaryPath); err == nil {
		glog.Infof("llama-rpc-server binary already exists at %s", s.binaryPath)
		return nil
	}

	glog.Infof("Downloading llama-rpc-server binary to %s...", s.binaryPath)
	if err := os.MkdirAll(filepath.Dir(s.binaryPath), 0755); err != nil {
		return fmt.Errorf("failed to create bin directory: %w", err)
	}

	url, isZip := getReleaseURL()
	if url == "" {
		return fmt.Errorf("unsupported OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status code downloading binary: %d", resp.StatusCode)
	}

	if isZip {
		// Download zip to temp file and extract
		tmpFile, err := os.CreateTemp("", "llama-*.zip")
		if err != nil {
			return err
		}
		defer os.Remove(tmpFile.Name())

		_, err = io.Copy(tmpFile, resp.Body)
		if err != nil {
			return err
		}
		tmpFile.Close()

		if err := extractZipBinary(tmpFile.Name(), s.binaryPath); err != nil {
			return err
		}
	} else {
		// Direct download (e.g., Linux/Mac binary)
		out, err := os.OpenFile(s.binaryPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("failed to create binary file: %w", err)
		}
		defer out.Close()

		if _, err := io.Copy(out, resp.Body); err != nil {
			return fmt.Errorf("failed to write binary: %w", err)
		}
	}

	// Ensure executable permissions
	if runtime.GOOS != "windows" {
		if err := os.Chmod(s.binaryPath, 0755); err != nil {
			return fmt.Errorf("failed to make binary executable: %w", err)
		}
	}

	glog.Infof("Successfully downloaded llama-rpc-server")
	return nil
}

// Start spawns the llama-rpc-server subprocess.
func (s *Sidecar) Start(ctx context.Context) error {
	port, err := FindFreePort()
	if err != nil {
		return fmt.Errorf("failed to find free port: %w", err)
	}
	s.port = port

	ctx, cancel := context.WithCancel(ctx)
	s.cancelFunc = cancel

	go s.monitorLoop(ctx)

	return nil
}

// monitorLoop keeps the sidecar running and restarts it if it crashes.
func (s *Sidecar) monitorLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		glog.Infof("Starting llama-rpc-server on port %d...", s.port)
		s.cmd = exec.CommandContext(ctx, s.binaryPath, "--host", "127.0.0.1", "--port", fmt.Sprintf("%d", s.port))
		
		s.cmd.Stdout = os.Stdout
		s.cmd.Stderr = os.Stderr

		if err := s.cmd.Start(); err != nil {
			glog.Errorf("Failed to start llama-rpc-server: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		err := s.cmd.Wait()
		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled, expected termination
				return
			}
			glog.Errorf("llama-rpc-server exited with error: %v", err)
		} else {
			glog.Infof("llama-rpc-server exited cleanly")
		}

		// Prevent tight restart loop
		time.Sleep(2 * time.Second)
	}
}

// Stop gracefully kills the subprocess and cleans up.
func (s *Sidecar) Stop() {
	if s.cancelFunc != nil {
		s.cancelFunc()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		// Try to send SIGTERM nicely if possible, else it is killed by Context
		s.cmd.Process.Kill()
	}
}

// GetPort returns the port the sidecar is listening on.
func (s *Sidecar) GetPort() int {
	return s.port
}

// Helper functions

func getBinaryName() string {
	if runtime.GOOS == "windows" {
		return "llama-rpc-server.exe"
	}
	return "llama-rpc-server"
}

// getReleaseURL returns the download URL for the target OS/Arch and whether it's a zip.
func getReleaseURL() (string, bool) {
	// For MVP, we use the b4419 release of llama.cpp as an example reliable version
	const baseURL = "https://github.com/ggerganov/llama.cpp/releases/download/b4419"

	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		return fmt.Sprintf("%s/llama-b4419-bin-win-avx2-x64.zip", baseURL), true
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		// Note: llama.cpp usually distributes full zip files even on Linux in newer releases
		// For simplicity we'll assume a Linux zip release is available or use a pre-built static binary
		// In a real scenario, you'd want to pull the linux binary correctly.
		return fmt.Sprintf("%s/llama-b4419-bin-ubuntu-x64.zip", baseURL), true
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return fmt.Sprintf("%s/llama-b4419-bin-macos-arm64.zip", baseURL), true
	}
	return "", false
}

// extractZipBinary extracts the specified binary from a zip file.
func extractZipBinary(zipPath, outPath string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	binName := getBinaryName()
	var binFile *zip.File

	for _, f := range r.File {
		if strings.HasSuffix(f.Name, binName) {
			binFile = f
			break
		}
	}

	if binFile == nil {
		return fmt.Errorf("could not find %s in archive", binName)
	}

	src, err := binFile.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
}
