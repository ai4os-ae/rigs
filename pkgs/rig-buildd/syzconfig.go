package main

// Rendering a fuzz job into a syzkaller manager configuration.
//
// The generated file is kept in the job directory next to job.nix, and for the
// same reason: a run should be reproducible by hand. `syz-manager -config
// /var/lib/rig/buildd/jobs/<id>/syzkaller.cfg` is the whole of what the daemon
// does with it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The subset of syzkaller's mgrconfig.Config this daemon sets. Deliberately a
// subset: a field that is not here is one syzkaller gets to default, and
// upstream's defaults are better maintained than a copy of them would be.
type managerConfig struct {
	Target string `json:"target"`
	HTTP   string `json:"http"`

	Workdir string `json:"workdir"`

	KernelObj string `json:"kernel_obj"`
	KernelSrc string `json:"kernel_src,omitempty"`
	// Where the kernel was compiled, so that syz-manager can rewrite the paths
	// recorded in vmlinux's DWARF onto the source tree it actually has. See
	// nix/kernel-fuzz/default.nix — kernel-build compiles at a fixed path
	// inside the Nix sandbox, which is exactly what makes this possible.
	// Without it, every coverage and crash report names a file that does not
	// exist on this machine.
	KernelBuildSrc string `json:"kernel_build_src,omitempty"`

	Image     string `json:"image,omitempty"`
	SSHKey    string `json:"sshkey,omitempty"`
	Syzkaller string `json:"syzkaller"`

	Procs   int    `json:"procs"`
	Type    string `json:"type"`
	Sandbox string `json:"sandbox"`

	Cover     bool `json:"cover"`
	Reproduce bool `json:"reproduce"`
	// Keep corpus programs that no longer pass the syscall filters. On,
	// because a shared corpus outlives the focus that produced it — see
	// corpusFingerprint.
	PreserveCorpus bool `json:"preserve_corpus"`

	EnabledSyscalls  []string `json:"enable_syscalls,omitempty"`
	DisabledSyscalls []string `json:"disable_syscalls,omitempty"`

	VM           managerVM           `json:"vm"`
	Experimental managerExperimental `json:"experimental,omitzero"`
}

type managerVM struct {
	Count   int    `json:"count"`
	Kernel  string `json:"kernel"`
	Initrd  string `json:"initrd,omitempty"`
	Cmdline string `json:"cmdline,omitempty"`
	CPU     int    `json:"cpu"`
	Mem     int    `json:"mem"`
	// Left empty unless a job asked for extra devices, because setting it at
	// all discards syzkaller's own per-architecture defaults. See
	// renderQemuArgs.
	QemuArgs string `json:"qemu_args,omitempty"`
}

type managerExperimental struct {
	FocusAreas []managerFocusArea `json:"focus_areas,omitempty"`
}

type managerFocusArea struct {
	Name   string            `json:"name,omitempty"`
	Filter *managerCovFilter `json:"filter,omitempty"`
	Weight float64           `json:"weight"`
}

type managerCovFilter struct {
	Files     []string `json:"files,omitempty"`
	Functions []string `json:"functions,omitempty"`
}

// fuzzEnv is nix/kernel-fuzz's env.json: where everything a run needs ended up
// in the store. Paths inside the environment are relative to it, because a
// derivation cannot name its own output path at evaluation time.
type fuzzEnv struct {
	root string

	Kernel         string `json:"kernel"`
	Initrd         string `json:"initrd"`
	KernelObj      string `json:"kernel_obj"`
	Image          string `json:"image"`
	KernelSrc      string `json:"kernel_src"`
	KernelBuildSrc string `json:"kernel_build_src"`
	Syzkaller      string `json:"syzkaller"`
}

func readFuzzEnv(root string) (*fuzzEnv, error) {
	raw, err := os.ReadFile(filepath.Join(root, "env.json"))
	if err != nil {
		return nil, fmt.Errorf("reading the fuzzing environment: %w", err)
	}
	env := &fuzzEnv{root: root}
	if err := json.Unmarshal(raw, env); err != nil {
		return nil, fmt.Errorf("the fuzzing environment is not readable: %w", err)
	}
	return env, nil
}

// path resolves one of the environment-relative entries.
func (e *fuzzEnv) path(rel string) string { return filepath.Join(e.root, rel) }

