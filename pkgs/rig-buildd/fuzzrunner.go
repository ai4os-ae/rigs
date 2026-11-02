package main

// Running a fuzz job.
//
// Everything else this daemon does ends in `nix build`, because everything
// else it does is a pure function of its inputs. A fuzzing run is not: it is a
// process that runs for hours, whose findings depend on how long it ran and
// how lucky it was, and whose corpus is worth more than any single run of it.
//
// So the derivation stops at the inputs. nix/kernel-fuzz gathers the kernel,
// the guest image and the fuzzer into one store path — cached, shared between
// jobs, and held by one GC root — and everything below supervises syz-manager
// against that path and against a deadline.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// How long syz-manager is given to shut down cleanly after its deadline.
//
// It flushes the corpus on SIGINT, and the corpus is the thing a bounded run
// exists to accumulate — so this is generous. A run killed before it writes
// corpus.db has thrown away most of what it achieved.
const fuzzShutdownGrace = 60 * time.Second

// A run that dies this quickly did not fail at fuzzing, it failed to start:
// a focus filter that matched nothing, an image syzkaller would not accept, a
// kernel with no coverage. Those are the caller's to fix and deserve to be
// reported as such rather than as an empty campaign.
const fuzzStartupWindow = 90 * time.Second

// runFuzz is the fuzz counterpart of Runner.run. It returns the directory
// holding the run's results, which is the syzkaller workdir itself.
func (r *Runner) runFuzz(ctx context.Context, job *Job, logFile *os.File) (string, error) {
	if r.cfg.KernelFuzz == "" {
		return "", fmt.Errorf("this rig is not configured to run fuzz jobs")
	}
	spec := job.Spec.Fuzz

	// Checked before anything expensive: a mistyped focus filter would
	// otherwise cost a kernel build and then be reported by syz-manager
	// refusing to start.
	if err := r.preflightFocus(ctx, job, logFile); err != nil {
		return "", err
	}

	sshKey, err := r.ensureSSHKey()
	if err != nil {
		return "", err
	}

	env, err := r.buildFuzzEnv(ctx, job, logFile, sshKey+".pub")
	if err != nil {
		return "", err
	}

	workdir := filepath.Join(r.store.dir(job.ID), "workdir")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return "", fmt.Errorf("creating the fuzzing workdir: %w", err)
	}

	// Published before the run starts, not after it. A four-hour campaign's
	// crashes are worth reading while it is still going, and this is what lets
	// the artifact endpoints serve a reproducer out of a job in progress.
	_ = r.store.Update(job.ID, func(j *Job) { j.Artifacts = workdir })

	if err := r.seedCorpus(spec, workdir, logFile); err != nil {
		// A corpus that will not load is a slow start, not a failed job.
		fmt.Fprintf(logFile, "--- could not seed the corpus: %v\n", err)
	}

	vm := sizeVMs(spec.VM, r.cfg.Limits)
	fmt.Fprintf(logFile, "--- %d VMs of %d vCPU / %d MiB (%s)\n",
		vm.Count, vm.CPU, vm.MemoryMB, vm.Reason)

	port, err := freePort()
	if err != nil {
		return "", err
	}

	cfg := renderConfig(job, env, workdir, sshKey, port, vm)
	cfgPath := filepath.Join(r.store.dir(job.ID), "syzkaller.cfg")
	raw, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return "", fmt.Errorf("rendering the manager config: %w", err)
	}
	if err := os.WriteFile(cfgPath, append(raw, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("writing the manager config: %w", err)
	}
	fmt.Fprintf(logFile, "--- manager config written to %s\n", cfgPath)
	fmt.Fprintf(logFile, "--- web UI on http://127.0.0.1:%d for as long as this runs\n", port)

	started := time.Now()
	runErr := r.superviseManager(ctx, job, env, cfgPath, logFile, workdir)
	elapsed := time.Since(started)

	// Saved even when the run ended badly. A campaign that crashed the manager
	// after three hours still learned three hours' worth of programs.
	if err := r.saveCorpus(spec, workdir, logFile); err != nil {
		fmt.Fprintf(logFile, "--- could not save the corpus: %v\n", err)
	}

	if runErr != nil && elapsed < fuzzStartupWindow {
		return "", fmt.Errorf("syz-manager stopped after %s without fuzzing: %w",
			elapsed.Round(time.Second), runErr)
	}

	if err := r.harvest(job, env, workdir, elapsed); err != nil {
		return "", fmt.Errorf("collecting the run's results: %w", err)
	}
	return workdir, nil
}

