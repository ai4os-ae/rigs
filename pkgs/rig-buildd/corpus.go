package main

// The shared corpus, and the small things a fuzz run needs around it.
//
// A syzkaller corpus is the accumulated set of programs that reached somewhere
// interesting. It is worth far more than any single run: an hour of a cold
// start is an hour spent rediscovering what the last run already knew, and a
// bounded four-hour job that always starts cold is mostly a rediscovery
// exercise.
//
// So a rig keeps one corpus per campaign, at
// <state>/fuzz-corpus/<fingerprint>/corpus.db, seeds each run from it, and
// writes it back afterwards. The fingerprint is over what the fuzzer was
// aiming at, not over the kernel — see corpusFingerprint.

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

// seedCorpus copies the campaign's accumulated corpus into a fresh workdir.
func (r *Runner) seedCorpus(spec *FuzzSpec, workdir string, logFile *os.File) error {
	if spec.Corpus == "cold" {
		fmt.Fprintf(logFile, "--- starting from an empty corpus, as asked\n")
		return nil
	}

	source := r.corpusPath(spec)
	if jobIDRE.MatchString(spec.Corpus) {
		// Seeding from a named job rather than the shared corpus: useful for
		// continuing one specific campaign, or for reproducing what a run had
		// available when it found something.
		other, ok := r.store.Get(spec.Corpus)
		if !ok {
			return fmt.Errorf("no job %q on this rig to take a corpus from", spec.Corpus)
		}
		source = filepath.Join(r.store.dir(other.ID), "workdir", "corpus.db")
	}

	target := filepath.Join(workdir, "corpus.db")
	if _, err := os.Stat(target); err == nil {
		// A requeued job — the rig rebooted mid-run — already has one, and it
		// is newer than the shared copy.
		fmt.Fprintf(logFile, "--- resuming with the corpus already in this job's workdir\n")
		return nil
	}

	info, err := os.Stat(source)
	if err != nil {
		fmt.Fprintf(logFile, "--- no corpus for this campaign yet; starting cold\n")
		return nil
	}

	if err := copyFile(source, target); err != nil {
		return err
	}
	fmt.Fprintf(logFile, "--- seeded the corpus from %s (%d KiB)\n", source, info.Size()/1024)
	return nil
}

// saveCorpus writes the run's corpus back to the campaign's shared copy.
//
// Last writer wins, and that is fine: the corpus is a cache of useful
// programs, not a ledger. Two runs of the same campaign finishing at once
// would each have most of the other's programs anyway, and syzkaller
// rediscovers what it is missing in minutes.
func (r *Runner) saveCorpus(spec *FuzzSpec, workdir string, logFile *os.File) error {
	if spec.Corpus == "cold" {
		return nil
	}

	source := filepath.Join(workdir, "corpus.db")
	info, err := os.Stat(source)
	if err != nil {
		// syz-manager was killed before it flushed, or never got far enough to
		// have anything to flush.
		return nil
	}

	target := r.corpusPath(spec)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := copyFile(source, target); err != nil {
		return err
	}
	fmt.Fprintf(logFile, "--- saved %d KiB of corpus to %s\n", info.Size()/1024, target)
	return nil
}

func (r *Runner) corpusPath(spec *FuzzSpec) string {
	return filepath.Join(r.cfg.StateDir, "fuzz-corpus", corpusFingerprint(spec), "corpus.db")
}

// copyFile writes through a temporary file and renames, so that a corpus
// half-written when the power went out is not a corpus that poisons every
// later run of the campaign.
func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := target + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, target)
}

// freePort asks the kernel for an unused port for syz-manager's web interface.
//
// Bound and immediately released rather than picked from a range: several fuzz
// runs can be in flight on a large rig, and a fixed port would mean the second
// one fails to start for a reason that has nothing to do with fuzzing.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding a port for the fuzzer's web interface: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
