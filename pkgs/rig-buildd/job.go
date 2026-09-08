package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Kind tags what a job asks for. Only builds are implemented; the field exists
// because boot tests and fuzz runs are coming and they are not the same shape
// of work. A build and a boot test are pure functions of their inputs and are
// answered by a derivation; a fuzz run is a long-lived process with a corpus
// that has to survive it. Tagging the envelope now means adding the second
// backend later does not change this API.
type Kind string

const KindBuild Kind = "build"

// State is what happened to the job, which is not the same question as whether
// the kernel compiled.
//
// A job whose kernel failed to compile is Succeeded: it ran, and it produced
// exactly the answer the caller asked for, with logs and a report saying
// passed=false. Failed means the daemon or Nix itself broke and the caller
// learned nothing about their patch.
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

func (s State) terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

type KernelSpec struct {
	Commit string `json:"commit"`
	// Where to fetch the commit from if the mirror does not have it. Empty
	// means the rig's configured default, which is the normal case.
	Remote string `json:"remote,omitempty"`
}

type ConfigSpec struct {
	// A kernel make target: defconfig, tinyconfig, allmodconfig.
	Base string `json:"base"`
	// Symbol overrides applied on top, each "y", "m" or "n". Deliberately not
	// free-form text: this is data, not a hook for arbitrary make commands.
	Set map[string]string `json:"set,omitempty"`
}

type Spec struct {
	Kind   Kind       `json:"kind"`
	Kernel KernelSpec `json:"kernel"`
	// A unified diff, applied with patch -p1. Empty builds the commit as-is,
	// which is how a caller warms the shared base before submitting patches.
	Patch  string     `json:"patch,omitempty"`
	Config ConfigSpec `json:"config"`
	Arch   string     `json:"arch"`
}

type Job struct {
	ID    string `json:"id"`
	Spec  Spec   `json:"spec"`
	State State  `json:"state"`

	// Set when State is Failed: why the infrastructure could not answer.
	Error string `json:"error,omitempty"`
	// The realised store path, once there is one.
	StorePath string `json:"store_path,omitempty"`
	// Lifted out of the build's own report.json so that listing jobs does not
	// mean reading a file per job out of the store.
	Passed        *bool  `json:"passed,omitempty"`
	KernelRelease string `json:"kernel_release,omitempty"`

	SubmittedAt time.Time  `json:"submitted_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

var (
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	symbolRE = regexp.MustCompile(`^(CONFIG_)?[A-Z0-9_]+$`)
	// Kernel make targets are word-shaped. Checked because this string is
	// passed to make, and a caller should not be able to smuggle a second
	// target or a shell fragment through it.
	configBaseRE = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// validate rejects a spec before it can reach the queue. Everything here is
// something the caller can fix, so it is worth a clear 400 rather than a build
// that fails half an hour later.
func (s *Spec) validate() error {
	if s.Kind == "" {
		s.Kind = KindBuild
	}
	if s.Kind != KindBuild {
		return fmt.Errorf("unsupported kind %q; this rig implements %q", s.Kind, KindBuild)
	}

	if !commitRE.MatchString(s.Kernel.Commit) {
		return fmt.Errorf("kernel.commit must be a full 40-character hex commit, got %q", s.Kernel.Commit)
	}

	if s.Arch == "" {
		s.Arch = "x86_64"
	}
	if s.Arch != "x86_64" {
		return fmt.Errorf("only x86_64 is implemented, got %q", s.Arch)
	}

	if s.Config.Base == "" {
		s.Config.Base = "defconfig"
	}
	if !configBaseRE.MatchString(s.Config.Base) {
		return fmt.Errorf("config.base must be a kernel make target such as defconfig, got %q", s.Config.Base)
	}

	for symbol, value := range s.Config.Set {
		if !symbolRE.MatchString(symbol) {
			return fmt.Errorf("config symbol %q is not a Kconfig symbol name", symbol)
		}
		if value != "y" && value != "m" && value != "n" {
			return fmt.Errorf("config.set[%s] must be y, m or n, got %q", symbol, value)
		}
	}

	return nil
}

// Store keeps jobs on disk, one directory each, and an index of them in
// memory. There is no database: the queue is serial and a rig holds a few
// hundred jobs at most, so a directory per job is both the storage and the
// thing a human reads when they want to know what happened.
type Store struct {
	root string

	mu   sync.RWMutex
	jobs map[string]*Job
}

func OpenStore(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "jobs"), 0o755); err != nil {
		return nil, err
	}

	s := &Store{root: root, jobs: map[string]*Job{}}

	entries, err := os.ReadDir(filepath.Join(root, "jobs"))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		job, err := s.read(entry.Name())
		if err != nil {
			// One unreadable job must not stop the daemon from starting: the
			// rest of the queue is still serviceable.
			continue
		}
		s.jobs[job.ID] = job
	}

	return s, nil
}

func (s *Store) dir(id string) string { return filepath.Join(s.root, "jobs", id) }

// LogPath and ResultPath are also the GC root: `nix build --out-link` pointing
// at ResultPath is what stops a completed job's artifacts being collected
// before the caller has fetched them.
func (s *Store) LogPath(id string) string    { return filepath.Join(s.dir(id), "log") }
func (s *Store) ResultPath(id string) string { return filepath.Join(s.dir(id), "result") }
func (s *Store) SpecPath(id string) string   { return filepath.Join(s.dir(id), "job.nix") }

func (s *Store) read(id string) (*Job, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir(id), "job.json"))
	if err != nil {
		return nil, err
	}
	var job Job
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Store) save(job *Job) error {
	raw, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename: a job.json truncated by a power cut is a job the
	// daemon cannot read at startup, and these machines do lose power.
	tmp := filepath.Join(s.dir(job.ID), "job.json.tmp")
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir(job.ID), "job.json"))
}

func (s *Store) Create(spec Spec) (*Job, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	// Time-ordered prefix so that `ls` in the state directory reads
	// chronologically, which is how somebody debugging a rig will look at it.
	id := time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(buf[:])

	job := &Job{
		ID:          id,
		Spec:        spec,
		State:       StateQueued,
		SubmittedAt: time.Now().UTC(),
	}

	if err := os.MkdirAll(s.dir(id), 0o755); err != nil {
		return nil, err
	}
	if err := s.save(job); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.jobs[id] = job
	s.mu.Unlock()

	return job, nil
}

func (s *Store) Get(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	copied := *job
	return &copied, true
}

// List returns jobs newest first, which is the order anybody asking wants.
func (s *Store) List() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		copied := *job
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// Update applies a change under the lock and persists it.
func (s *Store) Update(id string, fn func(*Job)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("no such job %q", id)
	}
	fn(job)
	return s.save(job)
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.jobs[id]; !ok {
		return fmt.Errorf("no such job %q", id)
	}
	delete(s.jobs, id)
	// Removing the directory drops the result symlink with it, which is what
	// releases the GC root and lets the store reclaim the artifacts.
	return os.RemoveAll(s.dir(id))
}

// Requeue puts jobs that were running when the daemon stopped back on the
// queue. Nix will have cached whatever finished, so this is usually cheap, and
// the alternative is a job that is never resumed and never reported.
func (s *Store) Requeue() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var ids []string
	for id, job := range s.jobs {
		if job.State == StateRunning {
			job.State = StateQueued
			job.StartedAt = nil
			_ = s.save(job)
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Pending lists queued jobs oldest first, for restoring the queue at startup.
func (s *Store) Pending() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var ids []string
	for id, job := range s.jobs {
		if job.State == StateQueued {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