// ensureSSHKey returns the key syz-manager authenticates to guests with,
// generating it once per rig.
//
// Generated rather than committed. The guest image is a derivation and needs
// the public half at evaluation time, which makes a checked-in keypair the
// obvious answer and a bad one: this repository states plainly that it does
// not carry private keys, and a key in git is a key on every machine that has
// ever cloned it. Generating here costs one ssh-keygen per rig and keeps the
// guest derivation pure — it takes the public half as a string.
func (r *Runner) ensureSSHKey() (string, error) {
	dir := filepath.Join(r.cfg.StateDir, "fuzz")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	key := filepath.Join(dir, "id_ed25519")

	if _, err := os.Stat(key); err == nil {
		return key, nil
	}

	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "rig-fuzz", "-f", key)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("generating the fuzzing ssh key: %w: %s", err, out)
	}
	return key, nil
}

// preflightFocus checks each file filter against the kernel tree before the
// build, using the mirror rather than a checkout.
//
// syz-manager refuses to start when a filter matches no compile unit, which is
// the right behaviour and the wrong moment: by then the job has paid for a
// KASAN kernel build. The regular expressions are Go's on both sides, so a
// match here means the same thing it will mean there.
//
// Function filters are not checked — the compiled symbol set is not knowable
// from the source tree — so syz-manager remains the authority for those.
func (r *Runner) preflightFocus(ctx context.Context, job *Job, logFile *os.File) error {
	spec := job.Spec.Fuzz

	var patterns []*regexp.Regexp
	var sources []string
	for _, area := range spec.Focus {
		for _, filter := range area.Files {
			re, err := regexp.Compile(filter)
			if err != nil {
				return fmt.Errorf("focus area %q: %q is not a valid regular expression: %w",
					area.Name, filter, err)
			}
			patterns = append(patterns, re)
			sources = append(sources, area.Name+": "+filter)
		}
	}
	if len(patterns) == 0 {
		return nil
	}

	commit := job.Spec.Kernel.Commit
	if commit == "" {
		// A job that named an earlier build inherits that build's commit.
		ref, err := resolveFrom(r.store, job.Spec.From)
		if err != nil {
			return err
		}
		commit = ref.Commit
	}
	if commit == "" {
		return nil
	}

	if err := r.mirror.Ensure(ctx, commit, job.Spec.Kernel.Remote, logFile); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "-C", r.mirror.path,
		"ls-tree", "-r", "--name-only", commit)
	out, err := cmd.Output()
	if err != nil {
		// Not fatal: this check is an early warning, not a gate. syz-manager
		// still performs the real one.
		fmt.Fprintf(logFile, "--- could not list the kernel tree to check focus filters: %v\n", err)
		return nil
	}

	matched := make([]bool, len(patterns))
	for _, path := range strings.Split(string(out), "\n") {
		for i, re := range patterns {
			if !matched[i] && re.MatchString(path) {
				matched[i] = true
			}
		}
	}

	for i, ok := range matched {
		if !ok {
			return fmt.Errorf("focus filter %q matches nothing in the kernel at %s; "+
				"syzkaller would refuse to start rather than fuzz an empty area",
				sources[i], commit[:12])
		}
	}

	fmt.Fprintf(logFile, "--- %d focus filters all match the kernel tree\n", len(patterns))
	return nil
}

