package main

// What a fuzz job asks for, and how it becomes a syzkaller manager config.
//
// The shape of this API is not syzkaller's config with a different spelling.
// Three things are deliberately different, and each of them is a mistake that
// syzkaller's own configuration format makes it easy to make and expensive to
// notice:
//
//   1. A caller cannot write the catch-all focus area, because a caller who
//      forgets it gets a fuzzer that ignores the entire kernel outside their
//      filters rather than one that merely prefers them. See FocusSpec.
//   2. A caller cannot pass qemu arguments, because that is not a fuzzing
//      knob, it is the ability to run QEMU on a rig with arbitrary argv.
//      See FuzzDevice.
//   3. A caller cannot pick a VM count the machine cannot carry, because
//      overcommitting memory produces a fuzzer that looks alive and executes
//      nothing. See sizeVMs.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// FocusSpec directs the fuzzer at a part of the kernel. It is syzkaller's
// `experimental.focus_areas` with one entry made unspellable.
//
// syzkaller treats a non-empty focus_areas list with no empty-filter entry as
// "ignore everything else" rather than "prefer these" — the general coverage
// signal is discarded entirely, silently, and the run still looks healthy. The
// fix upstream documents is to append `{"weight": 1.0}` yourself and remember
// to do it every time.
//
// So this type has no way to express the catch-all: rig-buildd appends it from
// FuzzSpec.BackgroundWeight, always. A caller who genuinely wants nothing but
// their filters sets background_weight to 0 and has said so on purpose.
type FocusSpec struct {
	// Shown in syz-manager's per-area statistics, which is the only way to
	// tell whether a focus area is actually being reached.
	Name string `json:"name"`

	// Regular expressions over kernel source paths, as syzkaller matches them:
	// "drivers/net/ethernet/intel/e1000/", "^net/core/tcp.c$", "tcp".
	Files []string `json:"files,omitempty"`
	// The same, over function names.
	Functions []string `json:"functions,omitempty"`

	// Relative to background_weight. A weight of 10 against the default
	// background of 1 means programs covering this area are picked at least
	// ten times as often as ones covering only the rest of the kernel.
	Weight float64 `json:"weight"`
}

// FuzzDevice is an emulated device to give each VM, from a rig-configured
// allow-list.
//
// This exists instead of exposing syzkaller's `vm.qemu_args`, which is the
// obvious thing and is a privilege escalation: a string handed to QEMU's argv
// turns "submit a patch to be fuzzed" into "read this rig's disk", via
// -fsdev local,path=/ and a dozen other options. Nothing about a focus profile
// needs that, and the profiles that do need a device (see
// nix/kernel-fuzz/focus/e1000.json, whose whole point is parking a second NIC
// where syzkaller's NIC-VF probe will find it) need only these three fields.
//
// Rendering them ourselves also fixes a trap. syzkaller seeds qemu_args from
// its per-architecture defaults and then *overwrites* the field from the
// config, so setting it at all drops "-enable-kvm -cpu host,migratable=off" —
// and the VMs silently fall back to full emulation, which does not fail, it
// just fuzzes about fifty times slower. renderQemuArgs puts them back.
type FuzzDevice struct {
	// A QEMU device type, checked against the rig's allow-list.
	Type string `json:"type"`

	// PCI address. Constrained to 0x10 and above, and that is load-bearing:
	// syzkaller appends its own NIC after these, so its device lands on a low
	// slot and is enumerated first, which is what makes it eth0 and what makes
	// the guest's static addressing land on the right interface. A device down
	// at a low address takes eth0 instead, sshd answers on a network
	// syz-manager cannot reach, and every VM in the pool is reported as having
	// failed to boot.
	Addr string `json:"addr,omitempty"`

	// "user-restricted" gives the device its own isolated user-mode network;
	// "none" (the default) attaches no backend at all.
	NetDev string `json:"netdev,omitempty"`
}

// FuzzVMSpec sizes the VM pool. Every field is optional and every field is
// capped: see sizeVMs.
type FuzzVMSpec struct {
	Count    int          `json:"count,omitempty"`
	CPU      int          `json:"cpu,omitempty"`
	MemoryMB int          `json:"memory_mb,omitempty"`
	Devices  []FuzzDevice `json:"devices,omitempty"`
}

