package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The policy a rig would hand these, sized so the arithmetic in sizeVMs is
// predictable rather than dependent on the machine running the tests.
func testLimits() Limits {
	return Limits{
		FuzzEnabled:     true,
		MaxFuzzDuration: 24 * 3600,
		AllowedDevices:  []string{"e1000", "virtio-net-pci"},
		VMCPU:           4,
		VMMemoryMB:      3072,
		HostReserveMB:   24576,
		HostReserveCPU:  2,
	}
}

func fuzzJob(spec *FuzzSpec) *Spec {
	return &Spec{
		Kind:   KindFuzz,
		Kernel: KernelSpec{Commit: strings.Repeat("a", 40)},
		Fuzz:   spec,
	}
}

func area(name string, weight float64, files ...string) FocusSpec {
	return FocusSpec{Name: name, Files: files, Weight: weight}
}

// THE MOST IMPORTANT TEST IN THIS FILE.
//
// syzkaller treats a non-empty focus_areas list with no empty-filter entry as
// "ignore everything outside these filters" rather than "prefer them", and
// does so silently. If this regresses, every directed run quietly stops
// producing general coverage and nothing about the job looks wrong.
func TestRenderFocusAreasAppendsCatchAll(t *testing.T) {
	spec := &FuzzSpec{Focus: []FocusSpec{area("e1000", 10, "drivers/net/ethernet/intel/e1000/")}}

	areas := renderFocusAreas(spec)

	if len(areas) != 2 {
		t.Fatalf("expected the focus area plus a catch-all, got %d areas", len(areas))
	}
	last := areas[len(areas)-1]
	if last.Filter != nil {
		t.Errorf("the catch-all must have no filter, got %+v", last.Filter)
	}
	if last.Weight != 1.0 {
		t.Errorf("the catch-all should default to weight 1.0, got %g", last.Weight)
	}
}

func TestRenderFocusAreasHonoursBackgroundWeight(t *testing.T) {
	zero := 0.0
	spec := &FuzzSpec{
		Focus:            []FocusSpec{area("net", 5, "net/")},
		BackgroundWeight: &zero,
	}

	areas := renderFocusAreas(spec)

	// Zero is a legitimate request — "genuinely only this" — but it has to be
	// asked for rather than arrived at by forgetting the entry.
	if got := areas[len(areas)-1].Weight; got != 0 {
		t.Errorf("background_weight of 0 should reach the config, got %g", got)
	}
}

// A general run must carry no focus_areas at all. A list holding only the
// catch-all is not the same thing as no list.
func TestRenderFocusAreasEmptyWhenUndirected(t *testing.T) {
	if areas := renderFocusAreas(&FuzzSpec{}); areas != nil {
		t.Errorf("an undirected run should have no focus_areas, got %+v", areas)
	}
}

// A caller must not be able to write the catch-all themselves, because the
// weighting then depends on whether they also remembered the real areas.
func TestValidateRejectsUnfilteredArea(t *testing.T) {
	spec := fuzzJob(&FuzzSpec{Focus: []FocusSpec{{Name: "everything", Weight: 1}}})

	err := spec.validate(testLimits())
	if err == nil {
		t.Fatal("an area with no filters should be rejected")
	}
	if !strings.Contains(err.Error(), "background_weight") {
		t.Errorf("the error should point at background_weight, got: %v", err)
	}
}