// fuzzExpression renders the job as a Nix file: a kernel built with
// syzkaller's requirements forced into its configuration, and an environment
// gathered around it.
//
// The same safety argument as Runner.expression applies, and is why this is
// safe to evaluate: everything interpolated has been through validation. The
// commit is 40 hex characters, config symbols are Kconfig names with values of
// y/m/n, module names are word-shaped, and the two pieces of genuinely
// free-form text a caller can send — the patch and, here, nothing else — are
// written to files and read back with builtins.readFile rather than
// interpolated. Focus areas never reach this file at all: they are the
// manager's configuration, not the kernel's.
func (r *Runner) fuzzExpression(ctx context.Context, job *Job, logFile *os.File, pubKeyPath string) (string, error) {
	spec := job.Spec.Fuzz

	// A profile's configuration symbols sit underneath the caller's, and
	// nix/kernel-fuzz/config.nix forces syzkaller's own over the top of both.
	configSet := map[string]string{}
	if spec.Profile != "" {
		profile, err := loadProfile(r.profileDir(), spec.Profile)
		if err != nil {
			return "", err
		}
		maps.Copy(configSet, profile.Config)
	}
	maps.Copy(configSet, job.Spec.Config.Set)

	var (
		kernel string
		source string
		name   string
		err    error
	)

	if job.Spec.From != "" {
		// Booting a kernel somebody already built. It has to be one built for
		// this: a defconfig kernel has no KCOV, and syz-manager would spend
		// half a minute booting VMs before saying so.
		var ref kernelRef
		if ref, err = resolveFrom(r.store, job.Spec.From); err != nil {
			return "", err
		}
		if err := requireFuzzableConfig(ref); err != nil {
			return "", err
		}
		fmt.Fprintf(logFile, "--- fuzzing the kernel from job %s: %s\n", job.Spec.From, ref.StorePath)
		kernel = fmt.Sprintf("builtins.storePath %s", ref.StorePath)
		source = fmt.Sprintf("\n  source = builtins.storePath %s + \"/source\";", ref.Source)
		name = ref.Name
	} else {
		if kernel, err = r.kernelFromSpec(ctx, job, logFile, "  ", "kernelFuzz.configSetFor", configSet); err != nil {
			return "", err
		}
		name = "linux-" + job.Spec.Kernel.Commit[:12]
	}

	// Read rather than interpolated: it is a file on disk either way, and this
	// keeps the expression free of anything shaped like a credential.
	pubKey := fmt.Sprintf("builtins.readFile %s", pubKeyPath)

	return fmt.Sprintf(`# Generated by rig-buildd for job %s. Kept so that a fuzzing run can be
# reproduced by hand: nix build -f this-file -L, then point syz-manager at
# the env.json in the result.
let
  pkgs = import %s { system = "x86_64-linux"; };
  kernelBuild = pkgs.callPackage %s { };
  kernelFuzz = pkgs.callPackage %s {
    syzkaller = pkgs.callPackage %s/package.nix { };
  };
in
kernelFuzz.env {
  name = %q;

  kernel = %s;%s

  authorizedKey = %s;
  modules = %s;
}
`,
		job.ID,
		r.cfg.Nixpkgs,
		r.cfg.KernelBuild,
		r.cfg.KernelFuzz,
		r.cfg.SyzkallerPkg,
		name,
		kernel,
		source,
		pubKey,
		nixStringList(spec.Modules),
	), nil
}