// FuzzSpec is the fuzz half of a job.
type FuzzSpec struct {
	// How long to fuzz for. A run ends on its own: rigs are unattended and
	// shared with build and test jobs, and a run that ends only when somebody
	// remembers it is a rig out of service.
	DurationSeconds int `json:"duration_seconds,omitempty"`

	// A curated profile from nix/kernel-fuzz/focus/, by name. Profiles carry
	// the things that are subtle enough to want reviewing in git rather than
	// retyping into a request — the devices a target needs to be reachable at
	// all, and the configuration symbols that go with them.
	Profile string `json:"profile,omitempty"`

	// Focus areas, merged over the profile's by name.
	Focus []FocusSpec `json:"focus,omitempty"`

	// The weight given to everything the focus areas do not cover. Defaults to
	// 1.0, so focus weights read as multiples of "the rest of the kernel".
	// Zero means genuinely ignore the rest, which is almost never wanted — see
	// the comment on FocusSpec.
	BackgroundWeight *float64 `json:"background_weight,omitempty"`

	// Restrict the fuzzer to, or away from, particular syscalls. Passed
	// through to syzkaller, which is the only thing that can validate them:
	// the set depends on the descriptions the pinned syzkaller was built with.
	EnableSyscalls  []string `json:"enable_syscalls,omitempty"`
	DisableSyscalls []string `json:"disable_syscalls,omitempty"`

	// Parallel test processes inside each VM. A profile may lower it: the
	// NIC-VF device is one per VM and the grab is racy, so fewer procs means a
	// larger share of executions have it.
	Procs int `json:"procs,omitempty"`

	// Whether syz-manager should try to reduce each crash to a reproducer.
	// On by default: a crash without a reproducer is a report a maintainer
	// cannot act on.
	Reproduce *bool `json:"reproduce,omitempty"`

	// "shared" (the default) seeds this run from the rig's corpus for this
	// focus fingerprint and writes back to it, so a bounded run is not a cold
	// start. "cold" does neither. A job id seeds from that job and writes
	// back to the shared corpus.
	Corpus string `json:"corpus,omitempty"`

	// Modules to load in the guest before sshd starts, for a driver under test
	// that was configured =m. A driver the fuzzer cannot reach is a focus area
	// that reports no coverage for a reason that has nothing to do with
	// weighting.
	Modules []string `json:"modules,omitempty"`

	VM FuzzVMSpec `json:"vm,omitzero"`
}

// FuzzProgress is what a run has done so far, lifted out of syz-manager's
// periodic statistics line. Surfaced on GET /v1/jobs/{id} because a four-hour
// job that reports nothing until it finishes is a job nobody trusts.
//
// The line it comes from looks like this:
//
//	candidates=0 corpus=1904 corpus [e1000]=0 coverage=28810 \
//	  exec total=43646 (255/sec) pending=0 reproducing=0
type FuzzProgress struct {
	// Programs in the corpus, and basic blocks covered. Both should climb; a
	// coverage figure that has stopped climbing is a campaign that has found
	// what it is going to find.
	Corpus   int `json:"corpus"`
	Coverage int `json:"coverage"`

	Executed   int `json:"executed"`
	ExecPerSec int `json:"exec_per_sec"`

	Candidates  int `json:"candidates"`
	Reproducing int `json:"reproducing"`

	// FOR A DIRECTED RUN THIS IS THE FIELD THAT MATTERS. syz-manager reports
	// corpus size per focus area, and an area sitting at zero while the total
	// climbs means the fuzzer cannot reach that code at all — which is a
	// different problem from weighting it too low, and is not otherwise
	// visible. nix/kernel-fuzz/focus/e1000.json exists because of exactly this
	// failure: the driver is unreachable without an extra device, and a run
	// without one reports healthy totals and zero for the area.
	FocusCorpus map[string]int `json:"focus_corpus,omitempty"`

	// Counted from the workdir rather than the stats line, which does not
	// carry it.
	Crashes int `json:"crashes"`

	ElapsedSeconds int `json:"elapsed_seconds"`
	// Where the run is against its deadline, so a client can show progress
	// without knowing what was asked for.
	DurationSeconds int `json:"duration_seconds"`
}

// Parsed with one regular expression per field rather than by splitting the
// line, so that a syzkaller upgrade that adds, removes or reorders a counter
// costs us that counter and not the whole of the progress reporting.
var (
	statCorpusRE      = regexp.MustCompile(`\bcorpus=(\d+)`)
	statFocusCorpusRE = regexp.MustCompile(`\bcorpus \[([^\]]+)\]=(\d+)`)
	statCoverageRE    = regexp.MustCompile(`\bcoverage=(\d+)`)
	statExecRE        = regexp.MustCompile(`\bexec total=(\d+)`)
	statRateRE        = regexp.MustCompile(`\((\d+)/sec\)`)
	statCandidatesRE  = regexp.MustCompile(`\bcandidates=(\d+)`)
	statReproducingRE = regexp.MustCompile(`\breproducing=(\d+)`)
)

