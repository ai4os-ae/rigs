package main

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Text is the wording on the page, all of it from flags and none of it read
// from the machine. It is fixed for the life of the process.
type Text struct {
	Heading    string `json:"heading"`
	Subheading string `json:"subheading"`
	Warning    string `json:"warning"`
	Link       string `json:"link"`
	HasQR      bool   `json:"qr"`

	// The link without its scheme: what goes under the QR code, where
	// "https://" is eight characters of nothing anybody needs to read.
	LinkLabel string `json:"link_label"`

	// How long the page waits before the screensaver takes over, and how long
	// it runs for. In milliseconds because that is what the page's own timers
	// are counted in.
	IdleMS        int64 `json:"idle_ms"`
	ScreensaverMS int64 `json:"screensaver_ms"`
}

// Status is everything the page shows. It is also the shape of /status.json,
// so anything added here appears in both without further work.
type Status struct {
	Text

	Hostname   string    `json:"hostname"`
	Uptime     string    `json:"uptime"`
	UptimeSecs float64   `json:"uptime_seconds"`
	BootedAt   string    `json:"booted_at"`
	Load       string    `json:"load"`
	Addresses  []Address `json:"addresses"`
	Time       string    `json:"time"`
}

// Address is one usable address on one interface. Loopback and link-local are
// left out: the point of putting this on screen is to be able to reach the rig
// from somewhere else in the building.
type Address struct {
	Interface string `json:"interface"`
	IP        string `json:"ip"`
}

func (a Address) String() string { return a.Interface + "  " + a.IP }

// collect gathers the current state. Everything is read fresh on each request
// — the page polls, and these are cheap reads of /proc and a netlink dump.
func collect(now time.Time, text Text) Status {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	if text.Subheading == "" {
		text.Subheading = hostname
	}

	text.LinkLabel = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(text.Link, "https://"), "http://"), "/")

	s := Status{
		Text:      text,
		Hostname:  hostname,
		Load:      loadAverage(),
		Addresses: addresses(),
		Time:      now.Format("Mon 2 Jan 15:04"),
		Uptime:    "unknown",
	}

	if up, err := uptime(); err == nil {
		s.UptimeSecs = up.Seconds()
		s.Uptime = formatDuration(up)
		s.BootedAt = now.Add(-up).Format("Mon 2 Jan 15:04")
	}

	return s
}

// uptime reads /proc/uptime rather than shelling out to `uptime`, whose output
// format is a moving target and which would drag a coreutils dependency onto
// the page's critical path.
func uptime() (time.Duration, error) {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}

	// "350735.47 234388.90" — seconds up, then seconds idle summed over cores.
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, fmt.Errorf("uptime: %q is not in the expected format", raw)
	}

	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("uptime: %w", err)
	}

	return time.Duration(secs * float64(time.Second)), nil
}

func loadAverage() string {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unknown"
	}

	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return "unknown"
	}

	// 1, 5 and 15 minute averages; the rest of the line is scheduling detail
	// nobody reads from across a room.
	return strings.Join(fields[:3], " ")
}

func addresses() []Address {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var found []Address
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			prefix, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}

			ip := prefix.IP
			// Link-local addresses are not reachable from anywhere useful, and
			// on a machine with wifi up there is always an fe80:: to drown out
			// the address somebody actually wants to read.
			if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}

			found = append(found, Address{Interface: iface.Name, IP: ip.String()})
		}
	}

	// IPv4 first: it is the one a person is going to type.
	sort.SliceStable(found, func(i, j int) bool {
		return (net.ParseIP(found[i].IP).To4() != nil) && (net.ParseIP(found[j].IP).To4() == nil)
	})

	return found
}

// formatDuration renders a duration the way somebody reading a screen wants it:
// coarse, and without units that are always zero.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	var parts []string
	if days > 0 {
		parts = append(parts, strconv.Itoa(days)+"d")
	}
	if hours > 0 || days > 0 {
		parts = append(parts, strconv.Itoa(hours)+"h")
	}
	parts = append(parts, strconv.Itoa(minutes)+"m")

	return strings.Join(parts, " ")
}
