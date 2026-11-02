package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rig is a build daemon whose jobs go on the board: this rig's own, on
// loopback, and on the rig that runs the fleet broker, every other member's
// over the mesh.
type Rig struct {
	Name string
	URL  string
}

// RigJobs is one rig's panel on the board.
type RigJobs struct {
	Name string `json:"name"`
	Up   bool   `json:"up"`

	// One line for the panel's header — "2 building · 1 queued", "idle",
	// "unreachable" — so the page does not have to know the counting rules.
	Summary string `json:"summary"`

	// Build slots with nothing in them, drawn as empty outlines so that a
	// glance says how much room the rig has as well as what it is doing.
	FreeSlots int `json:"free_slots"`

	Jobs []Job `json:"jobs"`
}

// Job is one running job.
type Job struct {
	// Enough of the id to pick the job out of a listing, not all of it.
	ID   string `json:"id"`
	Kind string `json:"kind"`

	// Seconds since the job started, as of this response. The page counts on
	// from here with its own clock, which is the server's clock too, so a
	// timer ticking every second costs no extra requests. Null when the rig
	// did not say when the job started.
	Elapsed *float64 `json:"elapsed_seconds"`
}

// The page polls every few seconds, so a build daemon that has stopped
// answering must not hold the whole status response open. Loopback answers in
// well under this; a peer over the mesh from campus wireless sometimes does
// not, and is shown as unreachable for one poll rather than stalling the page.
var builddClient = &http.Client{Timeout: 1500 * time.Millisecond}

// boardOf asks every rig at once, so one rig that has gone away costs the page
// one timeout rather than one per rig.
func boardOf(rigs []Rig) []RigJobs {
	board := make([]RigJobs, len(rigs))

	var wg sync.WaitGroup
	for i, rig := range rigs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			board[i] = jobsOn(rig, time.Now())
		}()
	}
	wg.Wait()

	return board
}

func jobsOn(rig Rig, now time.Time) RigJobs {
	out := RigJobs{Name: rig.Name, Summary: "unreachable", Jobs: []Job{}}

	resp, err := builddClient.Get(rig.URL + "/v1/status")
	if err != nil {
		return out
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out
	}

	var status struct {
		QueueDepth     int      `json:"queue_depth"`
		Running        []string `json:"running"`
		Capacity       int      `json:"capacity"`
		FuzzQueueDepth int      `json:"fuzz_queue_depth"`
		RunningJobs    []struct {
			ID        string     `json:"id"`
			Kind      string     `json:"kind"`
			StartedAt *time.Time `json:"started_at"`
		} `json:"running_jobs"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&status); err != nil {
		return out
	}

	out.Up = true

	// A daemon from before running_jobs existed lists ids and nothing else.
	// Both rigs upgrade from the same flake overnight, so this is the gap of
	// a few hours between them, and an id with no timer beats no tile.
	if status.RunningJobs == nil {
		for _, id := range status.Running {
			out.Jobs = append(out.Jobs, Job{ID: shortID(id)})
		}
	}

	for _, running := range status.RunningJobs {
		job := Job{ID: shortID(running.ID), Kind: running.Kind}
		if running.StartedAt != nil {
			elapsed := max(now.Sub(*running.StartedAt).Seconds(), 0)
			job.Elapsed = &elapsed
		}
		out.Jobs = append(out.Jobs, job)
	}

	// Longest-running first: the job somebody is most likely to be waiting on
	// is the one that has been going longest.
	sort.SliceStable(out.Jobs, func(i, j int) bool {
		a, b := out.Jobs[i].Elapsed, out.Jobs[j].Elapsed
		if (a == nil) != (b == nil) {
			return a != nil
		}
		return a != nil && *a > *b
	})

	counts := map[string]int{}
	for _, job := range out.Jobs {
		counts[job.Kind]++
	}

	// Fuzz runs hold slots of their own, so only builds and tests take up the
	// outlines drawn for build capacity.
	out.FreeSlots = max(status.Capacity-(len(out.Jobs)-counts["fuzz"]), 0)

	var parts []string
	for _, kind := range []struct{ kind, verb string }{
		{"build", "building"},
		{"test", "testing"},
		{"fuzz", "fuzzing"},
		{"", "running"},
	} {
		if n := counts[kind.kind]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, kind.verb))
		}
	}
	if queued := status.QueueDepth + status.FuzzQueueDepth; queued > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", queued))
	}
	out.Summary = "idle"
	if len(parts) > 0 {
		out.Summary = strings.Join(parts, " · ")
	}

	return out
}

// shortID keeps the random half of a job id. Ids are a timestamp then random
// hex — "20261102T153012-a1b2c3d4e5f6a7b8" — and the timestamp is the part
// every job submitted this afternoon shares, while the board already says how
// long each has been running. Eight hex characters are what tells them apart.
func shortID(id string) string {
	if _, random, ok := strings.Cut(id, "-"); ok {
		id = random
	}
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}
