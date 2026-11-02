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

	// What this rig can be asked for. Not every rig runs every kind — fuzzing
	// needs syzkaller and a kernel-fuzz expression pinned into the unit — so a
	// client should look rather than assume.
	Kinds []string `json:"kinds"`

	QueueDepth int      `json:"queue_depth"`
	Running    []string `json:"running"`
	Capacity   int      `json:"capacity"`

	// Fuzzing is reported separately because queue depth is the wrong question
	// for it. A rig with an empty queue whose only fuzz slot is four hours into
	// a campaign is not available for another one, and a broker that reads
	// queue_depth alone would place one there anyway.
	FuzzQueueDepth int `json:"fuzz_queue_depth"`
	FuzzSlotsFree  int `json:"fuzz_slots_free"`
	FuzzCapacity   int `json:"fuzz_capacity"`

	Load           []float64 `json:"load"`
	UptimeSeconds  int64     `json:"uptime_seconds"`
	StoreFreeBytes uint64    `json:"store_free_bytes"`

	Mirror map[string]any `json:"mirror"`
}

func collectStatus(runner *Runner, mirror *Mirror) Status {
	hostname, _ := os.Hostname()

	kinds := []string{string(KindBuild), string(KindTest)}
	if runner.cfg.Limits.FuzzEnabled {
		kinds = append(kinds, string(KindFuzz))
	}

	return Status{
		Hostname:       hostname,
		System:         nixSystem(),
		Time:           time.Now().UTC(),
		Kinds:          kinds,
		QueueDepth:     runner.QueueDepth(),
		Running:        runner.Running(),
		Capacity:       runner.cfg.Capacity,
		FuzzQueueDepth: runner.FuzzQueueDepth(),
		FuzzSlotsFree:  runner.FuzzSlotsFree(),
		FuzzCapacity:   runner.cfg.FuzzCapacity,
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

// numCPU and totalMemoryMB are what sizeVMs works from: a fuzz job's VM pool
// is sized against the machine rather than against the request, because a pool
// that overcommits memory does not fail, it thrashes.
//
// runtime.NumCPU rather than /proc/cpuinfo because it respects a CPU affinity
// mask, and a daemon confined by systemd to a subset of the machine should
// size its VMs against the subset it actually has.
func numCPU() int { return runtime.NumCPU() }

func totalMemoryMB() int {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return int(kb / 1024)
	}
	return 0
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
