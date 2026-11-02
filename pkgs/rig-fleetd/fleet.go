package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Member is one rig, as declared in the NixOS configuration. The fleet is a
// static list rather than something rigs register into: a rig is already
// declared in this repository, with a fixed mesh address, so the configuration
// is the truth and there is no enrollment flow to get wrong.
type Member struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// MemberStatus is a member plus the last thing it said about itself. Polled in
// the background rather than on demand, so placing a job does not wait on a
// round trip to every rig — least of all a rig that has gone away.
type MemberStatus struct {
	Member

	Up    bool   `json:"up"`
	Error string `json:"error,omitempty"`

	System         string    `json:"system,omitempty"`
	QueueDepth     int       `json:"queue_depth"`
	Running        int       `json:"running"`
	Capacity       int       `json:"capacity"`
	Load           []float64 `json:"load,omitempty"`
	StoreFreeBytes uint64    `json:"store_free_bytes,omitempty"`
	Kinds          []string  `json:"kinds,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`

	// Fuzzing is counted separately because queue depth does not describe it.
	// A fuzz run holds its slot for hours by design, so a rig with an empty
	// queue and its only slot four hours into a campaign is the *least*
	// available rig in the fleet while looking like the most.
	FuzzSlotsFree int `json:"fuzz_slots_free"`
	FuzzCapacity  int `json:"fuzz_capacity"`
}

// pending is how loaded a rig is for placement purposes: work already accepted
// but not finished. Load average is not used — it lags by a minute, which is
// most of a patched build.
func (m *MemberStatus) pending() int { return m.QueueDepth + m.Running }

// capacity is how many jobs of a kind the rig runs at once. A rig that has not
// said is taken to run one, which is what every rig did before it could say.
func (m *MemberStatus) capacity(kind string) int {
	c := m.Capacity
	if kind == "fuzz" && m.FuzzCapacity > 0 {
		c = m.FuzzCapacity
	}
	return max(c, 1)
}

// hasRoom reports whether a job placed here would start now rather than wait
// in this rig's queue.
func (m *MemberStatus) hasRoom(kind string) bool {
	if kind == "fuzz" {
		return m.canFuzzNow()
	}
	return m.pending() < m.capacity(kind)
}

// canFuzzNow reports whether a fuzz job placed here would start rather than
// wait out somebody else's campaign.
//
// A rig that has not said — an older one — is taken at its word that it runs
// the kind, on the same principle as `runs`: assume the fleet still works.
func (m *MemberStatus) canFuzzNow() bool {
	if m.FuzzCapacity == 0 {
		return true
	}
	return m.FuzzSlotsFree > 0
}

// runs reports whether this rig implements a kind of job.
//
// A rig that has not said — an older one, or one whose status did not parse —
// is taken to run builds and nothing else, which is what every rig has always
// done. Assuming the opposite would empty the fleet the first time a status
// field changed shape.
func (m *MemberStatus) runs(kind string) bool {
	if kind == "" {
		return true
	}
	if len(m.Kinds) == 0 {
		return kind == "build"
	}
	return slices.Contains(m.Kinds, kind)
}

type Fleet struct {
	members []Member
	client  *http.Client

	mu     sync.RWMutex
	status map[string]*MemberStatus

	affinity *Affinity
}

func NewFleet(members []Member, affinity *Affinity) *Fleet {
	f := &Fleet{
		members:  members,
		affinity: affinity,
		// Short: this is a status poll on the mesh, and a rig that cannot
		// answer it in two seconds should be treated as busy or gone rather
		// than allowed to stall placement.
		client: &http.Client{Timeout: 2 * time.Second},
		status: map[string]*MemberStatus{},
	}
	for _, m := range members {
		f.status[m.Name] = &MemberStatus{Member: m}
	}
	return f
}

// Poll refreshes every member forever. Members are polled concurrently: one
// unreachable rig must not delay the view of the others.
func (f *Fleet) Poll(ctx context.Context, every time.Duration) {
	f.refresh(ctx)

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.refresh(ctx)
		}
	}
}