// update folds one line of syz-manager output into the progress counters, and
// reports whether the line was one. Anything that is not a statistics line —
// the feature probe, crash reports, the VM chatter — is left alone.
func (p *FuzzProgress) update(line string) bool {
	if !strings.Contains(line, "exec total=") {
		return false
	}

	first := func(re *regexp.Regexp, into *int) {
		if m := re.FindStringSubmatch(line); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				*into = v
			}
		}
	}

	first(statCorpusRE, &p.Corpus)
	first(statCoverageRE, &p.Coverage)
	first(statExecRE, &p.Executed)
	first(statRateRE, &p.ExecPerSec)
	first(statCandidatesRE, &p.Candidates)
	first(statReproducingRE, &p.Reproducing)

	for _, m := range statFocusCorpusRE.FindAllStringSubmatch(line, -1) {
		if v, err := strconv.Atoi(m[2]); err == nil {
			if p.FocusCorpus == nil {
				p.FocusCorpus = map[string]int{}
			}
			p.FocusCorpus[m[1]] = v
		}
	}

	return true
}

// FuzzProfile is a curated campaign, as shipped in nix/kernel-fuzz/focus/.
type FuzzProfile struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Focus       []FocusSpec       `json:"focus,omitempty"`
	Procs       int               `json:"procs,omitempty"`
	Config      map[string]string `json:"config,omitempty"`
	VM          FuzzVMSpec        `json:"vm,omitzero"`
}

