package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunnerConfig is everything the NixOS module pins about how work runs here.
//
// Store paths rather than NIX_PATH lookups: what a rig builds against is a
// property of its configuration and not of whatever channel happens to be
// registered on the machine.
type RunnerConfig struct {
	Nixpkgs     string
	KernelBuild string
	KernelTest  string
	// Both empty when this rig does not run fuzz jobs. SyzkallerPkg is the
	// package *expression*, like the others here, rather than a built
	// syzkaller: it lives under pkgs/ and so cannot be reached by a relative
	// path from inside the nix/ directory the module copies in one piece.
	KernelFuzz   string
	SyzkallerPkg string

	// Where the daemon keeps state that outlives a job: the fuzzing SSH key
	// and the shared corpora.
	StateDir string

	Capacity     int
	FuzzCapacity int

	Limits Limits
}

// Runner is the queue. One goroutine per concurrency slot takes job ids off a
// channel and runs them; a rig has twelve threads and a kernel build wants all
// of them, so the default slot count is one and raising it is usually wrong.
//
// Nix is the build backend but not the interface: everything a caller sees is
// the HTTP API, and swapping what happens in `build` below would not change it.
// What Nix buys here is that identical work is never repeated — two jobs with
// the same commit, patch and configuration resolve to the same derivation, and
// the second is answered from the store — and that a patched build reuses the
// expensive unpatched base it shares with every other patch against that
// commit.
//
// A fuzz job takes the same path as far as its *inputs*, and then diverges: it
// has no derivation to wait on, only a process to supervise. See fuzzrunner.go.
type Runner struct {
	store  *Store
	mirror *Mirror
	cfg    RunnerConfig

	queue chan string
	// Fuzz jobs get their own queue, and it is not a tuning decision. A fuzz
	// run holds a slot for hours by design; sharing the build queue would mean
	// a ten-minute kernel build waiting behind somebody's overnight campaign.
	fuzzQueue chan string

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	running map[string]bool
}

func NewRunner(store *Store, mirror *Mirror, cfg RunnerConfig) *Runner {
	return &Runner{
		store:  store,
		mirror: mirror,
		cfg:    cfg,
		// Buffered generously: submitting must not block on a busy queue, and
		// a rig will not hold thousands of pending kernel builds.
		queue:     make(chan string, 1024),
		fuzzQueue: make(chan string, 1024),
		cancels:   map[string]context.CancelFunc{},
		running:   map[string]bool{},
	}
}

func (r *Runner) Start() {
	for range r.cfg.Capacity {
		go func() {
			for id := range r.queue {
				r.build(id)
			}
		}()
	}
	for range r.cfg.FuzzCapacity {
		go func() {
			for id := range r.fuzzQueue {
				r.build(id)
			}
		}()
	}
}

// Submit puts a job on the queue its kind belongs to.
func (r *Runner) Submit(id string) {
	if job, ok := r.store.Get(id); ok && job.Spec.Kind == KindFuzz {
		r.fuzzQueue <- id
		return
	}
	r.queue <- id
}

func (r *Runner) QueueDepth() int { return len(r.queue) }

func (r *Runner) FuzzQueueDepth() int { return len(r.fuzzQueue) }

// FuzzSlotsFree is what a broker reads to decide whether this rig can take a
// fuzz job now. Queue depth is the wrong question for a job that runs for
// hours: a rig with an empty queue and its only slot four hours into a
// campaign is not available.
func (r *Runner) FuzzSlotsFree() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	busy := 0
	for id := range r.running {
		if job, ok := r.store.Get(id); ok && job.Spec.Kind == KindFuzz {
			busy++
		}
	}
	return max(r.cfg.FuzzCapacity-busy, 0)
}

