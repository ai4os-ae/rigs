package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Temperature is how hot the CPU is, for the one row on the page that says
// whether a rig running a build is coping with it.
//
// WHICH SENSOR. A laptop offers a dozen of them and most are the wrong answer.
// Measured on a rig-class Intel laptop, idle and then with every thread busy:
//
//	coretemp "Package id 0"   54 -> 72 °C     the CPU's own die sensor
//	thermal zone x86_pkg_temp 54 -> 72 °C     the same register, other interface
//	thermal zone acpitz       49 -> 70 °C     a board sensor, and it lags
//	thermal zone SEN1..SEN5   varies          skin sensors, for fan policy
//	hwmon nvme "Composite"    47 -> 47 °C     the disk. Does not move.
//	hwmon iwlwifi             55 -> 57 °C     the wifi card. Does not move.
//
// So: the package sensor, which is the one that answers "is this machine hot
// because of the work it is doing". The others are either measuring something
// else entirely or measuring the same thing later.
//
// Nothing here parses `sensors` or shells out. These are four small reads of
// /sys on a page that polls every five seconds.
type Temperature struct {
	Celsius  float64 `json:"celsius"`
	Source   string  `json:"source"`
	Critical float64 `json:"critical,omitempty"`

	// Whether the CPU is *currently* pulling its own clocks down to stay
	// within thermal limits. This is the reading that actually matters to
	// somebody looking at a rig: a hot machine is fine, a throttling machine
	// is doing less work per hour than the queue depth suggests it is.
	Throttling bool `json:"throttling"`

	// What the page prints. Assembled here rather than in the template so the
	// polled JSON and the first render cannot drift apart.
	Text string `json:"text"`
}

// sysfsRoot is where the sensors are, and is a variable only so that the
// throttling logic can be exercised against a directory of fixtures. Nothing
// sets it but the tests: a rig has exactly one /sys.
var sysfsRoot = "/"

// sources is the search order. The first one that reads is used.
//
// coretemp and k10temp are the vendors' own die sensors and are named by
// label, because the same chip reports per-core temperatures beside the
// package one and the per-core figures are noisier and no more useful.
// x86_pkg_temp is the same Intel register reached through the thermal
// framework, and is here because coretemp is a module that a stripped-down
// kernel may not have loaded. acpitz is last: it is firmware's idea of the
// board temperature, it is not the CPU, and on some machines it is a constant.
var sources = []struct {
	hwmon string // hwmon driver name, or "" for a thermal zone
	label string // the temp*_label to match, for hwmon
	zone  string // the thermal zone type, for a zone
	name  string
}{
	{hwmon: "coretemp", label: "Package id 0", name: "CPU package"},
	{hwmon: "k10temp", label: "Tdie", name: "CPU die"},
	{hwmon: "k10temp", label: "Tctl", name: "CPU"},
	{zone: "x86_pkg_temp", name: "CPU package"},
	{zone: "acpitz", name: "board"},
}

// thermal holds the one thing a single read cannot answer.
//
// The throttle counters in /sys are totals since boot, so their value says
// only that a rig was too hot at some point in the last three weeks — which is
// not what anybody wants on a screen. What matters is whether it is happening
// now, and that is a difference between two reads. Hence a little state, and a
// mutex, because the page and /status.json are separate requests.
type thermal struct {
	mu       sync.Mutex
	seen     bool
	lastMS   uint64
	lastRise time.Time
}

// throttlingWindow is how long after the counter last moved the page keeps
// saying so. Comfortably longer than the poll interval, so that throttling
// which stops and starts reads as continuous rather than flickering, and short
// enough that it clears on its own once the machine cools.
const throttlingWindow = 30 * time.Second

func (t *thermal) read(now time.Time) *Temperature {
	celsius, critical, source, ok := readPackageTemp()
	if !ok {
		// A rig with no sensor the kernel exposes. The page leaves the row out
		// rather than showing a permanently blank fact, as it does for jobs.
		return nil
	}

	temp := &Temperature{
		Celsius:    celsius,
		Critical:   critical,
		Source:     source,
		Throttling: t.throttling(now),
	}

	temp.Text = fmt.Sprintf("%.0f °C", temp.Celsius)
	if temp.Throttling {
		temp.Text += " · throttling"
	}
	return temp
}