// requireFuzzableConfig refuses a `from` job whose kernel cannot be fuzzed.
//
// The build's own report carries the verified configuration — kernel-build
// re-reads every requested symbol out of the resulting .config — so this is
// checked rather than assumed. Without KCOV there is no coverage feedback, and
// syzkaller degrades to random syscall generation without ever saying so.
func requireFuzzableConfig(ref kernelRef) error {
	raw, err := os.ReadFile(filepath.Join(ref.StorePath, "report.json"))
	if err != nil {
		return fmt.Errorf("the referenced build has no readable report: %w", err)
	}

	var report struct {
		Config *struct {
			Requested map[string]string `json:"requested"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return fmt.Errorf("the referenced build has an unreadable report: %w", err)
	}

	if report.Config == nil || report.Config.Requested["KCOV"] != "y" {
		return fmt.Errorf("the kernel from that job was not built for fuzzing: it has no CONFIG_KCOV, "+
			"so there would be no coverage feedback. Submit a fuzz job describing the kernel "+
			"instead of naming an earlier build, and this rig will build one with %s",
			"KCOV, KASAN and debug info")
	}
	return nil
}

// profileDir is where the curated focus profiles live inside the kernel-fuzz
// store path.
func (r *Runner) profileDir() string { return filepath.Join(r.cfg.KernelFuzz, "focus") }

// buildFuzzEnv realises nix/kernel-fuzz's env for this job and reads it back.
func (r *Runner) buildFuzzEnv(ctx context.Context, job *Job, logFile *os.File, pubKeyPath string) (*fuzzEnv, error) {
	expr, err := r.fuzzExpression(ctx, job, logFile, pubKeyPath)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(r.store.SpecPath(job.ID), []byte(expr), 0o644); err != nil {
		return nil, fmt.Errorf("writing the job expression: %w", err)
	}
	fmt.Fprintf(logFile, "--- job expression written to %s\n", r.store.SpecPath(job.ID))

	// The GC root. A run holds its kernel for hours while a rig collects
	// garbage on a timer, so this is not a convenience.
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
		return nil, fmt.Errorf("nix build: %w", err)
	}

	root := strings.TrimSpace(out.String())
	if root == "" {
		return nil, fmt.Errorf("nix build produced no output path")
	}
	fmt.Fprintf(logFile, "\n--- fuzzing environment: %s\n", root)

	_ = r.store.Update(job.ID, func(j *Job) { j.StorePath = root })
	return readFuzzEnv(root)
}

// superviseManager runs syz-manager until its deadline, the job is cancelled,
// or it exits on its own.
func (r *Runner) superviseManager(ctx context.Context, job *Job, env *fuzzEnv,
	cfgPath string, logFile *os.File, workdir string) error {

	spec := job.Spec.Fuzz
	deadline := time.Duration(spec.DurationSeconds) * time.Second

	// Two contexts on purpose. The inner one expires at the run's deadline and
	// is a *successful* end; the outer one is cancellation and is not. Sharing
	// one would make a completed run indistinguishable from a cancelled job.
	runCtx, stop := context.WithTimeout(ctx, deadline)
	defer stop()

	cmd := exec.Command(filepath.Join(env.Syzkaller, "bin", "syz-manager"), "-config", cfgPath)

	// CPATH MUST NOT REACH syz-manager, and this is the least obvious line in
	// this file.
	//
	// To build a C reproducer syzkaller preprocesses its executor header with
	// `cpp -nostdinc -fdirectives-only` and *relies* on every #include failing
	// to resolve, so the directives survive into the reproducer rather than
	// being expanded inline. -nostdinc does not clear CPATH, so a CPATH with
	// anything in it gives cpp somewhere to look, and a miss becomes a fatal
	// "no such file" instead of the non-fatal diagnostic upstream expects. cpp
	// then dies twenty lines in, and because syzkaller only treats *empty* cpp
	// output as failure it accepts the truncated header and emits a reproducer
	// with no main(), which fails to link. Every bug is then reported as not
	// reproducible in C.
	//
	// LIBRARY_PATH is the other half and is set by the unit — see
	// modules/rig/buildd.nix.
	cmd.Env = append(os.Environ(), "CPATH=")

	// One pipe for both streams, so the log reads in the order syz-manager
	// wrote it: it puts its statistics on stdout and its crash reports on
	// stderr, and interleaving them after the fact is guesswork.
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = writer
	cmd.Stderr = writer

	if err := cmd.Start(); err != nil {
		writer.Close()
		reader.Close()
		return fmt.Errorf("starting syz-manager: %w", err)
	}
	// The child holds its own descriptor now. Ours has to go, or the scanner
	// below never sees EOF and the goroutine outlives the run.
	writer.Close()
	defer reader.Close()

	started := time.Now()
	progress := &FuzzProgress{DurationSeconds: spec.DurationSeconds}

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(logFile, line)

			if progress.update(line) {
				progress.ElapsedSeconds = int(time.Since(started).Seconds())
				progress.Crashes = countCrashes(workdir)
				snapshot := *progress
				_ = r.store.Update(job.ID, func(j *Job) { j.Fuzz = &snapshot })
			}
		}
	}()

	// Wait for whichever comes first: the process ending, or the deadline.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		<-done
		return err

	case <-runCtx.Done():
		// SIGINT rather than SIGKILL: syz-manager writes corpus.db on its way
		// out, and that corpus is most of what a bounded run is for.
		fmt.Fprintf(logFile, "\n--- %s elapsed; asking syz-manager to stop and flush its corpus\n",
			time.Since(started).Round(time.Second))
		_ = cmd.Process.Signal(os.Interrupt)

		select {
		case <-exited:
		case <-time.After(fuzzShutdownGrace):
			fmt.Fprintf(logFile, "--- syz-manager did not stop within %s; killing it\n", fuzzShutdownGrace)
			_ = cmd.Process.Kill()
			<-exited
		}
		<-done

		// A run stopped by its own deadline finished. A run stopped because
		// the job was cancelled did not, and Runner.build tells them apart by
		// asking the outer context.
		return nil
	}
}

// Crash is one distinct bug syzkaller found.
type Crash struct {
	// syzkaller's own directory name for it: a hash of the title.
	ID    string `json:"id"`
	Title string `json:"title"`
	// How many times it was hit. A crash seen once may be a fluke; one seen
	// two hundred times is in the way of everything else.
	Count int `json:"count"`

	// A syz program always, a C program only when syzkaller managed to
	// translate it. The C one is what a maintainer wants.
	Repro  bool `json:"repro"`
	CRepro bool `json:"crepro"`

	// Relative to the job's artifacts, so a client can fetch it directly.
	Path string `json:"path"`
}

// readCrashes reads syzkaller's crashes directory. Safe to call while the run
// is still going, which is the point: GET /v1/jobs/{id}/crashes answers from
// this.
func readCrashes(workdir string) []Crash {
	root := filepath.Join(workdir, "crashes")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []Crash
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())

		title, err := os.ReadFile(filepath.Join(dir, "description"))
		if err != nil {
			// Being written right now, most likely.
			continue
		}

		crash := Crash{
			ID:    entry.Name(),
			Title: strings.TrimSpace(string(title)),
			Path:  filepath.Join("crashes", entry.Name()),
		}

		files, _ := os.ReadDir(dir)
		for _, file := range files {
			switch {
			case strings.HasPrefix(file.Name(), "log"):
				crash.Count++
			case file.Name() == "repro.prog":
				crash.Repro = true
			case file.Name() == "repro.cprog":
				crash.CRepro = true
			}
		}
		out = append(out, crash)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Title < out[j].Title
	})
	return out
}

func countCrashes(workdir string) int { return len(readCrashes(workdir)) }

// harvest writes the run's report.json.
//
// The schema is nix/kernel-build's, so that a client reads a fuzz job with the
// code it already has for builds and tests: `passed`, `kernel_release`, and
// the details underneath.
//
// WHAT `passed` MEANS HERE, because it reads backwards from what a person
// wants. The rule everywhere in this API is that `passed` describes the
// kernel, not whether the caller got what they hoped for — a patch that does
// not compile is a successful job with passed=false. A fuzzing run that found
// crashes is the same shape: the job did exactly what was asked, and the
// kernel did not survive it. So crashes mean passed=false, and a clean run to
// the deadline means passed=true.
func (r *Runner) harvest(job *Job, env *fuzzEnv, workdir string, elapsed time.Duration) error {
	crashes := readCrashes(workdir)

	release := ""
	if raw, err := os.ReadFile(filepath.Join(env.root, "kernel-release")); err == nil {
		release = strings.TrimSpace(string(raw))
	}

	progress := job.Fuzz
	if latest, ok := r.store.Get(job.ID); ok && latest.Fuzz != nil {
		progress = latest.Fuzz
	}

	if crashes == nil {
		crashes = []Crash{}
	}

	artifacts, err := walkArtifacts(workdir)
	if err != nil {
		return err
	}

	report := map[string]any{
		"schema":         "rig_kernel_build_report_v1",
		"stage":          "fuzz",
		"passed":         len(crashes) == 0,
		"kernel_release": release,

		"fuzz": map[string]any{
			"duration_seconds": job.Spec.Fuzz.DurationSeconds,
			"elapsed_seconds":  int(elapsed.Seconds()),
			"focus":            job.Spec.Fuzz.Focus,
			"profile":          job.Spec.Fuzz.Profile,
			"progress":         progress,
		},
		"crashes": crashes,

		// Same shape kernel-build's report.py produces, so that a client reads
		// the manifest for a fuzz job with the code it already has.
		"artifacts": artifacts,

		// No `commands`: a fuzzing run is one long process, not a sequence of
		// recorded steps, and an empty list is more honest than a synthetic
		// entry that says the same thing as `passed`.
		"commands": []any{},
	}

	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workdir, "report.json"), append(raw, '\n'), 0o644)
}

// walkArtifacts hashes what a run produced, in the same shape
// nix/kernel-build/report.py writes, so that GET /v1/jobs/{id}/artifacts
// answers the same way for every kind of job.
//
// Deliberately not everything in the workdir. A syzkaller workdir holds the
// per-VM scratch directories and, in a long campaign, a corpus of tens of
// megabytes; hashing all of it at the end of every run would cost minutes for
// a manifest describing files nobody fetches individually. What is kept is
// what somebody actually came for: the crashes, their reproducers, and the
// corpus as a single artifact.
func walkArtifacts(workdir string) (map[string]any, error) {
	artifacts := map[string]any{}

	add := func(path string) error {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil //nolint:nilerr // a file that vanished mid-run is not an error
		}
		rel, err := filepath.Rel(workdir, path)
		if err != nil {
			return err
		}

		file, err := os.Open(path)
		if err != nil {
			return nil //nolint:nilerr
		}
		defer file.Close()

		digest := sha256.New()
		if _, err := io.Copy(digest, file); err != nil {
			return err
		}
		artifacts[rel] = map[string]any{
			"size":   info.Size(),
			"sha256": hex.EncodeToString(digest.Sum(nil)),
		}
		return nil
	}

	if err := add(filepath.Join(workdir, "corpus.db")); err != nil {
		return nil, err
	}

	crashes := filepath.Join(workdir, "crashes")
	err := filepath.WalkDir(crashes, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// No crashes directory at all is the good outcome, not a failure.
			return nil //nolint:nilerr
		}
		if entry.IsDir() {
			return nil
		}
		return add(path)
	})
	if err != nil {
		return nil, err
	}

	return artifacts, nil
}
