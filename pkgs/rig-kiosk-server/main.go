// rig-kiosk-server draws the page on an AI4OS rig's own screen: what the
// machine is, where it is on the network, and how long it has been up.
//
// It listens on loopback and is read by the Chromium kiosk session on the same
// machine. Nothing here is authenticated, so what it reports is deliberately
// limited to what anybody standing in front of the screen can already see.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

//go:embed page.html
var assets embed.FS

var page = template.Must(template.ParseFS(assets, "page.html"))

// Path to the QR code drawn in the corner. The code itself is rendered by
// qrencode at build time rather than by an encoder in here — this program has
// no dependencies and a QR encoder is not the thing to break that for — so the
// build injects the path with -ldflags -X. Empty in a plain `go build`, which
// simply means no QR.
var defaultQR string

func main() {
	listen := flag.String("listen", "127.0.0.1:8000", "address to serve on, when not socket-activated")
	heading := flag.String("heading", "AI4OS Test Rig", "large line on the page")
	subheading := flag.String("subheading", "", "smaller line under it (default: this machine's hostname)")
	warning := flag.String("warning", "Do not turn off or unplug this device", "red line along the bottom; empty to leave it off")
	link := flag.String("link", "https://github.com/ai4os-ae", "address the QR code points at, captioned under it")
	qrPath := flag.String("qr", defaultQR, "SVG of the QR code to display; empty to leave it off")
	idle := flag.Duration("idle", 10*time.Minute, "how long without input before the screensaver takes over")
	screensaver := flag.Duration("screensaver", time.Minute, "how long the screensaver runs; zero to leave it off")
	flag.Parse()

	log.SetFlags(0) // The journal already stamps every line.

	text := Text{
		Heading:       *heading,
		Subheading:    *subheading,
		Warning:       *warning,
		Link:          *link,
		IdleMS:        idle.Milliseconds(),
		ScreensaverMS: screensaver.Milliseconds(),
	}

	// Read once at startup: it is a store path, so it cannot change under us,
	// and a QR that failed to load is not a reason to withhold the rig's
	// address and uptime from the screen.
	qr, err := os.ReadFile(*qrPath)
	switch {
	case *qrPath == "":
	case err != nil:
		log.Printf("no QR code on the page: %v", err)
	default:
		text.HasQR = true
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		// The page polls, so it must not be held in a cache — by Chromium here,
		// or by anything a future url points through.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		if err := page.Execute(w, collect(time.Now(), text)); err != nil {
			// Too late for an error status: the template writes as it renders.
			log.Printf("rendering page: %v", err)
		}
	})

	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(collect(time.Now(), text)); err != nil {
			log.Printf("encoding status: %v", err)
		}
	})

	mux.HandleFunc("GET /qr.svg", func(w http.ResponseWriter, r *http.Request) {
		if !text.HasQR {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "image/svg+xml")
		if _, err := w.Write(qr); err != nil {
			log.Printf("writing QR code: %v", err)
		}
	})

	ln, err := listener(*listen)
	if err != nil {
		log.Fatalf("listening: %v", err)
	}

	server := &http.Server{
		Handler: mux,
		// A kiosk browser on loopback is the only client. These exist so a
		// stuck connection cannot pin the process open forever.
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("serving the kiosk page on %s", ln.Addr())
	if err := server.Serve(ln); err != nil {
		log.Fatalf("serving: %v", err)
	}
}

// listener prefers the socket systemd passed in. That is what makes the boot
// ordering exact: systemd binds the port before anything starts, so the
// browser's first connection is queued rather than refused, and the kiosk never
// comes up showing a connection-error page.
//
// The protocol is three environment variables and a file descriptor at 3; it is
// small enough to implement here and not worth a dependency.
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