func (f *Fleet) refresh(ctx context.Context) {
	var wg sync.WaitGroup
	for _, m := range f.members {
		wg.Add(1)
		go func(m Member) {
			defer wg.Done()
			f.check(ctx, m)
		}(m)
	}
	wg.Wait()
}

func (f *Fleet) check(ctx context.Context, m Member) {
	next := &MemberStatus{Member: m, CheckedAt: time.Now().UTC()}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL+"/v1/status", nil)
	if err == nil {
		var resp *http.Response
		if resp, err = f.client.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("status %s", resp.Status)
			} else {
				var s struct {
					System         string    `json:"system"`
					QueueDepth     int       `json:"queue_depth"`
					Running        []string  `json:"running"`
					Capacity       int       `json:"capacity"`
					Load           []float64 `json:"load"`
					StoreFreeBytes uint64    `json:"store_free_bytes"`
					Kinds          []string  `json:"kinds"`
					FuzzSlotsFree  int       `json:"fuzz_slots_free"`
					FuzzCapacity   int       `json:"fuzz_capacity"`
				}
				if err = json.NewDecoder(resp.Body).Decode(&s); err == nil {
					next.Up = true
					next.System = s.System
					next.QueueDepth = s.QueueDepth
					next.Running = len(s.Running)
					next.Capacity = s.Capacity
					next.Load = s.Load
					next.StoreFreeBytes = s.StoreFreeBytes
					next.Kinds = s.Kinds
					next.FuzzSlotsFree = s.FuzzSlotsFree
					next.FuzzCapacity = s.FuzzCapacity
				}
			}
		}
	}
	if err != nil {
		next.Error = err.Error()
	}

	f.mu.Lock()
	f.status[m.Name] = next
	f.mu.Unlock()
}

