package main

import (
	"path/filepath"
	"testing"
)

// The fleet as declared: one rig with eight slots and one with a single slot.
func testFleet(t *testing.T) *Fleet {
	t.Helper()
	members := []Member{
		{Name: "desktop-01", URL: "http://desktop"},
		{Name: "laptop-01", URL: "http://laptop"},
	}
	f := NewFleet(members, LoadAffinity(filepath.Join(t.TempDir(), "affinity.json")))
	f.status["desktop-01"] = &MemberStatus{Member: members[0], Up: true, System: "x86_64-linux", Capacity: 8, StoreFreeBytes: 100}
	// More free disk on the small rig, which used to be enough to send it the
	// whole of a burst.
	f.status["laptop-01"] = &MemberStatus{Member: members[1], Up: true, System: "x86_64-linux", Capacity: 1, StoreFreeBytes: 900}
	return f
}

func TestPickFillsLargeRigAndNeverQueuesOnSmallOne(t *testing.T) {
	f := testFleet(t)

	placed := map[string]int{}
	for range 20 {
		m, err := f.Pick("x86_64-linux", "", "build", "")
		if err != nil {
			t.Fatal(err)
		}
		f.Placed(m.Name, "build")
		placed[m.Name]++
	}

	if placed["laptop-01"] != 1 {
		t.Errorf("laptop-01 got %d jobs from a burst of 20, want 1 (its single slot, no queue)", placed["laptop-01"])
	}
	if placed["desktop-01"] != 19 {
		t.Errorf("desktop-01 got %d jobs from a burst of 20, want 19", placed["desktop-01"])
	}
}

func TestPickUsesSmallRigOnlyOnceLargeOneIsFull(t *testing.T) {
	f := testFleet(t)

	for i := range 8 {
		m, err := f.Pick("x86_64-linux", "", "build", "")
		if err != nil {
			t.Fatal(err)
		}
		if m.Name != "desktop-01" {
			t.Fatalf("job %d went to %s while desktop-01 had a free slot", i+1, m.Name)
		}
		f.Placed(m.Name, "build")
	}

	m, _ := f.Pick("x86_64-linux", "", "build", "")
	if m.Name != "laptop-01" {
		t.Errorf("ninth job went to %s, want laptop-01's idle slot", m.Name)
	}
}

func TestAffinityDoesNotQueueOnFullRig(t *testing.T) {
	f := testFleet(t)
	f.affinity.Set("base", "laptop-01")

	m, _ := f.Pick("x86_64-linux", "", "build", "base")
	if m.Name != "laptop-01" {
		t.Fatalf("idle laptop-01 with a warm store lost to %s", m.Name)
	}
	f.Placed(m.Name, "build")

	m, _ = f.Pick("x86_64-linux", "", "build", "base")
	if m.Name != "desktop-01" {
		t.Errorf("second job on the same base went to %s, want desktop-01 rather than a queue on laptop-01", m.Name)
	}
}

func TestSmallRigTakesQueueWhenAlone(t *testing.T) {
	f := testFleet(t)
	f.status["desktop-01"].Up = false
	f.status["laptop-01"].Running = 1

	m, err := f.Pick("x86_64-linux", "", "build", "")
	if err != nil || m.Name != "laptop-01" {
		t.Errorf("with desktop-01 down got %v, %v; want laptop-01", m.Name, err)
	}
}
