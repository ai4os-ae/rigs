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

// Kind tags what a job asks for. A build compiles a kernel; a test boots one
// in a virtual machine and runs something in it. Both are pure functions of
// their inputs and are answered by a derivation, which is why they share this
// envelope, this queue and this state machine.
//
// A fuzz run is not one of those, which is what the tagged envelope was for.
// It is a long-lived process with a corpus that has to survive it, so only its
// *inputs* are a derivation — the kernel, the guest image and the fuzzer, all
// gathered by nix/kernel-fuzz — and the run itself is a child process the
// daemon supervises against a deadline. It shares the envelope, the store and
// every endpoint; it does not share the queue, because a four-hour run must
// not sit in front of somebody's ten-minute build. See fuzzrunner.go.
type Kind string

const (
	KindBuild Kind = "build"
	KindTest  Kind = "test"
	KindFuzz  Kind = "fuzz"
)

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

// KselftestSpec selects the kernel's own tests. Absent means the default set
// for a test with no script of its own, and nothing for a test that has one:
// somebody who wrote a script asked a specific question, and answering a
// different one alongside it doubles the runtime and muddies the verdict.
type KselftestSpec struct {
	// nil is "decide from whether there is a script"; false and true are the
	// caller overriding that.
	Enable *bool `json:"enable,omitempty"`
	// Which selftest collections to build and run. Empty is the rig's default
	// set. A target that does not compile is reported, not fatal.
	Targets []string `json:"targets,omitempty"`
}

// TestSpec is what to do once the kernel has booted.
type TestSpec struct {
	// Python 3, run inside the guest as an ordinary program. Its exit status
	// is the test's: zero passed, anything else did not. Empty runs no script,
	// which is the boot-and-kselftest case.
	Script string `json:"script,omitempty"`

	// Modules to load before anything else runs, so that a script or a
	// selftest can assume the driver under test is present. A module that will
	// not load fails the test.
	Modules []string `json:"modules,omitempty"`

	Kselftest *KselftestSpec `json:"kselftest,omitempty"`

	// How long the whole run may take, including boot. A kernel that hangs is
	// a result and this is how long the rig waits for it.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// The virtual machine's size. Defaults are generous rather than minimal:
	// the guest image is uncompressed and lands in a tmpfs, so a small -m is a
	// confusing out-of-memory during boot rather than a saving.
	MemoryMB int `json:"memory_mb,omitempty"`
	CPUs     int `json:"cpus,omitempty"`

	// Whether a WARNING splat in the kernel log fails the test. Off by
	// default: a clean kernel warns about something on QEMU's hardware often
	// enough that failing on it teaches people to disregard the verdict. An
	// oops, a BUG or a panic always fails, and is not configurable.
	FailOnWarning bool `json:"fail_on_warning,omitempty"`
}

type Spec struct {
	Kind   Kind       `json:"kind"`
	Kernel KernelSpec `json:"kernel"`
	// A unified diff, applied with patch -p1. Empty builds the commit as-is,
	// which is how a caller warms the shared base before submitting patches.
	Patch  string     `json:"patch,omitempty"`
	Config ConfigSpec `json:"config"`
	Arch   string     `json:"arch"`

	// A test job only. The id of a finished job on this rig whose kernel to
	// boot, instead of describing one to build.
	//
	// Both spellings exist because they answer different needs. A spec is
	// self-contained and means the same thing on any rig, and costs nothing to
	// repeat because the store already has the build. `from` is for the
	// obvious thing a person actually wants to say — boot what I just built —
	// and is the only way to test a kernel whose spec you no longer have.
	From string `json:"from,omitempty"`

	Test *TestSpec `json:"test,omitempty"`
	Fuzz *FuzzSpec `json:"fuzz,omitempty"`
}