// The guest kernel command line.
//
// net.ifnames=0 so the NIC is eth0, which is what the guest's static
// addressing expects — see nix/kernel-fuzz/guest-init.sh.
//
// The LSM arguments are inherited from a hard-won lesson and kept as
// insurance. Against an image that ships a policy, the fuzzer reaches
// selinuxfs and writes /sys/fs/selinux/enforce itself; from that moment every
// syz-executor mmap of the data segment is denied execmem and fails with
// EACCES, the VM is useless until it reboots, and the pool spends its time
// booting rather than fuzzing. This guest loads no policy, so the situation
// cannot arise — but it costs nothing to keep it impossible.
//
// syzkaller prepends "root=/dev/sda console=ttyS0" from its own architecture
// defaults. The root= is inert here: the kernel finds an /init in the
// initramfs and never looks for a root filesystem.
const guestCmdline = "net.ifnames=0 lsm= selinux=0 apparmor=0"

// renderConfig turns a validated job into the manager configuration for it.
func renderConfig(job *Job, env *fuzzEnv, workdir, sshKey string, httpPort int, vm resolvedVM) *managerConfig {
	spec := job.Spec.Fuzz

	cfg := &managerConfig{
		Target:  "linux/amd64",
		HTTP:    "127.0.0.1:" + strconv.Itoa(httpPort),
		Workdir: workdir,

		KernelObj:      env.path(env.KernelObj),
		KernelSrc:      env.KernelSrc,
		KernelBuildSrc: env.KernelBuildSrc,

		Image:     env.path(env.Image),
		SSHKey:    sshKey,
		Syzkaller: env.Syzkaller,

		Procs: spec.Procs,
		Type:  "qemu",
		// The executor runs as root in a VM whose entire purpose is to be
		// destroyed, and every layer of sandboxing inside it is kernel surface
		// the fuzzer then cannot reach.
		Sandbox: "none",

		Cover:          true,
		Reproduce:      spec.Reproduce == nil || *spec.Reproduce,
		PreserveCorpus: true,

		EnabledSyscalls:  spec.EnableSyscalls,
		DisabledSyscalls: spec.DisableSyscalls,

		VM: managerVM{
			Count:    vm.Count,
			Kernel:   env.path(env.Kernel),
			Initrd:   env.path(env.Initrd),
			Cmdline:  guestCmdline,
			CPU:      vm.CPU,
			Mem:      vm.MemoryMB,
			QemuArgs: renderQemuArgs(spec.VM.Devices),
		},

		Experimental: managerExperimental{
			FocusAreas: renderFocusAreas(spec),
		},
	}

	return cfg
}

// renderFocusAreas converts the job's focus areas, and appends the catch-all.
//
// THE CATCH-ALL IS THE POINT OF THIS FUNCTION. With a non-empty focus_areas
// list and no entry whose filter is empty, syzkaller does not merely
// deprioritise the rest of the kernel — it ignores it, throwing away the
// general coverage signal, silently, while continuing to look like a healthy
// run. Appending it here rather than asking callers to remember it is why
// FocusSpec has no way to express an unfiltered area.
func renderFocusAreas(spec *FuzzSpec) []managerFocusArea {
	if len(spec.Focus) == 0 {
		// No focus at all is a general run, and a general run must not carry a
		// focus_areas list: a list containing only the catch-all is not the
		// same thing as no list, and the difference is a needless layer of
		// weighting between syzkaller and its own defaults.
		return nil
	}

	areas := make([]managerFocusArea, 0, len(spec.Focus)+1)
	for _, area := range spec.Focus {
		areas = append(areas, managerFocusArea{
			Name: area.Name,
			Filter: &managerCovFilter{
				Files:     area.Files,
				Functions: area.Functions,
			},
			Weight: area.Weight,
		})
	}

	background := 1.0
	if spec.BackgroundWeight != nil {
		background = *spec.BackgroundWeight
	}
	areas = append(areas, managerFocusArea{Weight: background})

	return areas
}

