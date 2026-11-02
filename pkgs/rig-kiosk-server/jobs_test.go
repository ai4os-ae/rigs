package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJobsOn(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	rig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"queue_depth": 2,
			"capacity": 4,
			"running": ["20261102T115000-aaaaaaaa11111111", "20261102T100000-bbbbbbbb22222222", "20261102T080000-cccccccc33333333"],
			"running_jobs": [
				{"id": "20261102T115000-aaaaaaaa11111111", "kind": "build", "started_at": "2026-11-02T11:50:00Z"},
				{"id": "20261102T100000-bbbbbbbb22222222", "kind": "test", "started_at": "2026-11-02T11:00:00Z"},
				{"id": "20261102T080000-cccccccc33333333", "kind": "fuzz", "started_at": "2026-11-02T08:00:00Z"}
			]
		}`))
	}))
	defer rig.Close()

	got := jobsOn(Rig{Name: "desktop-01", URL: rig.URL}, now)

	if !got.Up {
		t.Fatal("rig reported down")
	}
	if want := "1 building · 1 testing · 1 fuzzing · 2 queued"; got.Summary != want {
		t.Errorf("summary = %q, want %q", got.Summary, want)
	}
	// Four build slots, two taken by the build and the test; the fuzz run has
	// a slot of its own.
	if got.FreeSlots != 2 {
		t.Errorf("free slots = %d, want 2", got.FreeSlots)
	}

	wantIDs := []string{"cccccccc", "bbbbbbbb", "aaaaaaaa"}
	if len(got.Jobs) != len(wantIDs) {
		t.Fatalf("got %d jobs, want %d", len(got.Jobs), len(wantIDs))
	}
	for i, id := range wantIDs {
		if got.Jobs[i].ID != id {
			t.Errorf("job %d = %q, want %q (longest-running first)", i, got.Jobs[i].ID, id)
		}
	}
	if e := got.Jobs[2].Elapsed; e == nil || *e != 600 {
		t.Errorf("elapsed = %v, want 600", e)
	}
}

func TestJobsOnOlderDaemon(t *testing.T) {
	rig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"capacity": 1, "running": ["20261102T115000-aaaaaaaa11111111"]}`))
	}))
	defer rig.Close()

	got := jobsOn(Rig{Name: "laptop-01", URL: rig.URL}, time.Now())

	if len(got.Jobs) != 1 || got.Jobs[0].ID != "aaaaaaaa" || got.Jobs[0].Elapsed != nil {
		t.Errorf("jobs = %+v, want one untimed aaaaaaaa", got.Jobs)
	}
	if got.Summary != "1 running" || got.FreeSlots != 0 {
		t.Errorf("summary %q, free %d", got.Summary, got.FreeSlots)
	}
}

func TestJobsOnUnreachable(t *testing.T) {
	got := jobsOn(Rig{Name: "gone", URL: "http://127.0.0.1:1"}, time.Now())
	if got.Up || got.Summary != "unreachable" {
		t.Errorf("got %+v, want unreachable", got)
	}
}