func (f *Fleet) Status() []*MemberStatus {
	f.mu.RLock()
	defer f.mu.RUnlock()

	out := make([]*MemberStatus, 0, len(f.status))
	for _, s := range f.status {
		copied := *s
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *Fleet) Lookup(name string) (Member, bool) {
	for _, m := range f.members {
		if m.Name == name {
			return m, true
		}
	}
	return Member{}, false
}

// Pick chooses the rig to run a job on.
//
//  1. An explicit pin wins, and fails loudly if that rig is down — somebody who
//     named a rig wants that rig, not a silent substitution.
//  2. Otherwise, among live rigs that run this kind and this system: the one
//     that most recently built this same (commit, configuration). That rig
//     already has the expensive unpatched base in its store, which is the
//     difference between a twenty-second job and a six-minute one, and is by
//     far the largest lever available here.
//  3. Otherwise a rig with a free slot, the largest first; and when every rig
//     is full, the largest takes the queue. Rigs are not equals — one with
//     eight slots drains a queue far faster than one with one — so a small rig
//     takes overflow while it is idle and never accumulates a backlog of its
//     own while a bigger one is up. Fewer pending jobs, then free disk, break
//     ties between rigs of one size.
func (f *Fleet) Pick(system, pin, kind, affinityKey string) (Member, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if pin != "" {
		s, ok := f.status[pin]
		if !ok {
			return Member{}, fmt.Errorf("no rig named %q in this fleet", pin)
		}
		if !s.Up {
			return Member{}, fmt.Errorf("rig %q is not reachable: %s", pin, s.Error)
		}
		if system != "" && s.System != "" && s.System != system {
			return Member{}, fmt.Errorf("rig %q is %s, not %s", pin, s.System, system)
		}
		if !s.runs(kind) {
			return Member{}, fmt.Errorf("rig %q does not run %s jobs", pin, kind)
		}
		return s.Member, nil
	}

	var candidates []*MemberStatus
	for _, s := range f.status {
		if !s.Up {
			continue
		}
		if system != "" && s.System != "" && s.System != system {
			continue
		}
		if !s.runs(kind) {
			continue
		}
		// A rig mid-campaign would accept the job and then sit on it for
		// hours. Queue depth says nothing about that, so it is asked directly.
		if kind == "fuzz" && !s.canFuzzNow() {
			continue
		}
		candidates = append(candidates, s)
	}
	if len(candidates) == 0 {
		if kind == "fuzz" {
			return Member{}, fmt.Errorf(
				"no reachable rig on %s has a free fuzz slot; a fuzzing run holds one for hours, "+
					"so this usually means waiting rather than a fleet problem", system)
		}
		return Member{}, fmt.Errorf("no reachable rig runs %s jobs on %s", kind, system)
	}

	if preferred := f.affinity.Get(affinityKey); preferred != "" {
		for _, s := range candidates {
			// Only when it would start there: a warm store is worth a lot, but
			// not worth queueing behind other builds, least of all on a rig
			// that runs one at a time.
			if s.Name == preferred && s.hasRoom(kind) {
				return s.Member, nil
			}
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.hasRoom(kind) != b.hasRoom(kind) {
			return a.hasRoom(kind)
		}
		if a.capacity(kind) != b.capacity(kind) {
			return a.capacity(kind) > b.capacity(kind)
		}
		if a.pending() != b.pending() {
			return a.pending() < b.pending()
		}
		if a.StoreFreeBytes != b.StoreFreeBytes {
			return a.StoreFreeBytes > b.StoreFreeBytes
		}
		return a.Name < b.Name
	})
	return candidates[0].Member, nil
}

// Placed counts a job a rig has just accepted against it, until the next poll
// reports the rig's own figure. Without it every job in a burst is placed
// against the same stale status and lands on the same rig, which for a run of
// submissions inside one poll interval is all of them.
func (f *Fleet) Placed(name, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	s, ok := f.status[name]
	if !ok {
		return
	}
	next := *s
	if kind == "fuzz" && next.FuzzCapacity > 0 {
		next.FuzzSlotsFree--
	} else {
		next.QueueDepth++
	}
	f.status[name] = &next
}

// Affinity remembers which rig last built a given (commit, configuration), so
// that the next patch against it lands where the base already is.
//
// Deliberately a hint and not a record: it is consulted, never required, and
// losing it costs one slow build. It is still persisted, because the file is
// tiny and a broker restart otherwise scatters a run of related jobs across
// the fleet at exactly the moment that is most expensive.
type Affinity struct {
	path string

	mu sync.Mutex
	to map[string]string
}

func LoadAffinity(path string) *Affinity {
	a := &Affinity{path: path, to: map[string]string{}}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &a.to); err != nil {
			log.Printf("affinity file %s is unreadable, starting empty: %v", path, err)
			a.to = map[string]string{}
		}
	}
	return a
}

func (a *Affinity) Get(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.to[key]
}

func (a *Affinity) Set(key, rig string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.to[key] == rig {
		return
	}
	a.to[key] = rig

	raw, err := json.MarshalIndent(a.to, "", "  ")
	if err != nil {
		return
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("writing affinity: %v", err)
		return
	}
	if err := os.Rename(tmp, a.path); err != nil {
		log.Printf("writing affinity: %v", err)
	}
}

// affinityKey identifies the shared base a job will build on: the commit and
// the configuration, and deliberately not the patch. Two different patches
// against one commit share a base and should land on the same rig, which is
// the whole point.
func affinityKey(commit, configBase, arch string, set map[string]string) string {
	symbols := make([]string, 0, len(set))
	for symbol, value := range set {
		symbols = append(symbols, symbol+"="+value)
	}
	sort.Strings(symbols)

	sum := sha256.Sum256([]byte(strings.Join([]string{
		commit, configBase, arch, strings.Join(symbols, ","),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func LoadMembers(path string) ([]Member, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var members []Member
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("%s declares no rigs", filepath.Base(path))
	}
	return members, nil
}