// renderQemuArgs builds the extra-device arguments, or returns "" so that
// syzkaller's own defaults survive untouched.
//
// When it does return something it has to restate "-enable-kvm -cpu
// host,migratable=off". syzkaller seeds qemu_args from its per-architecture
// configuration and then overwrites the whole field with whatever the config
// says, so a job that asks for one extra NIC and gets only that would drop
// KVM — and the VMs would fall back to emulating every instruction. That does
// not fail; it just runs roughly fifty times slower, which reads as a rig
// having a bad day rather than as a configuration mistake.
func renderQemuArgs(devices []FuzzDevice) string {
	if len(devices) == 0 {
		return ""
	}

	args := []string{"-enable-kvm", "-cpu", "host,migratable=off"}
	for i, device := range devices {
		spec := device.Type
		if device.Addr != "" {
			spec += ",addr=" + device.Addr
		}

		if device.NetDev == "user-restricted" {
			id := "vf" + strconv.Itoa(i)
			spec += ",netdev=" + id
			args = append(args, "-device", spec,
				// restrict=on: the guest gets a network it cannot leave. It is
				// running attacker-shaped code by construction.
				"-netdev", "user,id="+id+",restrict=on")
			continue
		}
		args = append(args, "-device", spec)
	}

	return strings.Join(args, " ")
}

// resolvedVM is the VM pool after the rig's capacity has had its say.
type resolvedVM struct {
	Count    int
	CPU      int
	MemoryMB int

	// Why the count is what it is, for the job log. A caller who asked for
	// eight VMs and got two should be told which resource ran out rather than
	// left to wonder whether the request was ignored.
	Reason string
}

// sizeVMs decides how many VMs of what size this rig can actually carry.
//
// Ported from the syz-linux devshell's syz-init, and the rule it encodes is
// the interesting part: the count is the smaller of what the cores and the RAM
// can carry. Deriving it from core count alone overcommits memory on anything
// with a high core-to-RAM ratio — a 128-core machine once produced 64 VMs of
// 2048 MiB on a 125 GiB host, which does not fail, it thrashes.
//
// A caller may ask for fewer than the computed cap. They may not ask for more:
// the cap is about the machine, and a rig that has been talked into
// overcommitting is a rig that stops answering.
func sizeVMs(spec FuzzVMSpec, lim Limits) resolvedVM {
	cpu := spec.CPU
	if cpu == 0 {
		cpu = lim.VMCPU
	}
	memory := spec.MemoryMB
	if memory == 0 {
		memory = lim.VMMemoryMB
	}
	if memory < minFuzzVMMemoryMB {
		memory = minFuzzVMMemoryMB
	}

	cores := numCPU()
	totalMB := totalMemoryMB()

	byCPU := (cores - lim.HostReserveCPU) / cpu
	byMemory := (totalMB - lim.HostReserveMB) / memory

	limit, reason := byCPU, "limited by cores"
	if byMemory < byCPU {
		limit, reason = byMemory, "limited by memory"
	}
	if limit < 1 {
		limit, reason = 1, "the machine is smaller than one VM's share; running one anyway"
	}

	count := limit
	switch {
	case spec.Count > 0 && spec.Count <= limit:
		count, reason = spec.Count, "as requested"
	case spec.Count > limit:
		reason = fmt.Sprintf("%d requested, %s to %d", spec.Count, reason, limit)
	}

	return resolvedVM{Count: count, CPU: cpu, MemoryMB: memory, Reason: reason}
}

// corpusFingerprint identifies the corpus a run should start from and write
// back to.
//
// Keyed on the focus areas and syscall filters, and deliberately NOT on the
// commit or the kernel configuration. A syzkaller corpus is a set of programs,
// not a set of addresses, so it carries across kernel rebuilds — which is how
// the corpus is used in practice, and what makes a bounded four-hour run
// something other than a cold start. What does invalidate it is a change to
// what the fuzzer was aiming at, because that is what shaped it.
func corpusFingerprint(spec *FuzzSpec) string {
	// Weights are excluded on purpose. Re-weighting an existing campaign
	// changes how hard the fuzzer leans on code it was already aiming at, and
	// the programs it found there stay just as useful; changing the filters
	// points it somewhere else, and they do not.
	areas := make([]FocusSpec, 0, len(spec.Focus))
	for _, area := range spec.Focus {
		areas = append(areas, FocusSpec{
			Name:      area.Name,
			Files:     sortedCopy(area.Files),
			Functions: sortedCopy(area.Functions),
		})
	}
	sort.Slice(areas, func(i, j int) bool { return areas[i].Name < areas[j].Name })

	key := struct {
		Focus    []FocusSpec `json:"focus"`
		Enabled  []string    `json:"enabled"`
		Disabled []string    `json:"disabled"`
	}{areas, sortedCopy(spec.EnableSyscalls), sortedCopy(spec.DisableSyscalls)}

	raw, _ := json.Marshal(key)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}