var (
	focusNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// A coverage filter is a regular expression over source paths, and it is
	// put into a JSON document rather than a command line — so this is not
	// about injection, it is about catching a caller's typo before it costs a
	// kernel build. The set is the characters a path-shaped regex needs.
	focusFilterRE = regexp.MustCompile(`^[A-Za-z0-9_./^$+*?()\[\]|\\-]+$`)
	// A QEMU PCI address at slot 0x10 or above. See FuzzDevice.Addr.
	deviceAddrRE = regexp.MustCompile(`^0x[1-9a-f][0-9a-f]$`)
	// A QEMU device type, as `-device help` names them.
	deviceTypeRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	// A syscall as syzkaller names one: `ioctl$sock_SIOCETHTOOL`, `openat`,
	// and the `*` globs its config accepts.
	syscallRE = regexp.MustCompile(`^[a-zA-Z0-9_$*.-]+$`)
	// A profile name, which is also a filename under focus/.
	profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// Bounds. Generous rather than tight, on the same principle as the test
// bounds: the point is that a runaway ends, not that a caller has to justify
// wanting eight processes.
const (
	defaultFuzzDuration = 4 * 3600
	defaultFuzzProcs    = 8
	maxFuzzProcs        = 32
	maxFocusAreas       = 16
	maxFocusFilters     = 32
	maxFocusFilterBytes = 256
	maxFuzzSyscalls     = 512
	maxFuzzDevices      = 4
	maxFocusWeight      = 1000.0

	// Below this a KASAN kernel's syz-executor cannot mmap its data segment at
	// 0x200000000000. The VM dies with "SYZFAIL: mmap of data segment failed",
	// is rebooted, dies again, and the pool spends its life booting. Nothing
	// reports an error: the fuzzer is up, the VM count is right, and the
	// execution rate simply stays at zero. It is the single most confusing way
	// to configure this wrong, which is why it is a floor and not advice.
	minFuzzVMMemoryMB = 3072
	maxFuzzVMMemoryMB = 16384
	maxFuzzVMCPU      = 32
	maxFuzzVMCount    = 64
)

// Limits are the rig's policy, from modules/rig/buildd.nix. Passed in rather
// than read from globals so that validation is testable and so that two rigs
// of different sizes can disagree about them.
type Limits struct {
	// Whether this rig runs fuzz jobs at all.
	FuzzEnabled bool
	// The longest run this rig will accept.
	MaxFuzzDuration int
	// QEMU device types a request may ask for.
	AllowedDevices []string

	// VM sizing defaults and the host's share, which sizeVMs works from.
	VMCPU          int
	VMMemoryMB     int
	HostReserveMB  int
	HostReserveCPU int
}

func (l Limits) deviceAllowed(kind string) bool {
	return slices.Contains(l.AllowedDevices, kind)
}

// validate checks everything a caller can fix. Bounds that depend on the
// machine are applied later, by sizeVMs, because they depend on what else is
// running.
func (f *FuzzSpec) validate(lim Limits) error {
	if !lim.FuzzEnabled {
		return fmt.Errorf("this rig does not run fuzz jobs; check `kinds` in /v1/status")
	}

	if f.DurationSeconds == 0 {
		f.DurationSeconds = defaultFuzzDuration
	}
	if f.DurationSeconds < 60 || f.DurationSeconds > lim.MaxFuzzDuration {
		return fmt.Errorf("fuzz.duration_seconds must be between 60 and %d, got %d",
			lim.MaxFuzzDuration, f.DurationSeconds)
	}

	if f.Profile != "" && !profileNameRE.MatchString(f.Profile) {
		return fmt.Errorf("fuzz.profile must be the name of a profile on this rig, got %q", f.Profile)
	}

	if err := validateFocus(f.Focus); err != nil {
		return err
	}

	if f.BackgroundWeight != nil {
		w := *f.BackgroundWeight
		if math.IsNaN(w) || w < 0 || w > maxFocusWeight {
			return fmt.Errorf("fuzz.background_weight must be between 0 and %g, got %g", maxFocusWeight, w)
		}
	}

	for _, list := range [][]string{f.EnableSyscalls, f.DisableSyscalls} {
		if len(list) > maxFuzzSyscalls {
			return fmt.Errorf("at most %d syscalls may be named, got %d", maxFuzzSyscalls, len(list))
		}
		for _, call := range list {
			if !syscallRE.MatchString(call) {
				return fmt.Errorf("%q is not a syscall name syzkaller would recognise", call)
			}
		}
	}

	if f.Procs == 0 {
		f.Procs = defaultFuzzProcs
	}
	if f.Procs < 1 || f.Procs > maxFuzzProcs {
		return fmt.Errorf("fuzz.procs must be between 1 and %d, got %d", maxFuzzProcs, f.Procs)
	}

	for _, module := range f.Modules {
		if !moduleRE.MatchString(module) {
			return fmt.Errorf("fuzz.modules[%q] is not a module name", module)
		}
	}

	switch f.Corpus {
	case "":
		f.Corpus = "shared"
	case "shared", "cold":
	default:
		if !jobIDRE.MatchString(f.Corpus) {
			return fmt.Errorf(`fuzz.corpus must be "shared", "cold" or a job id, got %q`, f.Corpus)
		}
	}

	return f.VM.validate(lim)
}

func validateFocus(areas []FocusSpec) error {
	if len(areas) > maxFocusAreas {
		return fmt.Errorf("at most %d focus areas, got %d", maxFocusAreas, len(areas))
	}

	seen := map[string]bool{}
	for i, area := range areas {
		if !focusNameRE.MatchString(area.Name) {
			return fmt.Errorf("fuzz.focus[%d].name must be a short lowercase name, got %q", i, area.Name)
		}
		if seen[area.Name] {
			return fmt.Errorf("fuzz.focus has two areas named %q; names label their statistics and must differ", area.Name)
		}
		seen[area.Name] = true

		if len(area.Files) == 0 && len(area.Functions) == 0 {
			// An area with no filter is the catch-all, and the catch-all is
			// not the caller's to write — see the comment on FocusSpec.
			return fmt.Errorf("fuzz.focus[%q] has no files or functions; "+
				"an unfiltered area would match the whole kernel, which is what "+
				"background_weight is for", area.Name)
		}

		for _, filters := range [][]string{area.Files, area.Functions} {
			if len(filters) > maxFocusFilters {
				return fmt.Errorf("fuzz.focus[%q] has %d filters; the limit is %d",
					area.Name, len(filters), maxFocusFilters)
			}
			for _, filter := range filters {
				if len(filter) > maxFocusFilterBytes {
					return fmt.Errorf("fuzz.focus[%q] filter %q is %d bytes; the limit is %d",
						area.Name, filter, len(filter), maxFocusFilterBytes)
				}
				if !focusFilterRE.MatchString(filter) {
					return fmt.Errorf("fuzz.focus[%q] filter %q is not a kernel path or regular expression",
						area.Name, filter)
				}
			}
		}

		if math.IsNaN(area.Weight) || area.Weight <= 0 || area.Weight > maxFocusWeight {
			return fmt.Errorf("fuzz.focus[%q].weight must be between 0 and %g, got %g",
				area.Name, maxFocusWeight, area.Weight)
		}
	}
	return nil
}

func (v *FuzzVMSpec) validate(lim Limits) error {
	if v.Count < 0 || v.Count > maxFuzzVMCount {
		return fmt.Errorf("fuzz.vm.count must be between 1 and %d, got %d", maxFuzzVMCount, v.Count)
	}
	if v.CPU < 0 || v.CPU > maxFuzzVMCPU {
		return fmt.Errorf("fuzz.vm.cpu must be between 1 and %d, got %d", maxFuzzVMCPU, v.CPU)
	}
	if v.MemoryMB != 0 && (v.MemoryMB < minFuzzVMMemoryMB || v.MemoryMB > maxFuzzVMMemoryMB) {
		return fmt.Errorf("fuzz.vm.memory_mb must be between %d and %d, got %d; "+
			"below %d syz-executor cannot map its data segment against a KASAN kernel and the VM never fuzzes",
			minFuzzVMMemoryMB, maxFuzzVMMemoryMB, v.MemoryMB, minFuzzVMMemoryMB)
	}

	if len(v.Devices) > maxFuzzDevices {
		return fmt.Errorf("at most %d extra devices, got %d", maxFuzzDevices, len(v.Devices))
	}
	for i := range v.Devices {
		device := &v.Devices[i]

		if !deviceTypeRE.MatchString(device.Type) {
			return fmt.Errorf("fuzz.vm.devices[%d].type is not a QEMU device name, got %q", i, device.Type)
		}
		if !lim.deviceAllowed(device.Type) {
			return fmt.Errorf("this rig does not offer the %q device; it allows %s",
				device.Type, strings.Join(lim.AllowedDevices, ", "))
		}
		if device.Addr != "" && !deviceAddrRE.MatchString(device.Addr) {
			return fmt.Errorf("fuzz.vm.devices[%d].addr must be a PCI address of 0x10 or above, got %q; "+
				"a lower slot is enumerated before syzkaller's own NIC and takes eth0, "+
				"which leaves the VM unreachable", i, device.Addr)
		}

		switch device.NetDev {
		case "", "none":
			device.NetDev = "none"
		case "user-restricted":
		default:
			return fmt.Errorf(`fuzz.vm.devices[%d].netdev must be "none" or "user-restricted", got %q`,
				i, device.NetDev)
		}
	}

	return nil
}

// merge folds a profile's settings in underneath the request's. The request
// always wins, so a profile is a starting point rather than a constraint —
// except that a profile's devices and configuration are what make its target
// reachable at all, and a request that overrode them without meaning to would
// get a campaign that quietly fuzzes nothing.
func (f *FuzzSpec) merge(profile *FuzzProfile) {
	if f.Procs == 0 {
		f.Procs = profile.Procs
	}
	if len(f.VM.Devices) == 0 {
		f.VM.Devices = profile.VM.Devices
	}
	if f.VM.Count == 0 {
		f.VM.Count = profile.VM.Count
	}
	if f.VM.CPU == 0 {
		f.VM.CPU = profile.VM.CPU
	}
	if f.VM.MemoryMB == 0 {
		f.VM.MemoryMB = profile.VM.MemoryMB
	}

	// Focus areas merge by name: an inline area replaces the profile's area of
	// the same name, and anything new is appended. That makes "the e1000
	// profile, but weight the driver harder" a two-line request rather than a
	// copy of the whole profile.
	merged := make([]FocusSpec, 0, len(profile.Focus)+len(f.Focus))
	overridden := map[string]bool{}
	for _, area := range f.Focus {
		overridden[area.Name] = true
	}
	for _, area := range profile.Focus {
		if !overridden[area.Name] {
			merged = append(merged, area)
		}
	}
	f.Focus = append(merged, f.Focus...)
}

// loadProfile reads one of the profiles shipped in nix/kernel-fuzz/focus/.
//
// Whole-line # comments are stripped first. That is not a local invention:
// syzkaller's own configuration parser does the same, and these files carry
// the reasoning for why a campaign is shaped the way it is — which is the most
// valuable thing in them and does not survive a format that has nowhere to put
// it.
func loadProfile(dir, name string) (*FuzzProfile, error) {
	if !profileNameRE.MatchString(name) {
		return nil, fmt.Errorf("%q is not a profile name", name)
	}

	raw, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return nil, fmt.Errorf("no fuzz profile %q on this rig", name)
	}

	var profile FuzzProfile
	if err := json.Unmarshal(stripJSONComments(raw), &profile); err != nil {
		return nil, fmt.Errorf("fuzz profile %q is not readable: %w", name, err)
	}
	profile.Name = name
	return &profile, nil
}

// listProfiles enumerates what this rig ships, for GET /v1/fuzz/profiles. An
// unreadable profile is skipped rather than fatal: the rest are still usable.
func listProfiles(dir string) []*FuzzProfile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []*FuzzProfile
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		if name == entry.Name() {
			continue
		}
		profile, err := loadProfile(dir, name)
		if err != nil {
			continue
		}
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func stripJSONComments(raw []byte) []byte {
	lines := strings.Split(string(raw), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return []byte(strings.Join(kept, "\n"))
}
