package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Runner is the queue. One goroutine per concurrency slot takes job ids off a
// channel and builds them; a rig has twelve threads and a kernel build wants
// all of them, so the default slot count is one and raising it is usually
// wrong.
//
// Nix is the build backend but not the interface: everything a caller sees is
// the HTTP API, and swapping what happens in `build` below would not change it.
// What Nix buys here is that identical work is never repeated — two jobs with
// the same commit, patch and configuration resolve to the same derivation, and
// the second is answered from the store — and that a patched build reuses the
// expensive unpatched base it shares with every other patch against that
// commit.
type Runner struct {
	store  *Store
	mirror *Mirror

	// Store paths, injected by the NixOS module. Pinning nixpkgs here rather
	// than reading NIX_PATH is what keeps a rig's builds reproducible across
	// whatever the channel happens to be.
	nixpkgs     string
	kernelBuild string

	queue chan string

	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	running  map[string]bool
	capacity int
}

func NewRunner(store *Store, mirror *Mirror, nixpkgs, kernelBuild string, capacity int) *Runner {
	return &Runner{
		store:       store,
		mirror:      mirror,
		nixpkgs:     nixpkgs,
		kernelBuild: kernelBuild,
		// Buffered generously: submitting must not block on a busy queue, and
		// a rig will not hold thousands of pending kernel builds.
		queue:    make(chan string, 1024),
		cancels:  map[string]context.CancelFunc{},
		running:  map[string]bool{},
		capacity: capacity,
	}
}

func (r *Runner) Start() {
	for i := 0; i < r.capacity; i++ {
		go func() {
			for id := range r.queue {
				r.build(id)
			}
		}()
	}
}

func (r *Runner) Submit(id string) { r.queue <- id }

func (r *Runner) QueueDepth() int { return len(r.queue) }

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

	storePath, err := r.run(ctx, job, logFile)

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
		// The build ran. Whether the *kernel* compiled is a separate question,
		// answered by report.json, and a failure there is a result rather than
		// an error — see the comment on State.
		passed, release := r.summarise(storePath)
		_ = r.store.Update(id, func(j *Job) {
			j.State = StateSucceeded
			j.StorePath = storePath
			j.Passed = passed
			j.KernelRelease = release
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
	if err := r.mirror.Ensure(ctx, job.Spec.Kernel.Commit, job.Spec.Kernel.Remote, logFile); err != nil {
		return "", err
	}

	patchPath := ""
	if job.Spec.Patch != "" {
		patchPath = r.store.dir(job.ID) + "/candidate.patch"
		if err := os.WriteFile(patchPath, []byte(job.Spec.Patch), 0o644); err != nil {
			return "", fmt.Errorf("writing the candidate patch: %w", err)
		}
	}

	expr := r.expression(job, patchPath)
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

// expression renders the job as a Nix file.
//
// Every value interpolated here has been through Spec.validate: the commit is
// 40 hex characters, the config base matches [a-z0-9_]+, symbols are Kconfig
// names and their values are one of y/m/n. The paths are the daemon's own. So
// there is nothing a caller controls that can escape a string literal — which
// matters, because this text is evaluated.
func (r *Runner) expression(job *Job, patchPath string) string {
	var set strings.Builder
	symbols := make([]string, 0, len(job.Spec.Config.Set))
	for symbol := range job.Spec.Config.Set {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	for _, symbol := range symbols {
		fmt.Fprintf(&set, "    %s = %q;\n", symbol, job.Spec.Config.Set[symbol])
	}

	patch := "null"
	if patchPath != "" {
		patch = fmt.Sprintf("builtins.readFile %s", patchPath)
	}

	return fmt.Sprintf(`# Generated by rig-buildd for job %s. Kept so that a build can be
# reproduced by hand: nix build -f this-file -L
let
  pkgs = import %s { system = "x86_64-linux"; };
  kernelBuild = pkgs.callPackage %s { };
in
kernelBuild.build {
  commit = %q;
  configBase = %q;
  arch = %q;

  configSet = {
%s  };

  src = builtins.fetchGit {
    url = "file://%s";
    rev = %q;
    allRefs = true;
  };

  patch = %s;
}
`,
		job.ID,
		r.nixpkgs,
		r.kernelBuild,
		job.Spec.Kernel.Commit,
		job.Spec.Config.Base,
		job.Spec.Arch,
		set.String(),
		r.mirror.path,
		job.Spec.Kernel.Commit,
		patch,
	)
}
