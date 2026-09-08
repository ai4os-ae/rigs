// rig-buildd runs build jobs on an AI4OS rig and publishes them over HTTP.
//
// A client posts a kernel commit, an optional patch and a configuration; the
// rig builds it and serves back the artifacts, the logs and a report. Nix is
// the backend, which is why identical work is never repeated and why a patched
// build reuses the expensive unpatched base it shares with every other patch
// against the same commit — but none of that is visible here. Clients need no
// Nix, no store access and no SSH trust; they need an HTTP client.
//
// It listens on the mesh interface, where nebula has already authenticated and
// encrypted the connection and restricted it to the ai4os and trusted groups.
// -token-file adds a bearer token on top for when that stops being enough.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9000", "address to serve on, when not socket-activated")
	state := flag.String("state", "/var/lib/rig/buildd", "directory holding jobs and the kernel mirror")
	mirrorPath := flag.String("mirror", "", "bare kernel clone (default: <state>/linux.git)")
	remote := flag.String("remote", "https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git", "where to fetch kernel commits from")
	nixpkgs := flag.String("nixpkgs", "", "store path of the nixpkgs the builds evaluate against (required)")
	kernelBuild := flag.String("kernel-build", "", "store path of the kernel-build expression (required)")
	kernelTest := flag.String("kernel-test", "", "store path of the kernel-test expression (required)")
	capacity := flag.Int("max-concurrent", 1, "builds to run at once; a kernel build wants the whole machine")
	tokenFile := flag.String("token-file", "", "file holding a bearer token; empty means rely on the mesh alone")
	flag.Parse()

	log.SetFlags(0) // The journal already stamps every line.

	if *nixpkgs == "" || *kernelBuild == "" || *kernelTest == "" {
		log.Fatal("-nixpkgs, -kernel-build and -kernel-test are required; they pin what jobs evaluate against")
	}
	if *mirrorPath == "" {
		*mirrorPath = filepath.Join(*state, "linux.git")
	}

	var token string
	if *tokenFile != "" {
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatalf("reading the bearer token: %v", err)
		}
		token = strings.TrimSpace(string(raw))
	}

	store, err := OpenStore(*state)
	if err != nil {
		log.Fatalf("opening the job store: %v", err)
	}
	mirror := NewMirror(*mirrorPath, *remote)
	runner := NewRunner(store, mirror, *nixpkgs, *kernelBuild, *kernelTest, *capacity)
	runner.Start()

	// A rig reboots for its nightly upgrade, and a build that was running is
	// not a build anybody cancelled. Nix will have cached whatever finished.
	for _, id := range store.Requeue() {
		log.Printf("job %s was running when the daemon stopped; requeueing", id)
	}
	for _, id := range store.Pending() {
		runner.Submit(id)
	}

	srv := &server{store: store, runner: runner, mirror: mirror}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs", srv.submit)
	mux.HandleFunc("GET /v1/jobs", srv.list)
	mux.HandleFunc("GET /v1/jobs/{id}", srv.get)
	mux.HandleFunc("DELETE /v1/jobs/{id}", srv.delete)
	mux.HandleFunc("GET /v1/jobs/{id}/log", srv.logs)
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts", srv.manifest)
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts/{path...}", srv.artifact)
	mux.HandleFunc("GET /v1/jobs/{id}/bundle.tar.gz", srv.bundle)
	mux.HandleFunc("GET /v1/status", srv.status)

	ln, err := listener(*listen)
	if err != nil {
		log.Fatalf("listening: %v", err)
	}

	server := &http.Server{
		Handler:           authenticated(token, mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Deliberately no WriteTimeout: following a build's log is a response
		// that stays open for as long as the build runs, which is half an hour.
	}

	log.Printf("serving build jobs on %s", ln.Addr())
	if err := server.Serve(ln); err != nil {
		log.Fatalf("serving: %v", err)
	}
}

// authenticated wraps the mux in a bearer check when a token is configured. On
// the mesh alone this is a pass-through, which is the expected deployment.
func authenticated(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// listener prefers the socket systemd passed in, so that a client's first
// connection is queued rather than refused while the daemon is still starting.
// The protocol is three environment variables and a file descriptor at 3.
func listener(addr string) (net.Listener, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1" {
		const fd = 3

		name := os.Getenv("LISTEN_FDNAMES")
		if name == "" {
			name = "systemd"
		}

		f := os.NewFile(fd, name)
		defer f.Close()

		return net.FileListener(f)
	}

	return net.Listen("tcp", addr)
}

