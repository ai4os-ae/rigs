package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Status is what a client reads to decide whether to send this rig a job.
// Everything here is either free to compute or already in memory: it is polled.
type Status struct {
	Hostname string    `json:"hostname"`
	Time     time.Time `json:"time"`

	// Nix's spelling of the machine — "x86_64-linux". A broker matches a job's
	// requested system against this rather than assuming every rig is the
	// same architecture, which stops being true the moment an arm board joins.
	System string `json:"system"`

	// What this rig can be asked for. Fuzz runs will appear here when their
	// backend does; a client should look rather than assume.
	Kinds []string `json:"kinds"`

	QueueDepth int      `json:"queue_depth"`
	Running    []string `json:"running"`
	Capacity   int      `json:"capacity"`

	Load           []float64 `json:"load"`
	UptimeSeconds  int64     `json:"uptime_seconds"`
	StoreFreeBytes uint64    `json:"store_free_bytes"`

	Mirror map[string]any `json:"mirror"`
}

func collectStatus(runner *Runner, mirror *Mirror) Status {
	hostname, _ := os.Hostname()

	return Status{
		Hostname:       hostname,
		System:         nixSystem(),
		Time:           time.Now().UTC(),
		Kinds:          []string{string(KindBuild), string(KindTest)},
		QueueDepth:     runner.QueueDepth(),
		Running:        runner.Running(),
		Capacity:       runner.capacity,
		Load:           loadAverage(),
		UptimeSeconds:  uptime(),
		StoreFreeBytes: freeBytes("/nix/store"),
		Mirror:         mirror.Status(),
	}
}

// nixSystem renders this machine the way Nix names it. Go and Nix disagree on
// the spelling of every architecture that matters here, so the mapping is
// explicit rather than inferred.
func nixSystem() string {
	arch := map[string]string{
		"amd64":   "x86_64",
		"arm64":   "aarch64",
		"riscv64": "riscv64",
	}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	return arch + "-" + runtime.GOOS
}

func loadAverage() []float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return nil
	}
	out := make([]float64, 0, 3)
	for _, field := range fields[:3] {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil
		}
		out = append(out, value)
	}
	return out
}

func uptime() int64 {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(seconds)
}

// freeBytes reports space available on the filesystem holding path. Worth
// serving: a kernel build needs several gigabytes of scratch plus its output,
// and a rig that is nearly full should not be sent a job.
func freeBytes(path string) uint64 {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0
	}
	return fs.Bavail * uint64(fs.Bsize)
}
