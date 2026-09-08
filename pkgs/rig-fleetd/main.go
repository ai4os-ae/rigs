// rig-fleetd is the front door to the rigs.
//
// Every rig runs rig-buildd and can be talked to directly over the mesh. This
// puts one endpoint in front of all of them: it decides which rig a job should
// run on, forwards it, and proxies everything afterwards — status, logs and
// artifacts — so a client sees a fleet rather than a list of machines.
//
// Unlike the rigs, this is meant to be reachable from outside the mesh, behind
// a reverse proxy. So every request needs an API key, and the fleet itself is
// never exposed: a client cannot reach a rig through here except by way of a
// job it owns.
//
// It holds no job state. A job id is "<rig>.<id-on-that-rig>", which is enough
// to route every later request, so there is nothing to keep consistent and
// nothing lost across a restart. The one thing it does remember is a placement
// hint, and losing that costs a slow build and nothing else.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9100", "address to serve on, when not socket-activated")
	membersPath := flag.String("members", "", "JSON file listing the rigs: [{\"name\":…,\"url\":…}] (required)")
	apiKeysPath := flag.String("api-keys", "", "file of API keys, one per line (required)")
	state := flag.String("state", "/var/lib/rig/fleetd", "directory for the placement hint file")
	poll := flag.Duration("poll", 10*time.Second, "how often to ask each rig how it is doing")
	flag.Parse()

	log.SetFlags(0) // The journal already stamps every line.

	if *membersPath == "" || *apiKeysPath == "" {
		log.Fatal("-members and -api-keys are both required")
	}

	members, err := LoadMembers(*membersPath)
	if err != nil {
		log.Fatalf("reading the fleet: %v", err)
	}

	keys, err := loadAPIKeys(*apiKeysPath)
	if err != nil {
		log.Fatalf("reading API keys: %v", err)
	}

	if err := os.MkdirAll(*state, 0o750); err != nil {
		log.Fatalf("creating the state directory: %v", err)
	}

	fleet := NewFleet(members, LoadAffinity(filepath.Join(*state, "affinity.json")))
	go fleet.Poll(context.Background(), *poll)

	srv := &server{fleet: fleet}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs", srv.submit)
	mux.HandleFunc("GET /v1/jobs", srv.list)
	mux.HandleFunc("GET /v1/jobs/{id}", srv.get)
	mux.HandleFunc("DELETE /v1/jobs/{id}", srv.forward)
	mux.HandleFunc("GET /v1/jobs/{id}/log", srv.forward)
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts", srv.forward)
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts/{path...}", srv.forward)
	mux.HandleFunc("GET /v1/jobs/{id}/bundle.tar.gz", srv.forward)
	mux.HandleFunc("GET /v1/rigs", srv.rigs)
	mux.HandleFunc("GET /v1/status", srv.status)

	ln, err := listener(*listen)
	if err != nil {
		log.Fatalf("listening: %v", err)
	}

	server := &http.Server{
		Handler:           authenticated(keys, mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: following a build log holds a response open for as
		// long as the build runs, and a bundle is tens of megabytes over a
		// link that may be slow.
	}

	log.Printf("serving %d rigs on %s", len(members), ln.Addr())
	if err := server.Serve(ln); err != nil {
		log.Fatalf("serving: %v", err)
	}
}

// loadAPIKeys reads one key per line. Blank lines and # comments are skipped so
// the file can say who each key belongs to, which is the difference between
// being able to revoke one and having to rotate all of them.
func loadAPIKeys(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var keys [][]byte
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, []byte(line))
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s contains no keys", path)
	}
	return keys, nil
}

// authenticated requires a bearer key on every request. This service is
// intended to be published, so there is no unauthenticated path through it —
// not even the status endpoint, which would otherwise enumerate the fleet.
func authenticated(keys [][]byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))

		// Every key is compared, and in constant time, so that neither the
		// number of configured keys nor how far down the list a key sits is
		// observable in the response time.
		var ok int
		for _, key := range keys {
			ok |= subtle.ConstantTimeCompare(presented, key)
		}
		if ok != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

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
	fleet *Fleet
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

// submitRequest is the fleet's job envelope: a rig's job, plus where it should
// run. Everything else is passed through untouched, so a field added to
// rig-buildd needs no change here.
type submitRequest struct {
	// Run it on this rig by name. Empty lets the fleet choose.
	Rig string `json:"rig,omitempty"`
	// Run it on a rig of this system, "x86_64-linux". Empty is derived from
	// arch, and failing that the fleet's default.
	System string `json:"system,omitempty"`

	Kernel struct {
		Commit string `json:"commit"`
	} `json:"kernel"`
	Config struct {
		Base string            `json:"base"`
		Set  map[string]string `json:"set"`
	} `json:"config"`
	Arch string `json:"arch"`
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request: "+err.Error())
		return
	}

	var req submitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed job: "+err.Error())
		return
	}

	system := req.System
	if system == "" {
		arch := req.Arch
		if arch == "" {
			arch = "x86_64"
		}
		system = arch + "-linux"
	}

	base := req.Config.Base
	if base == "" {
		base = "defconfig"
	}
	key := affinityKey(req.Kernel.Commit, base, req.Arch, req.Config.Set)

	member, err := s.fleet.Pick(system, req.Rig, key)
	if err != nil {
		// The fleet is reachable but cannot take this job: that is a
		// conflict, not a malformed request.
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Forwarded as received. rig-buildd ignores the fields it does not know,
	// so `rig` and `system` need no stripping.
	upstream, err := s.post(r.Context(), member, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("rig %s: %v", member.Name, err))
		return
	}

	if id, ok := upstream["id"].(string); ok {
		upstream["id"] = member.Name + "." + id
		// Only once the rig has actually accepted it: a hint pointing at a rig
		// that refused the job is worse than no hint.
		s.fleet.affinity.Set(key, member.Name)
	}
	upstream["rig"] = member.Name

	w.Header().Set("Location", "/v1/jobs/"+fmt.Sprint(upstream["id"]))
	writeJSON(w, http.StatusAccepted, upstream)
}

