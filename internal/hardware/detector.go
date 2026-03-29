package hardware

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/compushare/compushare/internal/config"
)

// DetectHardware interrogates the system for AI capabilities (RAM, GPU, VRAM)
func DetectHardware() config.HardwareInfo {
	hw := config.HardwareInfo{
		RAMGB: getSystemRAMGB(),
	}

	// 1. Try NVIDIA GPU via nvidia-smi
	if gpuModel, vramGB, cudaVer, ok := detectNvidiaGPU(); ok {
		hw.GPUModel = gpuModel
		if cudaVer != "" {
			hw.GPUModel += " (CUDA " + cudaVer + ")"
		}
		hw.VRAMGB = vramGB
		hw.HasGPU = true
		return hw
	}

	// 2. Try AMD ROCm
	if gpuModel, vramGB, ok := detectROCm(); ok {
		hw.GPUModel = gpuModel + " (ROCm)"
		hw.VRAMGB = vramGB
		hw.HasGPU = true
		return hw
	}

	// 3. Try DirectML as fallback (for AMD/Intel GPUs on Windows)
	if ok := detectDirectML(); ok {
		hw.GPUModel = "DirectML Compatible"
		hw.VRAMGB = hw.RAMGB / 4 // Conservative estimate, user should manually configure if needed
		hw.HasGPU = true
		return hw
	}

	// 4. CPU-only fallback
	hw.GPUModel = "CPU-only"
	hw.VRAMGB = 0
	return hw
}

// memoryStatusEx is the Windows MEMORYSTATUSEX structure.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// getSystemRAMGB returns total system RAM in GB.
func getSystemRAMGB() int {
	kernel32, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		// Non-Windows fallback
		return 16
	}
	defer kernel32.Release()
	
	proc, err := kernel32.FindProc("GlobalMemoryStatusEx")
	if err != nil {
		return 16
	}
	
	msx := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&msx)))
	if ret == 0 {
		return 16 // fallback
	}
	return int(msx.totalPhys / (1024 * 1024 * 1024))
}

// detectNvidiaGPU uses nvidia-smi to detect NVIDIA GPU and CUDA version.
func detectNvidiaGPU() (model string, vramGB int, cudaVer string, ok bool) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader").Output()
	if err != nil {
		return "", 0, "", false
	}
	
	// Also get CUDA driver version
	cudaOut, err := exec.Command("nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader").Output()
	if err == nil {
		cudaVer = strings.TrimSpace(string(cudaOut))
	}
	
	line := strings.TrimSpace(string(out))
	parts := strings.SplitN(line, ",", 2)
	if len(parts) != 2 {
		return "", 0, cudaVer, false
	}
	
	model = strings.TrimSpace(parts[0])
	memStr := strings.TrimSpace(parts[1])
	memFields := strings.Fields(memStr)
	if len(memFields) < 1 {
		return model, 0, cudaVer, true
	}
	
	mib, err := strconv.Atoi(memFields[0])
	if err != nil {
		return model, 0, cudaVer, true
	}
	
	vramGB = mib / 1024
	return model, vramGB, cudaVer, true
}

func detectROCm() (model string, vramGB int, ok bool) {
	out, err := exec.Command("rocm-smi", "--showproductname").Output()
	if err != nil {
		return "", 0, false
	}
	// Very simple heuristic for ROCm parsing
	line := strings.TrimSpace(string(out))
	if strings.Contains(line, "Card") || strings.Contains(line, "AMD") {
		return "AMD ROCm Supported Device", 8, true // naive assumption
	}
	return "", 0, false
}

// detectDirectML checks for DirectML availability on Windows.
func detectDirectML() bool {
	_, err := os.Stat(`C:\Windows\System32\DirectML.dll`)
	return err == nil
}