func (r *Runner) Running() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.running))
	for id := range r.running {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Cancel stops a running job. Returns false if it was not running, which the
// handler treats as "nothing to cancel" rather than an error.
func (r *Runner) Cancel(id string) bool {
	r.mu.Lock()
	cancel, ok := r.cancels[id]
	r.mu.Unlock()

	if !ok {
		return false
	}
	cancel()
	return true
}

func (r *Runner) build(id string) {
	job, ok := r.store.Get(id)
	if !ok {
		return
	}
	// Cancelled or deleted while it sat in the queue.
	if job.State != StateQueued {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancels[id] = cancel
	r.running[id] = true
	r.mu.Unlock()

	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.cancels, id)
		delete(r.running, id)
		r.mu.Unlock()
	}()

	now := time.Now().UTC()
	_ = r.store.Update(id, func(j *Job) {
		j.State = StateRunning
		j.StartedAt = &now
	})

	logFile, err := os.Create(r.store.LogPath(id))
	if err != nil {
		r.finishFailed(id, fmt.Sprintf("opening the job log: %v", err))
		return
	}
	defer logFile.Close()

	// Both paths answer with the directory holding report.json and the
	// artifacts. For a build or a test that is a store path; for a fuzz run it
	// is the syzkaller workdir, which is not in the store and could not be —
	// it was written to for hours.
	var (
		resultDir string
		isStore   bool
	)
	if job.Spec.Kind == KindFuzz {
		resultDir, err = r.runFuzz(ctx, job, logFile)
	} else {
		resultDir, err = r.run(ctx, job, logFile)
		isStore = true
	}

	finished := time.Now().UTC()
	switch {
	case ctx.Err() != nil:
		fmt.Fprintf(logFile, "\n--- cancelled\n")
		_ = r.store.Update(id, func(j *Job) {
			j.State = StateCancelled
			j.FinishedAt = &finished
		})

	case err != nil:
		fmt.Fprintf(logFile, "\n--- job failed: %v\n", err)
		_ = r.store.Update(id, func(j *Job) {
			j.State = StateFailed
			j.Error = err.Error()
			j.FinishedAt = &finished
		})

	default:
		// The job ran. Whether the *kernel* compiled, booted or survived being
		// fuzzed is a separate question, answered by report.json, and a failure
		// there is a result rather than an error — see the comment on State.
		passed, release := r.summarise(resultDir)
		_ = r.store.Update(id, func(j *Job) {
			j.State = StateSucceeded
			if isStore {
				j.StorePath = resultDir
			}
			j.Artifacts = resultDir
			j.Passed = passed
			if release != "" {
				j.KernelRelease = release
			}
			j.FinishedAt = &finished
		})
	}
}

func (r *Runner) finishFailed(id, msg string) {
	finished := time.Now().UTC()
	_ = r.store.Update(id, func(j *Job) {
		j.State = StateFailed
		j.Error = msg
		j.FinishedAt = &finished
	})
}

func (r *Runner) run(ctx context.Context, job *Job, logFile *os.File) (string, error) {
	expr, err := r.expression(ctx, job, logFile)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(r.store.SpecPath(job.ID), []byte(expr), 0o644); err != nil {
		return "", fmt.Errorf("writing the job expression: %w", err)
	}
	fmt.Fprintf(logFile, "--- job expression written to %s\n", r.store.SpecPath(job.ID))

	// --out-link is not a convenience: it is the GC root that keeps this job's
	// artifacts alive until the job is deleted. Without it a nightly
	// nix-collect-garbage would remove results the caller has not fetched yet.
	cmd := exec.CommandContext(ctx, "nix", "build",
		"--file", r.store.SpecPath(job.ID),
		"--out-link", r.store.ResultPath(job.ID),
		"--print-out-paths",
		"--print-build-logs",
	)

	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = logFile

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("nix build: %w", err)
	}

	path := strings.TrimSpace(out.String())
	if path == "" {
		return "", fmt.Errorf("nix build produced no output path")
	}
	fmt.Fprintf(logFile, "\n--- result: %s\n", path)
	return path, nil
}