func (s *server) post(ctx context.Context, member Member, body []byte) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, member.URL+"/v1/jobs", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("unreadable reply: %w", err)
	}
	if resp.StatusCode >= 300 {
		if msg, ok := decoded["error"].(string); ok {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	return decoded, nil
}

// list asks every reachable rig at once and merges the answers, newest first.
// A rig that fails to answer is skipped rather than failing the whole listing:
// a fleet is expected to have machines that come and go.
func (s *server) list(w http.ResponseWriter, r *http.Request) {
	type reply struct {
		Jobs []map[string]any `json:"jobs"`
	}

	var (
		mu   sync.Mutex
		all  []map[string]any
		wg   sync.WaitGroup
		down []string
	)

	for _, status := range s.fleet.Status() {
		if !status.Up {
			down = append(down, status.Name)
			continue
		}
		wg.Add(1)
		go func(m Member) {
			defer wg.Done()

			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, m.URL+"/v1/jobs", nil)
			if err != nil {
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			var decoded reply
			if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
				return
			}

			mu.Lock()
			defer mu.Unlock()
			for _, job := range decoded.Jobs {
				if id, ok := job["id"].(string); ok {
					job["id"] = m.Name + "." + id
				}
				job["rig"] = m.Name
				all = append(all, job)
			}
		}(status.Member)
	}
	wg.Wait()

	// By submission time rather than by id: an id now begins with a rig name,
	// so sorting on it would group by machine instead of by when.
	sort.Slice(all, func(i, j int) bool {
		return fmt.Sprint(all[i]["submitted_at"]) > fmt.Sprint(all[j]["submitted_at"])
	})

	writeJSON(w, http.StatusOK, map[string]any{"jobs": all, "unreachable": down})
}

// get fetches one job and rewrites its id back into fleet form.
//
// Not proxied, unlike the endpoints below, precisely because of that id: a rig
// answers with its own, and handing that back would mean POST and GET
// disagreeing about what a job is called. Everything a client reads here can
// be fed straight back to this service.
func (s *server) get(w http.ResponseWriter, r *http.Request) {
	fleetID := r.PathValue("id")
	rig, id, ok := strings.Cut(fleetID, ".")
	if !ok {
		writeError(w, http.StatusBadRequest, "job id should be <rig>.<id>")
		return
	}

	member, found := s.fleet.Lookup(rig)
	if !found {
		writeError(w, http.StatusNotFound, "no rig named "+rig+" in this fleet")
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, member.URL+"/v1/jobs/"+id, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("rig %s: %v", rig, err))
		return
	}
	defer resp.Body.Close()

	var job map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&job); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("rig %s: unreadable reply: %v", rig, err))
		return
	}

	if _, present := job["id"]; present {
		job["id"] = fleetID
		job["rig"] = rig
	}
	writeJSON(w, resp.StatusCode, job)
}

// forward proxies everything about an existing job to the rig running it,
// including artifacts and bundles. Streamed, never buffered: a bundle is tens
// of megabytes and following a log is an open response for the length of a
// build.
func (s *server) forward(w http.ResponseWriter, r *http.Request) {
	rig, id, ok := strings.Cut(r.PathValue("id"), ".")
	if !ok {
		writeError(w, http.StatusBadRequest, "job id should be <rig>.<id>")
		return
	}

	member, found := s.fleet.Lookup(rig)
	if !found {
		writeError(w, http.StatusNotFound, "no rig named "+rig+" in this fleet")
		return
	}

	target, err := url.Parse(member.URL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Rebuild the path against the rig's own id. Everything after the id is
	// kept, so /log, /artifacts/... and /bundle.tar.gz all route without this
	// needing to know what they are.
	suffix := strings.TrimPrefix(r.URL.Path, "/v1/jobs/"+r.PathValue("id"))
	path := "/v1/jobs/" + id + suffix

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = path
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = target.Host
			// The client's key authenticates it to this service and says
			// nothing to a rig. Forwarding it would leak it onto the mesh.
			pr.Out.Header.Del("Authorization")
		},
		// Flush as it arrives: without this, following a log would deliver
		// nothing until the build finished.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("rig %s: %v", rig, err))
		},
	}
	proxy.ServeHTTP(w, r)
}

func (s *server) rigs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"rigs": s.fleet.Status()})
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()

	var up, queued int
	for _, status := range s.fleet.Status() {
		if status.Up {
			up++
			queued += status.pending()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hostname":  hostname,
		"time":      time.Now().UTC(),
		"rigs":      len(s.fleet.members),
		"rigs_up":   up,
		"jobs_open": queued,
	})
}
