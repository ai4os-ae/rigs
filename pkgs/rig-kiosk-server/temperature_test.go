package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Throttling is the one thing here that cannot be checked by looking at a
// running rig: it needs a machine that is currently too hot, and a laptop on a
// desk will not oblige on request. It is also the part most worth getting
// right — a page that says "throttling" whenever the daemon restarts, or that
// never says it at all, is worse than a page that shows the number alone.
//
// So the sensors are pointed at a directory instead, and the counter is moved
// by hand.

// fakeSysfs writes a package throttle counter under a temporary root and
// points the readers at it. Returns a function that sets the counter.
func fakeSysfs(t *testing.T, initial string) func(string) {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "sys/devices/system/cpu/cpu0/thermal_throttle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(dir, "package_throttle_total_time_ms")

	previous := sysfsRoot
	sysfsRoot = root
	t.Cleanup(func() { sysfsRoot = previous })

	set := func(value string) {
		if err := os.WriteFile(counter, []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	set(initial)
	return set
}

// A rig that has been up for weeks has a large counter and is not necessarily
// hot now. The first read must not report throttling on the strength of it.
func TestFirstReadIsOnlyABaseline(t *testing.T) {
	fakeSysfs(t, "421337")

	sensors := &thermal{}
	if sensors.throttling(time.Now()) {
		t.Fatal("a first read of a large counter reported throttling")
	}
}

func TestThrottlingFollowsTheCounter(t *testing.T) {
	set := fakeSysfs(t, "1000")
	sensors := &thermal{}
	now := time.Now()

	sensors.throttling(now) // baseline

	if sensors.throttling(now.Add(5 * time.Second)) {
		t.Fatal("an unchanged counter reported throttling")
	}

	set("1200")
	if !sensors.throttling(now.Add(10 * time.Second)) {
		t.Fatal("a counter that moved did not report throttling")
	}

	// Still within the window, even though the counter has stopped moving:
	// throttling that stops and starts should read as one continuous state
	// rather than flickering between polls.
	if !sensors.throttling(now.Add(20 * time.Second)) {
		t.Fatal("throttling cleared inside the window")
	}

	// And past it, it clears on its own without anything having to notice.
	if sensors.throttling(now.Add(10*time.Second + throttlingWindow)) {
		t.Fatal("throttling never cleared")
	}
}

// A machine whose kernel exposes no such counter — an AMD part, or a virtual
// machine — must report nothing rather than guessing.
func TestNoCounterIsNotThrottling(t *testing.T) {
	previous := sysfsRoot
	sysfsRoot = t.TempDir()
	t.Cleanup(func() { sysfsRoot = previous })

	sensors := &thermal{}
	if sensors.throttling(time.Now()) {
		t.Fatal("a machine with no throttle counter reported throttling")
	}
}
