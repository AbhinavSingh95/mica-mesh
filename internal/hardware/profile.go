// Package hardware provides best-effort diagnostic metadata, never admission policy.
package hardware

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

// Profile always returns a nonnil snapshot. Optional unavailable fields are
// absent. Native metadata has one total bound; no GPU profiler runs at startup.
func Profile(ctx context.Context) *meshv1.HardwareInfo {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}
	cores := uint32(runtime.NumCPU())
	p := &meshv1.HardwareInfo{Hostname: hostname, Architecture: runtime.GOARCH, CpuCores: &cores}
	if runtime.GOOS != "darwin" {
		return p
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	query := func(key string) string {
		data, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", key).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	if cpu := query("machdep.cpu.brand_string"); cpu != "" {
		p.Cpu = &cpu
	}
	if ram, err := strconv.ParseUint(query("hw.memsize"), 10, 64); err == nil && ram > 0 {
		p.RamBytes = &ram
	}
	// Apple silicon has unified memory; do not report RAM as dedicated GPU memory.
	if runtime.GOARCH == "arm64" {
		unified := true
		p.UnifiedMemory = &unified
	}
	return p
}