// throttling reports whether the package throttle counter has moved recently.
func (t *thermal) throttling(now time.Time) bool {
	total, ok := throttleTotalMS()
	if !ok {
		// AMD parts and virtual machines have no such counter. Absent is not
		// false, but there is nothing to report either way, and claiming a rig
		// is throttling on no evidence is worse than staying quiet.
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// The first read establishes a baseline and asserts nothing: the counter
	// is already large on a machine that has been up for a month, and treating
	// that as "throttling now" would light the page up on every restart.
	if !t.seen {
		t.seen, t.lastMS = true, total
		return false
	}

	if total > t.lastMS {
		t.lastMS, t.lastRise = total, now
	}
	return !t.lastRise.IsZero() && now.Sub(t.lastRise) < throttlingWindow
}

// readPackageTemp walks `sources` and returns the first reading it gets, in
// degrees Celsius. /sys reports thousandths.
func readPackageTemp() (celsius, critical float64, source string, ok bool) {
	for _, want := range sources {
		if want.zone != "" {
			if milli, found := readZone(want.zone); found {
				return float64(milli) / 1000, 0, want.name, true
			}
			continue
		}

		if input, found := findHwmon(want.hwmon, want.label); found {
			milli, err := readMilli(input + "_input")
			if err != nil {
				continue
			}
			// _crit is the temperature at which the firmware will cut power.
			// Absent on plenty of sensors, and not worth failing over.
			crit, _ := readMilli(input + "_crit")
			return float64(milli) / 1000, float64(crit) / 1000, want.name, true
		}
	}
	return 0, 0, "", false
}

// findHwmon returns the temp<N> prefix under the named driver whose label
// matches, e.g. /sys/class/hwmon/hwmon5/temp1 for coretemp's package sensor.
func findHwmon(driver, label string) (string, bool) {
	entries, err := filepath.Glob(filepath.Join(sysfsRoot, "sys/class/hwmon/hwmon*"))
	if err != nil {
		return "", false
	}

	for _, dir := range entries {
		if readTrimmed(filepath.Join(dir, "name")) != driver {
			continue
		}

		labels, err := filepath.Glob(filepath.Join(dir, "temp*_label"))
		if err != nil {
			continue
		}
		for _, path := range labels {
			if readTrimmed(path) == label {
				return strings.TrimSuffix(path, "_label"), true
			}
		}
	}
	return "", false
}

func readZone(kind string) (int64, bool) {
	zones, err := filepath.Glob(filepath.Join(sysfsRoot, "sys/class/thermal/thermal_zone*"))
	if err != nil {
		return 0, false
	}

	for _, dir := range zones {
		if readTrimmed(filepath.Join(dir, "type")) != kind {
			continue
		}
		milli, err := readMilli(filepath.Join(dir, "temp"))
		if err != nil {
			continue
		}
		return milli, true
	}
	return 0, false
}

// throttleTotalMS is the longest package-throttle total any CPU reports.
//
// The counter is per package and replicated to every CPU in it, so the maximum
// over all of them is the right reduction for a machine with more than one
// socket and is the same number as cpu0's on a machine with one.
func throttleTotalMS() (uint64, bool) {
	paths, err := filepath.Glob(filepath.Join(sysfsRoot, "sys/devices/system/cpu/cpu*/thermal_throttle/package_throttle_total_time_ms"))
	if err != nil || len(paths) == 0 {
		return 0, false
	}

	var highest uint64
	var found bool
	for _, path := range paths {
		value, err := strconv.ParseUint(readTrimmed(path), 10, 64)
		if err != nil {
			continue
		}
		found = true
		if value > highest {
			highest = value
		}
	}
	return highest, found
}

func readMilli(path string) (int64, error) {
	return strconv.ParseInt(readTrimmed(path), 10, 64)
}

func readTrimmed(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