func TestValidateFocus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		focus []FocusSpec
		want  string
	}{
		{"duplicate names", []FocusSpec{area("a", 1, "net/"), area("a", 2, "fs/")}, "two areas named"},
		{"zero weight", []FocusSpec{area("a", 0, "net/")}, "weight must be"},
		{"negative weight", []FocusSpec{area("a", -1, "net/")}, "weight must be"},
		{"absurd weight", []FocusSpec{area("a", 1e9, "net/")}, "weight must be"},
		{"bad name", []FocusSpec{area("Not A Name", 1, "net/")}, "name must be"},
		{"filter with a shell fragment", []FocusSpec{area("a", 1, "net/;rm -rf /")}, "not a kernel path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fuzzJob(&FuzzSpec{Focus: tc.focus}).validate(testLimits())
			if err == nil {
				t.Fatalf("expected a rejection, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected an error mentioning %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestValidateAppliesDefaults(t *testing.T) {
	spec := fuzzJob(nil)
	spec.Fuzz = nil

	if err := spec.validate(testLimits()); err != nil {
		t.Fatalf("a fuzz job with no instructions is a general run, not an error: %v", err)
	}

	switch {
	case spec.Fuzz == nil:
		t.Fatal("validate should have filled in a default FuzzSpec")
	case spec.Fuzz.DurationSeconds != defaultFuzzDuration:
		t.Errorf("duration default = %d, want %d", spec.Fuzz.DurationSeconds, defaultFuzzDuration)
	case spec.Fuzz.Procs != defaultFuzzProcs:
		t.Errorf("procs default = %d, want %d", spec.Fuzz.Procs, defaultFuzzProcs)
	case spec.Fuzz.Corpus != "shared":
		t.Errorf("corpus default = %q, want shared", spec.Fuzz.Corpus)
	}
}

func TestValidateDuration(t *testing.T) {
	lim := testLimits()
	lim.MaxFuzzDuration = 3600

	if err := fuzzJob(&FuzzSpec{DurationSeconds: 7200}).validate(lim); err == nil {
		t.Error("a run longer than the rig allows should be rejected, not silently clamped")
	}
	if err := fuzzJob(&FuzzSpec{DurationSeconds: 1800}).validate(lim); err != nil {
		t.Errorf("a run inside the limit should be accepted: %v", err)
	}
}

func TestValidateDevices(t *testing.T) {
	for _, tc := range []struct {
		name   string
		device FuzzDevice
		want   string
	}{
		{"not allowed", FuzzDevice{Type: "virtio-9p-pci", Addr: "0x11"}, "does not offer"},
		{"low pci slot", FuzzDevice{Type: "e1000", Addr: "0x03"}, "0x10 or above"},
		{"bad netdev", FuzzDevice{Type: "e1000", Addr: "0x11", NetDev: "bridge"}, "netdev must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fuzzJob(&FuzzSpec{VM: FuzzVMSpec{Devices: []FuzzDevice{tc.device}}}).validate(testLimits())
			if err == nil {
				t.Fatal("expected a rejection, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected an error mentioning %q, got: %v", tc.want, err)
			}
		})
	}
}

// A device at a low PCI slot is enumerated before syzkaller's own NIC, takes
// eth0, and leaves sshd answering on a network syz-manager cannot reach. Every
// VM is then reported as having failed to boot, which reads as a broken image.
func TestValidateAcceptsHighPCISlots(t *testing.T) {
	for _, addr := range []string{"0x10", "0x11", "0xff"} {
		spec := fuzzJob(&FuzzSpec{VM: FuzzVMSpec{
			Devices: []FuzzDevice{{Type: "e1000", Addr: addr, NetDev: "user-restricted"}},
		}})
		if err := spec.validate(testLimits()); err != nil {
			t.Errorf("addr %s should be accepted: %v", addr, err)
		}
	}
}

// syzkaller seeds qemu_args from its per-architecture defaults and then
// overwrites the field wholesale from the config. Setting it without restating
// -enable-kvm drops the VMs to full emulation — which does not fail, it just
// runs about fifty times slower, and looks like a rig having a bad day.
func TestRenderQemuArgsKeepsKVM(t *testing.T) {
	args := renderQemuArgs([]FuzzDevice{{Type: "e1000", Addr: "0x11", NetDev: "user-restricted"}})

	for _, want := range []string{
		"-enable-kvm",
		"-cpu host,migratable=off",
		"-device e1000,addr=0x11,netdev=vf0",
		"-netdev user,id=vf0,restrict=on",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("qemu_args is missing %q; got: %s", want, args)
		}
	}
}

// Empty rather than a bare "-enable-kvm ...", so that syzkaller's own defaults
// are left alone entirely when a job asked for nothing.
func TestRenderQemuArgsEmptyWithoutDevices(t *testing.T) {
	if args := renderQemuArgs(nil); args != "" {
		t.Errorf("no devices should mean no qemu_args at all, got %q", args)
	}
}

func TestRenderQemuArgsWithoutNetdev(t *testing.T) {
	args := renderQemuArgs([]FuzzDevice{{Type: "nvme", Addr: "0x12", NetDev: "none"}})

	if strings.Contains(args, "-netdev") {
		t.Errorf("a device with no netdev should get no backend, got: %s", args)
	}
	if !strings.Contains(args, "-device nvme,addr=0x12") {
		t.Errorf("expected the device itself, got: %s", args)
	}
}

// A profile is a starting point, not a constraint: the request wins, and
// anything the request did not mention is inherited.
func TestProfileMerge(t *testing.T) {
	profile := &FuzzProfile{
		Name:   "e1000",
		Procs:  2,
		Focus:  []FocusSpec{area("e1000", 10, "drivers/net/ethernet/intel/e1000/"), area("netdev-core", 3, "net/ethtool/")},
		Config: map[string]string{"CONFIG_E1000": "y"},
		VM:     FuzzVMSpec{Devices: []FuzzDevice{{Type: "e1000", Addr: "0x11", NetDev: "user-restricted"}}},
	}

	spec := &FuzzSpec{Focus: []FocusSpec{area("e1000", 50, "drivers/net/ethernet/intel/e1000/")}}
	spec.merge(profile)

	if spec.Procs != 2 {
		t.Errorf("procs should be inherited from the profile, got %d", spec.Procs)
	}
	if len(spec.VM.Devices) != 1 {
		t.Fatalf("the profile's devices are what make its target reachable; got %d", len(spec.VM.Devices))
	}

	// Two areas, not three: the request's e1000 replaced the profile's rather
	// than being added alongside it.
	if len(spec.Focus) != 2 {
		t.Fatalf("expected the profile's areas with the request's merged in by name, got %d", len(spec.Focus))
	}
	byName := map[string]float64{}
	for _, a := range spec.Focus {
		byName[a.Name] = a.Weight
	}
	if byName["e1000"] != 50 {
		t.Errorf("the request's weight should win, got %g", byName["e1000"])
	}
	if byName["netdev-core"] != 3 {
		t.Errorf("an area the request did not mention should survive, got %g", byName["netdev-core"])
	}
}

// The comments in these files are the most valuable thing in them, and JSON
// has nowhere to put them. syzkaller's own parser strips whole-line # comments
// and so does this.
func TestStripJSONComments(t *testing.T) {
	raw := []byte("{\n  # why this profile exists\n  \"procs\": 2\n}")

	var got map[string]any
	if err := json.Unmarshal(stripJSONComments(raw), &got); err != nil {
		t.Fatalf("commented profile should parse: %v", err)
	}
	if got["procs"] != 2.0 {
		t.Errorf("procs = %v, want 2", got["procs"])
	}
}

// The corpus is keyed on what the fuzzer was aiming at, not on the kernel:
// programs stay useful across rebuilds, which is what makes a bounded run
// something other than a cold start.
func TestCorpusFingerprint(t *testing.T) {
	base := &FuzzSpec{Focus: []FocusSpec{area("e1000", 10, "drivers/net/ethernet/intel/e1000/")}}

	reweighted := &FuzzSpec{Focus: []FocusSpec{area("e1000", 50, "drivers/net/ethernet/intel/e1000/")}}
	if corpusFingerprint(base) != corpusFingerprint(reweighted) {
		t.Error("re-weighting a campaign should keep its corpus; the same code is still the target")
	}

	elsewhere := &FuzzSpec{Focus: []FocusSpec{area("e1000", 10, "drivers/net/ethernet/intel/e1000e/")}}
	if corpusFingerprint(base) == corpusFingerprint(elsewhere) {
		t.Error("pointing the fuzzer at different code should not reuse the old corpus")
	}

	filtered := &FuzzSpec{
		Focus:          base.Focus,
		EnableSyscalls: []string{"ioctl$sock_SIOCETHTOOL"},
	}
	if corpusFingerprint(base) == corpusFingerprint(filtered) {
		t.Error("restricting the syscall set reshapes the corpus and should key differently")
	}
}

// Below this the executor cannot map its data segment against a KASAN kernel:
// the VM reboots forever while the fuzzer reports a healthy pool and a zero
// execution rate.
func TestSizeVMsEnforcesMemoryFloor(t *testing.T) {
	vm := sizeVMs(FuzzVMSpec{MemoryMB: 512}, testLimits())
	if vm.MemoryMB < minFuzzVMMemoryMB {
		t.Errorf("memory floor not applied: got %d, want at least %d", vm.MemoryMB, minFuzzVMMemoryMB)
	}
}

func TestSizeVMsCapsRequest(t *testing.T) {
	// A request for far more VMs than any machine could carry must come back
	// capped, and must say why rather than looking ignored.
	vm := sizeVMs(FuzzVMSpec{Count: maxFuzzVMCount}, testLimits())

	if vm.Count > maxFuzzVMCount || vm.Count < 1 {
		t.Errorf("count = %d, which is outside anything sensible", vm.Count)
	}
	if vm.Reason == "" {
		t.Error("sizeVMs should always explain the count it chose")
	}
}

// A fuzz spec on a build job means somebody believes it is being honoured.
func TestValidateRejectsFuzzSpecOnOtherKinds(t *testing.T) {
	spec := &Spec{
		Kind:   KindBuild,
		Kernel: KernelSpec{Commit: strings.Repeat("a", 40)},
		Fuzz:   &FuzzSpec{},
	}
	if err := spec.validate(testLimits()); err == nil {
		t.Error("a build job carrying a fuzz spec should be rejected")
	}
}

func TestValidateRejectsFuzzWhenDisabled(t *testing.T) {
	lim := testLimits()
	lim.FuzzEnabled = false

	err := fuzzJob(&FuzzSpec{}).validate(lim)
	if err == nil {
		t.Fatal("a rig that does not run fuzz jobs should say so")
	}
	if !strings.Contains(err.Error(), "kinds") {
		t.Errorf("the error should point the caller at /v1/status, got: %v", err)
	}
}

// The progress line is parsed field by field so a syzkaller upgrade costs a
// counter rather than the whole of progress reporting.
func TestFuzzProgressUpdate(t *testing.T) {
	line := "2026/09/10 18:09:49 candidates=0 corpus=1078 corpus [e1000]=7 " +
		"coverage=21454 exec total=24067 (297/sec) pending=0 reproducing=2"

	var p FuzzProgress
	if !p.update(line) {
		t.Fatal("a statistics line should be recognised")
	}

	// corpus= and "corpus [e1000]=" must not be confused for each other: the
	// per-area count is the signal that a directed run is working at all.
	if p.Corpus != 1078 {
		t.Errorf("corpus = %d, want 1078", p.Corpus)
	}
	if p.FocusCorpus["e1000"] != 7 {
		t.Errorf("focus corpus for e1000 = %d, want 7", p.FocusCorpus["e1000"])
	}
	if p.Coverage != 21454 {
		t.Errorf("coverage = %d, want 21454", p.Coverage)
	}
	if p.Executed != 24067 {
		t.Errorf("executed = %d, want 24067", p.Executed)
	}
	if p.ExecPerSec != 297 {
		t.Errorf("exec/sec = %d, want 297", p.ExecPerSec)
	}
	if p.Reproducing != 2 {
		t.Errorf("reproducing = %d, want 2", p.Reproducing)
	}
}

func TestFuzzProgressIgnoresOtherLines(t *testing.T) {
	var p FuzzProgress
	for _, line := range []string{
		"2026/09/10 18:08:42 coverage filter: drivers/net/: [drivers/net/foo.c]",
		"Coverage                : enabled",
		"",
	} {
		if p.update(line) {
			t.Errorf("%q is not a statistics line", line)
		}
	}
}