type Job struct {
	ID    string `json:"id"`
	Spec  Spec   `json:"spec"`
	State State  `json:"state"`

	// Set when State is Failed: why the infrastructure could not answer.
	Error string `json:"error,omitempty"`
	// The realised store path, once there is one. Build and test jobs answer
	// with one; a fuzz run's output is not in the store, so for those this
	// names the environment the run used rather than its results.
	StorePath string `json:"store_path,omitempty"`
	// Where this job's artifacts are, which for a build or a test is also
	// StorePath and for a fuzz run is not — a run writes into its own
	// directory for hours, and nothing written to for hours can be in the
	// store.
	//
	// Set as soon as a fuzz run starts rather than when it ends, which is what
	// lets the artifact endpoints serve a reproducer out of a job that is
	// still going. Persisted, not merely held in memory: a rig reboots for its
	// nightly upgrade, and a finished job whose artifacts became unreachable
	// across that reboot would be a job somebody has to re-run.
	Artifacts string `json:"artifacts_path,omitempty"`
	// Live counters for a fuzz job. Nil for anything else.
	Fuzz *FuzzProgress `json:"fuzz,omitempty"`
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
	// A job id as Store.Create mints them: a timestamp and eight random bytes.
	// Checked rather than merely looked up, because it is used to build a path.
	jobIDRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}-[0-9a-f]{16}$`)
	// Module names, as modprobe accepts them.
	moduleRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	// A selftest collection, which is a directory under
	// tools/testing/selftests and may be nested, as in net/mptcp.
	targetRE = regexp.MustCompile(`^[a-z0-9_][a-z0-9_/-]*$`)
)

// Bounds on a test, so that one job cannot take a rig out of service for a
// day. Generous rather than tight: the point is that a runaway ends, not that
// a caller has to justify wanting four gigabytes.
const (
	defaultTestTimeout = 900
	maxTestTimeout     = 7200
	defaultTestMemory  = 4096
	maxTestMemory      = 32768
	defaultTestCPUs    = 2
	maxTestCPUs        = 64
	// A test script is a test script. Anything larger is data that wants to be
	// fetched, and the guest has no network to fetch it with.
	maxScriptBytes = 1 << 20
)

// validate rejects a spec before it can reach the queue. Everything here is
// something the caller can fix, so it is worth a clear 400 rather than a build
// that fails half an hour later.
func (s *Spec) validate(lim Limits) error {
	if s.Kind == "" {
		s.Kind = KindBuild
	}
	switch s.Kind {
	case KindBuild, KindTest, KindFuzz:
	default:
		return fmt.Errorf("unsupported kind %q; this rig implements %q, %q and %q",
			s.Kind, KindBuild, KindTest, KindFuzz)
	}

	if s.Kind == KindBuild {
		if s.From != "" {
			return fmt.Errorf("from is for test and fuzz jobs; a build job describes the kernel it builds")
		}
		if s.Test != nil {
			return fmt.Errorf("test is for test jobs; set kind to %q", KindTest)
		}
	}
	if s.Kind != KindTest && s.Test != nil {
		return fmt.Errorf("test is for test jobs; set kind to %q", KindTest)
	}
	if s.Kind != KindFuzz && s.Fuzz != nil {
		return fmt.Errorf("fuzz is for fuzz jobs; set kind to %q", KindFuzz)
	}

	// A test either names the kernel it wants or describes one, and doing both
	// is a caller who believes one of them is being honoured when it is not.
	if s.From != "" {
		if !jobIDRE.MatchString(s.From) {
			return fmt.Errorf("from must be the id of a job on this rig, got %q", s.From)
		}
		if s.Kernel.Commit != "" || s.Patch != "" || s.Config.Base != "" || len(s.Config.Set) > 0 {
			return fmt.Errorf("a job with `from` boots that job's kernel; do not also describe one to build")
		}
	} else {
		if !commitRE.MatchString(s.Kernel.Commit) {
			return fmt.Errorf("kernel.commit must be a full 40-character hex commit, got %q", s.Kernel.Commit)
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
	}

	if s.Arch == "" {
		s.Arch = "x86_64"
	}
	if s.Arch != "x86_64" {
		return fmt.Errorf("only x86_64 is implemented, got %q", s.Arch)
	}

	if s.Kind == KindTest {
		if s.Test == nil {
			// A test with no instructions is the useful default rather than an
			// error: boot the kernel and run the kernel's own tests on it.
			s.Test = &TestSpec{}
		}
		if err := s.Test.validate(); err != nil {
			return err
		}
	}

	if s.Kind == KindFuzz {
		if s.Fuzz == nil {
			// A fuzz job with no instructions is a general, undirected run,
			// which is a perfectly good thing to ask for and is what syzkaller
			// does best. The defaults are in FuzzSpec.validate.
			s.Fuzz = &FuzzSpec{}
		}
		if err := s.Fuzz.validate(lim); err != nil {
			return err
		}
	}

	return nil
}

func (t *TestSpec) validate() error {
	if len(t.Script) > maxScriptBytes {
		return fmt.Errorf("test.script is %d bytes; the limit is %d", len(t.Script), maxScriptBytes)
	}

	for _, module := range t.Modules {
		if !moduleRE.MatchString(module) {
			return fmt.Errorf("test.modules[%q] is not a module name", module)
		}
	}

	if t.Kselftest != nil {
		for _, target := range t.Kselftest.Targets {
			if !targetRE.MatchString(target) {
				return fmt.Errorf("test.kselftest.targets[%q] is not a selftest collection", target)
			}
		}
	}

	if t.TimeoutSeconds == 0 {
		t.TimeoutSeconds = defaultTestTimeout
	}
	if t.TimeoutSeconds < 1 || t.TimeoutSeconds > maxTestTimeout {
		return fmt.Errorf("test.timeout_seconds must be between 1 and %d, got %d", maxTestTimeout, t.TimeoutSeconds)
	}

	if t.MemoryMB == 0 {
		t.MemoryMB = defaultTestMemory
	}
	// Not merely a lower bound for tidiness: the guest image is an
	// uncompressed initramfs unpacked into a tmpfs, so a machine too small to
	// hold it fails during boot with an out-of-memory that looks like a kernel
	// problem rather than a job that asked for too little.
	if t.MemoryMB < 512 || t.MemoryMB > maxTestMemory {
		return fmt.Errorf("test.memory_mb must be between 512 and %d, got %d", maxTestMemory, t.MemoryMB)
	}

	if t.CPUs == 0 {
		t.CPUs = defaultTestCPUs
	}
	if t.CPUs < 1 || t.CPUs > maxTestCPUs {
		return fmt.Errorf("test.cpus must be between 1 and %d, got %d", maxTestCPUs, t.CPUs)
	}

	return nil
}

// KselftestEnabled decides whether to run the kernel's own tests.
//
// The default depends on whether the caller wrote a script, which is the one
// piece of policy in this file: a test with no script of its own is asking
// "does this kernel work", and kselftest is the best available answer. A test
// with a script asked something specific, and running several minutes of
// unrelated tests alongside it would slow the answer down and blur what a
// failure means.
func (t *TestSpec) KselftestEnabled() bool {
	if t.Kselftest != nil && t.Kselftest.Enable != nil {
		return *t.Kselftest.Enable
	}
	return t.Script == ""
}

// KselftestTargets is the collections to build, or nil for the rig's default
// set, which nix/kernel-test chooses rather than this.
func (t *TestSpec) KselftestTargets() []string {
	if t.Kselftest == nil {
		return nil
	}
	return t.Kselftest.Targets
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
