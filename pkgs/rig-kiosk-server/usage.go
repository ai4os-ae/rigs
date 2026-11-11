package main

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
)

// Usage is how full one resource is: memory, or the disk jobs write to.
type Usage struct {
	Used    uint64  `json:"used_bytes"`
	Total   uint64  `json:"total_bytes"`
	Percent float64 `json:"percent"`

	// Past fullThreshold. A rig whose disk fills stops taking jobs, and one
	// whose memory fills starts killing them, so this is worth a colour.
	Full bool `json:"full"`

	// What the page prints, assembled here for the same reason as
	// Temperature.Text: so the first render and the polled JSON agree.
	Text string `json:"text"`
}

const fullThreshold = 90

func newUsage(used, total uint64) *Usage {
	if total == 0 {
		return nil
	}
	u := &Usage{Used: used, Total: total, Percent: float64(used) / float64(total) * 100}
	u.Full = u.Percent >= fullThreshold
	u.Text = fmt.Sprintf("%s / %s · %.0f%%", formatBytes(used), formatBytes(total), u.Percent)
	return u
}

// memoryUsage reads /proc/meminfo. "Used" is MemTotal less MemAvailable, the
// kernel's own estimate of what could be handed out without swapping, so page
// cache — which a rig that has just built a kernel has gigabytes of — does not
// make an idle machine look full.
func memoryUsage() *Usage {
	return parseMeminfo(readTrimmed("/proc/meminfo"))
}

func parseMeminfo(raw string) *Usage {
	var total, available uint64
	var haveTotal, haveAvailable bool

	for line := range strings.SplitSeq(raw, "\n") {
		// "MemTotal:       32542140 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = kb*1024, true
		case "MemAvailable:":
			available, haveAvailable = kb*1024, true
		}
	}

	if !haveTotal || !haveAvailable || available > total {
		return nil
	}
	return newUsage(total-available, total)
}

// diskUsage is the filesystem holding `path`, counted the way df counts it:
// the blocks reserved for root are neither used nor available, so they are
// left out of the total and a disk reads 100% at the point writes start
// failing for everybody else. On a rig every subvolume shares one btrfs pool,
// so / is the whole disk, job outputs included.
func diskUsage(path string) *Usage {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return nil
	}

	size := uint64(fs.Bsize)
	used := (fs.Blocks - fs.Bfree) * size
	return newUsage(used, used+fs.Bavail*size)
}

// formatBytes renders a size in binary units, to one decimal place below 100
// of them and none above, which is as much precision as anybody reads off a
// screen across a room.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}

	value := float64(n)
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	i := -1
	for value >= unit && i < len(suffixes)-1 {
		value /= unit
		i++
	}

	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, suffixes[i])
	}
	return fmt.Sprintf("%.1f %s", value, suffixes[i])
}