// summarise lifts the two fields worth having in a job listing out of the
// build's own report, so that listing jobs does not mean reading a file per job
// out of the store. A missing or unreadable report is not an error: the full
// report is served from the artifacts endpoint either way.
func (r *Runner) summarise(storePath string) (*bool, string) {
	raw, err := os.ReadFile(storePath + "/report.json")
	if err != nil {
		return nil, ""
	}
	var report struct {
		Passed        bool   `json:"passed"`
		KernelRelease string `json:"kernel_release"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		log.Printf("job report at %s is not readable: %v", storePath, err)
		return nil, ""
	}
	return &report.Passed, report.KernelRelease
}

// expression renders the job as a Nix file, doing whatever preparation that
// needs first — fetching the commit into the mirror, writing the patch out,
// writing the test script out.
//
// Every value interpolated below has been through Spec.validate: the commit is
// 40 hex characters, the config base matches [a-z0-9_]+, symbols are Kconfig
// names with values of y/m/n, module and selftest names are word-shaped, and
// the sizes are integers inside fixed bounds. The paths are the daemon's own,
// and the two pieces of genuinely free-form text a caller sends — the patch
// and the test script — are written to files and read back with
// builtins.readFile rather than interpolated. So there is nothing a caller
// controls that can escape a string literal, which matters because this text
// is evaluated.
func (r *Runner) expression(ctx context.Context, job *Job, logFile *os.File) (string, error) {
	if job.Spec.Kind == KindTest {
		return r.testExpression(ctx, job, logFile)
	}
	return r.buildExpression(ctx, job, logFile)
}

func (r *Runner) buildExpression(ctx context.Context, job *Job, logFile *os.File) (string, error) {
	kernel, err := r.kernelFromSpec(ctx, job, logFile, "", "", job.Spec.Config.Set)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(`# Generated by rig-buildd for job %s. Kept so that a build can be
# reproduced by hand: nix build -f this-file -L
let
  pkgs = import %s { system = "x86_64-linux"; };
  kernelBuild = pkgs.callPackage %s { };
in
%s
`, job.ID, r.cfg.Nixpkgs, r.cfg.KernelBuild, strings.TrimSpace(kernel)), nil
}

// testExpression renders a test job.
//
// Two shapes, and the difference is only in how the kernel is named. A job
// that describes one gets the same `kernelBuild.build` call a build job gets,
// which is what makes the two share a store path and makes testing a kernel
// somebody already built cost the boot and nothing else. A job that names an
// earlier one gets `builtins.storePath`, which both points at the result and
// registers the dependency, so the referenced build cannot be collected out
// from under a test that is still running.
func (r *Runner) testExpression(ctx context.Context, job *Job, logFile *os.File) (string, error) {
	var (
		kernel string
		source string
		name   string
		err    error
	)

	if job.Spec.From != "" {
		var ref kernelRef
		if ref, err = resolveFrom(r.store, job.Spec.From); err != nil {
			return "", err
		}
		fmt.Fprintf(logFile, "--- booting the kernel from job %s: %s\n", job.Spec.From, ref.StorePath)
		kernel = fmt.Sprintf("builtins.storePath %s", ref.StorePath)
		source = fmt.Sprintf("\n  source = builtins.storePath %s + \"/source\";", ref.Source)
		name = ref.Name
	} else {
		if kernel, err = r.kernelFromSpec(ctx, job, logFile, "  ", "", job.Spec.Config.Set); err != nil {
			return "", err
		}
		name = "linux-" + job.Spec.Kernel.Commit[:12]
	}

	spec := job.Spec.Test

	script := "null"
	if spec.Script != "" {
		path := filepath.Join(r.store.dir(job.ID), "test.py")
		if err := os.WriteFile(path, []byte(spec.Script), 0o644); err != nil {
			return "", fmt.Errorf("writing the test script: %w", err)
		}
		script = fmt.Sprintf("builtins.readFile %s", path)
	}

	// Left out entirely when the caller named none, so that the default set
	// stays nix/kernel-test's to choose. An empty list here would mean "run no
	// selftests", which is a different request.
	targets := ""
	if named := spec.KselftestTargets(); len(named) > 0 {
		targets = fmt.Sprintf("\n  kselftestTargets = %s;", nixStringList(named))
	}

	return fmt.Sprintf(`# Generated by rig-buildd for job %s. Kept so that a test can be
# reproduced by hand: nix build -f this-file -L
let
  pkgs = import %s { system = "x86_64-linux"; };
  kernelBuild = pkgs.callPackage %s { };
  kernelTest = pkgs.callPackage %s { };
in
kernelTest.run {
  name = %q;

  kernel = %s;%s

  script = %s;
  modules = %s;
  kselftest = %t;%s

  timeoutSeconds = %d;
  memoryMB = %d;
  cpus = %d;
  failOnWarning = %t;
}
`,
		job.ID,
		r.cfg.Nixpkgs,
		r.cfg.KernelBuild,
		r.cfg.KernelTest,
		name,
		kernel,
		source,
		script,
		nixStringList(spec.Modules),
		spec.KselftestEnabled(),
		targets,
		spec.TimeoutSeconds,
		spec.MemoryMB,
		spec.CPUs,
		spec.FailOnWarning,
	), nil
}

// kernelFromSpec renders the `kernelBuild.build { ... }` call for a job that
// describes its kernel, and makes sure the commit is in the mirror first. The
// indent is the caller's because the same text is a whole expression in a
// build job and a nested attribute in a test job.
//
// configWrap is a function to apply to the configuration attrset — empty for
// build and test jobs, and "kernelFuzz.configSetFor" for a fuzz job, whose
// symbols are the caller's request with syzkaller's requirements forced over
// the top of it. configSet is passed in rather than read off the spec because
// a fuzz job's may have a profile's symbols merged underneath the caller's.
func (r *Runner) kernelFromSpec(ctx context.Context, job *Job, logFile *os.File,
	indent, configWrap string, configSet map[string]string) (string, error) {

	if err := r.mirror.Ensure(ctx, job.Spec.Kernel.Commit, job.Spec.Kernel.Remote, logFile); err != nil {
		return "", err
	}

	patch := "null"
	if job.Spec.Patch != "" {
		path := filepath.Join(r.store.dir(job.ID), "candidate.patch")
		if err := os.WriteFile(path, []byte(job.Spec.Patch), 0o644); err != nil {
			return "", fmt.Errorf("writing the candidate patch: %w", err)
		}
		patch = fmt.Sprintf("builtins.readFile %s", path)
	}

	var set strings.Builder
	symbols := make([]string, 0, len(configSet))
	for symbol := range configSet {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	for _, symbol := range symbols {
		fmt.Fprintf(&set, "%s    %s = %q;\n", indent, symbol, configSet[symbol])
	}

	if configWrap != "" {
		configWrap += " "
	}

	return fmt.Sprintf(`kernelBuild.build {
%[1]s  commit = %[2]q;
%[1]s  configBase = %[3]q;
%[1]s  arch = %[4]q;

%[1]s  configSet = %[8]s{
%[5]s%[1]s  };

%[1]s  src = builtins.fetchGit {
%[1]s    url = "file://%[6]s";
%[1]s    rev = %[2]q;
%[1]s    allRefs = true;
%[1]s  };

%[1]s  patch = %[7]s;
%[1]s}`,
		indent,
		job.Spec.Kernel.Commit,
		job.Spec.Config.Base,
		job.Spec.Arch,
		set.String(),
		r.mirror.path,
		patch,
		configWrap,
	), nil
}

// kernelRef is where an earlier job's kernel is, and what is needed to test it.
type kernelRef struct {
	// The referenced job's output: the kernel to boot.
	StorePath string
	// The unpatched build it came from, whose $out holds the source tree. A
	// patched build does not keep one — it is several gigabytes and no caller
	// wants it — but it does record where its base is, and the base is
	// retained for as long as the patched output exists because that record is
	// a store reference. Building selftests needs that tree.
	Source string
	Name   string
	// The commit it was built from. Needed by a fuzz job that named an earlier
	// build: the focus pre-flight checks its filters against that kernel tree.
	Commit string
}

// resolveFrom finds the kernel a `from` test should boot, and refuses the
// cases where booting it would waste half an hour to learn nothing.
func resolveFrom(store *Store, id string) (kernelRef, error) {
	job, ok := store.Get(id)
	if !ok {
		return kernelRef{}, fmt.Errorf("no job %q on this rig to boot", id)
	}
	if job.Spec.Kind != KindBuild {
		return kernelRef{}, fmt.Errorf("job %s is a %s job; from must name a build", id, job.Spec.Kind)
	}
	if job.State != StateSucceeded || job.StorePath == "" {
		return kernelRef{}, fmt.Errorf("job %s is %s and has produced no kernel to boot", id, job.State)
	}
	if job.Passed != nil && !*job.Passed {
		return kernelRef{}, fmt.Errorf("job %s did not compile; there is nothing to boot", id)
	}

	// Defensive rather than expected: validate guarantees 40 hex characters
	// for a build job, but this reads a job.json off disk that some earlier
	// version of the daemon wrote, and a slice out of range here would take
	// the whole rig down rather than fail one job.
	name := "linux"
	if len(job.Spec.Kernel.Commit) >= 12 {
		name = "linux-" + job.Spec.Kernel.Commit[:12]
	}

	ref := kernelRef{
		StorePath: job.StorePath,
		Commit:    job.Spec.Kernel.Commit,
		// A job with no patch *is* its own base, and its output has the source
		// tree in it directly.
		Source: job.StorePath,
		Name:   name,
	}

	raw, err := os.ReadFile(filepath.Join(job.StorePath, "report.json"))
	if err != nil {
		return kernelRef{}, fmt.Errorf("job %s has no readable report: %w", id, err)
	}
	var report struct {
		BaseStorePath string `json:"base_store_path"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return kernelRef{}, fmt.Errorf("job %s has an unreadable report: %w", id, err)
	}
	if report.BaseStorePath != "" {
		ref.Source = report.BaseStorePath
	}

	return ref, nil
}

// nixStringList renders a Go slice as a Nix list. %q is Go's quoting, which
// agrees with Nix's for the word-shaped strings validate lets through.
func nixStringList(values []string) string {
	if len(values) == 0 {
		return "[ ]"
	}
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return "[ " + strings.Join(quoted, " ") + " ]"
}