type server struct {
	store  *Store
	runner *Runner
	mirror *Mirror
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	var spec Spec
	// Bounded: a patch is a diff, not a disk image.
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, "malformed job: "+err.Error())
		return
	}
	if err := spec.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolved now rather than when the job reaches the front of the queue.
	// Naming a job that does not exist, or one whose kernel never compiled, is
	// a mistake a caller can fix, and telling them half an hour later — after
	// the job has queued behind somebody's kernel build — helps nobody.
	//
	// It is checked again when the job runs, because the referenced job can be
	// deleted in between.
	if spec.From != "" {
		if _, err := resolveFrom(s.store, spec.From); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}

	job, err := s.store.Create(spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.runner.Submit(job.ID)

	w.Header().Set("Location", "/v1/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.store.List()})
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	job, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *server) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.Get(id); !ok {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}

	// Cancelling a running job leaves the record in place so the caller can
	// see it was cancelled; deleting it is a second, explicit call.
	if s.runner.Cancel(id) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
		return
	}

	if err := s.store.Delete(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// logs streams the build log. ?offset= resumes from a byte position, so a
// client whose wifi dropped mid-build can pick up where it stopped rather than
// re-reading half an hour of output. ?follow=1 keeps the response open until
// the job reaches a terminal state.
func (s *server) logs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.Get(id); !ok {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}

	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	follow := r.URL.Query().Get("follow") == "1"

	file, err := os.Open(s.store.LogPath(id))
	if err != nil {
		// A queued job has no log yet, which is not an error.
		writeError(w, http.StatusNotFound, "no log yet")
		return
	}
	defer file.Close()

	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			writeError(w, http.StatusBadRequest, "bad offset")
			return
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)

	buf := make([]byte, 32<<10)
	for {
		n, err := file.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return
		}

		if !follow {
			return
		}
		// Check for more only after draining: a job that finished between the
		// last read and this check may still have written its final lines.
		job, ok := s.store.Get(id)
		if !ok || job.State.terminal() {
			// One more pass to pick up anything written just before the job
			// reached its terminal state, then stop.
			if n, _ := file.Read(buf); n > 0 {
				_, _ = w.Write(buf[:n])
			}
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// resultDir resolves a job's output, or reports why there is not one yet.
func (s *server) resultDir(id string) (string, int, string) {
	job, ok := s.store.Get(id)
	if !ok {
		return "", http.StatusNotFound, "no such job"
	}
	if job.StorePath == "" {
		return "", http.StatusConflict, "job has produced no artifacts (state: " + string(job.State) + ")"
	}
	return job.StorePath, 0, ""
}

// manifest serves the artifact list with sizes and hashes. It comes straight
// out of the build's own report, which computed those hashes inside the
// sandbox — so a client can verify what it downloads against a manifest the
// build itself produced.
func (s *server) manifest(w http.ResponseWriter, r *http.Request) {
	dir, code, msg := s.resultDir(r.PathValue("id"))
	if code != 0 {
		writeError(w, code, msg)
		return
	}

	raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		writeError(w, http.StatusNotFound, "this build produced no report")
		return
	}
	var report struct {
		Artifacts map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		writeError(w, http.StatusInternalServerError, "unreadable report: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": report.Artifacts})
}

func (s *server) artifact(w http.ResponseWriter, r *http.Request) {
	dir, code, msg := s.resultDir(r.PathValue("id"))
	if code != 0 {
		writeError(w, code, msg)
		return
	}

	// os.OpenRoot confines every lookup below to the result directory, so a
	// path with .. in it cannot reach the rest of the store.
	root, err := os.OpenRoot(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer root.Close()

	file, err := root.Open(r.PathValue("path"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such artifact")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "no such artifact")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	if _, err := io.Copy(w, file); err != nil {
		log.Printf("serving artifact: %v", err)
	}
}

// bundle serves the whole output as one stream, which is what a client that
// wants a walkable build tree on its own disk should fetch. Consumers need
// more than a handful of named files — resolving a module's dependencies means
// globbing for .ko files and reading modules.dep alongside them — so the tar
// preserves the layout rather than flattening it.
func (s *server) bundle(w http.ResponseWriter, r *http.Request) {
	dir, code, msg := s.resultDir(r.PathValue("id"))
	if code != 0 {
		writeError(w, code, msg)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}

		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = rel
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tw, file)
		return err
	})
	if err != nil {
		// Too late for a status code; the client will see a truncated archive
		// and gzip will not check out.
		log.Printf("building bundle for job %s: %v", r.PathValue("id"), err)
	}
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, collectStatus(s.runner, s.mirror))
}
