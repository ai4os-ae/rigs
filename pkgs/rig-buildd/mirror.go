package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Mirror is the rig's own bare clone of the kernel.
//
// A job names a commit, not a source tree. Shipping a prepared worktree with
// every job would be about 1.5GB over campus wifi each time; keeping a mirror
// here means a job payload is a commit hash and a patch, and the clone is paid
// for once. It is also what lets the build itself stay pure: `builtins.fetchGit`
// reads this repository from the local filesystem, so nothing reaches the
// network once the commit is present.
type Mirror struct {
	path   string
	remote string

	// Serialised: two jobs wanting the same missing commit must not run
	// concurrent fetches into one repository.
	mu sync.Mutex
}

func NewMirror(path, remote string) *Mirror {
	return &Mirror{path: path, remote: remote}
}

func (m *Mirror) run(ctx context.Context, log io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = log
	cmd.Stderr = log
	fmt.Fprintf(log, "--- git %s\n", args)
	return cmd.Run()
}

func (m *Mirror) has(ctx context.Context, commit string) bool {
	// ^{commit} so that a hash which happens to name a tree or a blob is not
	// mistaken for a usable revision.
	cmd := exec.CommandContext(ctx, "git", "-C", m.path, "cat-file", "-e", commit+"^{commit}")
	return cmd.Run() == nil
}

// Ensure makes commit available locally, cloning or fetching if it is not.
// remote overrides the rig's default for this job; callers normally leave it
// empty.
func (m *Mirror) Ensure(ctx context.Context, commit, remote string, log io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if remote == "" {
		remote = m.remote
	}

	if _, err := os.Stat(filepath.Join(m.path, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
			return err
		}
		fmt.Fprintf(log, "--- no mirror yet; cloning %s (this takes a while, once)\n", remote)
		if err := m.run(ctx, log, "clone", "--mirror", remote, m.path); err != nil {
			return fmt.Errorf("cloning the kernel mirror from %s: %w", remote, err)
		}
	}

	if m.has(ctx, commit) {
		return nil
	}

	fmt.Fprintf(log, "--- %s is not in the mirror; fetching from %s\n", commit, remote)
	if err := m.run(ctx, log, "-C", m.path, "fetch", "--prune", "--tags", remote, "+refs/heads/*:refs/heads/*"); err != nil {
		fmt.Fprintf(log, "--- fetching refs failed: %v\n", err)
	}
	if m.has(ctx, commit) {
		return nil
	}

	// The commit may not be reachable from any branch — a topic branch, or a
	// commit somebody pushed to a personal tree. Asking for it by hash works
	// if the server allows it, and is the only remaining option.
	fmt.Fprintf(log, "--- still missing; asking %s for the commit directly\n", remote)
	if err := m.run(ctx, log, "-C", m.path, "fetch", remote, commit); err != nil {
		return fmt.Errorf("commit %s is not in the mirror and %s would not serve it: %w", commit, remote, err)
	}
	if !m.has(ctx, commit) {
		return fmt.Errorf("commit %s is still not present after fetching from %s", commit, remote)
	}

	return nil
}

// Status reports what the mirror is, for GET /v1/status. Callers use it to see
// whether a rig will need a long clone before it can start.
func (m *Mirror) Status() map[string]any {
	status := map[string]any{"path": m.path, "remote": m.remote}
	if _, err := os.Stat(filepath.Join(m.path, "HEAD")); err != nil {
		status["present"] = false
		return status
	}
	status["present"] = true
	return status
}
