# ai4os-rigs

![NixOS Flake](https://img.shields.io/badge/NixOS-flake-blue?logo=nixos)
![secrets sops-nix](https://img.shields.io/badge/secrets-sops--nix-blue)

NixOS configurations for the AI4OS **rigs** — remote machines used to test
research ideas and to run jobs. Each rig is unattended: no display, no console
anybody sits at, SSH-key login only, and a rebuild from this repository every
night.

## Rigs

| Name        | System              | CPU              | RAM  | Disk            | Mesh        | Role    | State |
|-------------|---------------------|------------------|------|-----------------|-------------|---------|-------|
| `laptop-01` | Dell Latitude 7440  | i7-1365U (12T)   | 32GB | 512GB NVMe      | 10.10.0.60  | Compute | New   |
| `desktop-01` | Desktop             | Core Ultra 7 265 (20T) | 128GB | 2TB NVMe | 10.10.0.62  | Compute | New   |

`laptop-01` has no ethernet port — wireless is its only uplink. That is why
`modules/base/wifi.nix` exists and why its install pre-seeds the host key (see
"Installing").

## Layout

```
flake.nix          inputs, devshell, treefmt
hosts/             one directory per rig
  default.nix      nixosConfigurations
  laptop-01/       default.nix, hardware.nix, disk.nix
modules/
  base/            applies to every rig
    options.nix    the `rigs.*` option namespace
    people.nix     the roster: who has an account, with which keys
    users.nix      turns the roster into NixOS accounts
    ssh.nix        sshd, key-only
    secrets.nix    sops-nix wiring
    nebula.nix     the sifr0 overlay; how a rig is reached
    wifi.nix       wpa_supplicant, for rigs with no ethernet
    autoupgrade.nix
  rig/             unattended always-on behaviour (lid, watchdog, oomd)
    buildd.nix     the build job daemon, on every rig
    fleetd.nix     the broker in front of them
nix/
  kernel-build/    parameterised two-stage Linux kernel builds
  kernel-test/     booting one of those under QEMU and running tests in it
pkgs/
  rig-api-docs/    OpenAPI spec + Swagger UI, served as the public root
  rig-buildd/      the job daemon itself (Go, stdlib only)
  rig-fleetd/      the fleet broker
  rig-kiosk-server/
secrets/           sops-encrypted, one file per rig plus all.yaml
```

Everything this repository configures lives under the `rigs.*` option
namespace.

## Working on it

```
nix develop            # sops, ssh-to-age, nixos-anywhere, nixos-rebuild
nix fmt                # nixfmt + deadnix + statix + shellcheck
nix flake check        # evaluates every rig; slow
```

To check a single rig without the full flake check:

```
nix build .#nixosConfigurations.laptop-01.config.system.build.toplevel
```

## Adding a person

Add a block to `modules/base/people.nix` with a fresh uid and their public SSH
key, then rebuild each rig. Nothing is per-host: the roster applies everywhere.

When somebody leaves, set `enable = false` rather than deleting the block, so
their uid is never handed to the next person.

## Adding a rig

1. Create `hosts/<name>/` with `default.nix`, `hardware.nix` and `disk.nix`.
   Copy `laptop-01` as the starting point.
2. Register it in `hosts/default.nix` under `nixosModules`,
   `nixosConfigurations` and `hydraJobs`.
3. Leave `rigs.bootstrap = true` for the first install.
4. Install (below), then finish the sops step and set `bootstrap = false`.
5. Issue it a mesh certificate and add its address to `modules/base/nebula.nix`
   (see "Reaching a rig").

## Installing

No USB stick is needed, and none of this uses one. The target kexecs into the
installer from whatever Linux is already on its disk; the store lives in the
initrd, so the disk is free to be repartitioned the moment the new kernel takes
over. `nixos-anywhere` drives the whole thing over SSH.

Two artifacts exist for this:

| Output      | What it is                          | For                        |
|-------------|-------------------------------------|----------------------------|
| `#kexec`    | kernel + initrd + `kexec-boot`      | a target already running Linux |
| `#netboot`  | kernel + initrd + `netboot.ipxe`    | PXE / UEFI HTTP boot (needs wired) |

`nix build .#kexec` / `nix build .#netboot`.

### 0. Prerequisites on the target

- **Turn Secure Boot off in the firmware.** Under Secure Boot the running
  kernel is in lockdown mode and refuses `kexec_load` for an unsigned kernel,
  which is exactly what we hand it. This is the single most common way the
  route below fails, and it fails with a permissions error that does not
  mention Secure Boot.
- Get root on the target. If it runs a distro you are locked out of: at the
  GRUB menu press `e`, change `ro` to `rw` on the `linux` line, append
  `init=/bin/bash`, and Ctrl-X. That is a root shell with no password; `passwd`
  from there, then reboot normally.
- Make sure `openssh-server` is installed and running on it.

### 1. Give the target a link this machine can reach

`nixos-anywhere` needs SSH to the target, and the SSH session has to come back
at the *same address* after the kexec. Most campus and office wireless networks
have client isolation, so two machines on the same SSID usually cannot reach
each other — check before assuming. Options, best first:

**Thunderbolt / USB4 cable.** The fastest and least fiddly option, but it has
three prerequisites that each fail silently:

- *A Thunderbolt 3/4 or USB4 cable.* Host-to-host networking is a Thunderbolt
  feature, not a USB one — two USB-C hosts joined by an ordinary USB-C cable do
  nothing at all. Most cables bundled with phones and chargers are USB 2.0 and
  will not establish a link.
- *The `thunderbolt-net` module on this end.* It is not loaded by default;
  only the `thunderbolt` core module is.
- *Authorisation.* Controllers ship at security level `user`, so a freshly
  attached peer is inert until it is enrolled. Check with
  `cat /sys/bus/thunderbolt/devices/domain0/security`.

```
sudo modprobe thunderbolt-net
boltctl list                      # find the peer's UUID
sudo boltctl enroll <uuid>        # once per peer; remembered afterwards
sudo ip addr add 10.99.0.1/24 dev thunderbolt0 && sudo ip link set thunderbolt0 up
```

The installer end is already configured: it comes up at `10.99.0.2/24` and
auto-authorises its peer, which it has to — the kexec re-enumerates the bus and
there is no `bolt` daemon or human on that side to re-approve the link.

**A throwaway hotspot from this machine.** Needs no cable and no dongle — an
Intel BE200 and most modern cards support AP mode. Bring one up, join the
target to it, and tear it down afterwards. Because the installer must rejoin
that network *by itself* after the kexec, put its credentials in
`secrets/installer-wifi.nix` (gitignored):

```nix
{ ssid = "rig-install"; psk = "a throwaway passphrase"; }
```

Use a passphrase minted for the occasion. It ends up in plaintext in the
installer's initrd — that is why the file is gitignored, and why a real
network's key does not belong in it.

### 2. Pre-seed the host key, then install

A rig's sops identity is derived from its SSH host key, so a freshly installed
rig cannot decrypt anything until its key is known — and a rig whose only
uplink is wireless keeps its wifi password in sops. Installing first and fixing
it later means installing a machine with no network. Generate the key *before*
installing instead:

```
mkdir -p /tmp/seed/etc/ssh
ssh-keygen -t ed25519 -N "" -C laptop-01 -f /tmp/seed/etc/ssh/ssh_host_ed25519_key
ssh-to-age < /tmp/seed/etc/ssh/ssh_host_ed25519_key.pub
```

Put that age key in `.sops.yaml`, create `secrets/all.yaml` and
`secrets/laptop-01.yaml`, set `rigs.bootstrap = false` on the host, then:

```
nixos-anywhere --flake .#laptop-01 \
  --extra-files /tmp/seed \
  root@10.99.0.2
```

`disko` partitions the disk from `hosts/<name>/disk.nix`. **Check the `device`
path in that file against `lsblk` on the target first** — the wrong path wipes
the wrong disk. Shred `/tmp/seed` afterwards.

Also replace `hosts/<name>/hardware.nix` with the output of
`nixos-generate-config --no-filesystems` run on the target before installing.

## Secrets

Secrets are `sops`-encrypted with `age`. A rig's age identity is derived from
its SSH host key, so it only exists after the first install.

Once the rig is up:

```
ssh root@<address> 'ssh-to-age < /etc/ssh/ssh_host_ed25519_key.pub'
```

Add that key to `.sops.yaml` under `keys:` and to the relevant
`creation_rules`, then re-encrypt:

```
sops updatekeys secrets/all.yaml
```

Finally set `rigs.bootstrap = false` on the host and rebuild. Until that
happens the admin accounts carry a known placeholder password, and the build
warns about it on every evaluation.

## Reaching a rig

Rigs sit on campus wireless behind NAT, with no inbound port and no address
that stays put. They are reached over **nebula** instead: `modules/base/nebula.nix`
puts every rig on the `sifr0` overlay, where it has a fixed `10.10.0.0/24`
address regardless of what network it woke up on.

The overlay is not this repository's. It belongs to the personal dotfiles fleet
and rigs are guests on it, which decides two things:

- The CA is that network's CA (`modules/base/sifr0-ca.crt`, public — the CA
  *key* is not here and is not on any rig). A certificate can only be issued by
  somebody holding it.
- Rigs are issued certificates in the **`ai4os` group and no other**. Groups are
  what the rest of the fleet writes its firewall rules against, so a rig
  carrying `trusted` would be handed the access a personal laptop has.

Addresses come from the **10.10.0.60+** block. Everything below it is a personal
device allocated in the dotfiles repo; allocating rigs from the top keeps the
two rosters from growing into each other.

Inbound, a rig accepts ICMP from anyone on the mesh, everything from `trusted`
(administration), and everything from `ai4os` (rig to rig). Outbound is
unrestricted — though a rig can only reach a personal host that admits `ai4os`
in its own rules, which by default none do.

To issue a certificate for a new rig, on a machine with the CA key:

```
nebula-cert sign -ca-crt ca.crt -ca-key ca.key \
  -name <rig> -networks 10.10.0.<next>/24 -groups ai4os \
  -out-crt <rig>.crt -out-key <rig>.key
```

Both halves go into that rig's sops file under `nebula/crt` and `nebula/key`
(the certificate is public, but keeping the pair together means nothing has to
be copied onto the rig by hand), owned by `nebula-sifr0` — nebula reads them
after dropping privileges. Then add the address to the `hosts` map in
`modules/base/nebula.nix` so the rest of the fleet can use the name.

## Build jobs

Running jobs is what a rig is for, and `rig-buildd` is how work gets to one.
Post a kernel commit, an optional patch and a configuration; the rig builds it
and serves back the artifacts, the logs and a report.

There is no central server and no agent polling one. Rigs have fixed mesh
addresses and already accept inbound connections from `ai4os` and `trusted`, so
a client connects to the rig it wants and picks between rigs itself using
`/v1/status`. Nebula has authenticated and encrypted the connection before the
daemon sees it, and the host firewall trusts `sifr0` and nothing else, so the
API is not exposed to whatever network the rig has joined.
`rigs.buildd.tokenFile` adds a bearer token for when that stops being enough.

| Method   | Path                              |                                               |
|----------|-----------------------------------|-----------------------------------------------|
| `POST`   | `/v1/jobs`                        | submit; returns the job                       |
| `GET`    | `/v1/jobs`                        | list, newest first                            |
| `GET`    | `/v1/jobs/{id}`                   | state, timings, whether the build passed      |
| `DELETE` | `/v1/jobs/{id}`                   | cancel if running, otherwise delete           |
| `GET`    | `/v1/jobs/{id}/log`               | `?offset=N&follow=1`                          |
| `GET`    | `/v1/jobs/{id}/artifacts`         | manifest with sizes and sha256                |
| `GET`    | `/v1/jobs/{id}/artifacts/{path}`  | one file                                      |
| `GET`    | `/v1/jobs/{id}/bundle.tar.gz`     | the whole output tree                         |
| `GET`    | `/v1/status`                      | load, free space, queue, mirror state         |

```
curl -sX POST http://laptop-01:9000/v1/jobs -d '{
  "kind": "build",
  "kernel": { "commit": "7d0a66e4bb9081d75c82ec4957c50034cb0ea449" },
  "config": { "base": "defconfig", "set": { "CONFIG_ZRAM": "m" } },
  "patch": "--- a/drivers/block/zram/zram_drv.c\n+++ ..."
}'
```

There are two kinds of job. `build` compiles a kernel; `test` boots one under
QEMU and runs something in it. Both are the same envelope, the same queue and
the same endpoints, because both are pure functions of their inputs and are
answered by a derivation. See "Test jobs" below.

Symbol values are `y`, `m` or `n` and nothing else — this is data, not a place
to smuggle in make commands. Every requested symbol is re-read from the
resulting `.config` and a build that lost one fails, because a symbol silently
dropped for unmet dependencies otherwise produces a kernel that quietly lacks
the driver under test.

**A failed compile is a successful job.** The job is `succeeded` and the report
says `passed: false`, with the compiler error in the logs. `failed` is reserved
for the daemon or Nix breaking, meaning the caller learned nothing about their
patch.

### Why it is fast the second time

Builds run under Nix, which is not visible in the API but is the reason the
same work is never done twice. A job is split in two: an unpatched base for its
(commit, configuration), and the patch applied on top of it. The base is shared
by every patch against that commit, so only the first one pays for it.

Measured on `laptop-01` (12 threads), x86_64 `defconfig` at v6.18. The patched
stage recompiles one source file and relinks:

```
base           5m28s    once per commit and configuration
patched          22s    per candidate patch
resubmitted       1s    identical job, straight from the store
```

### Builds are reproducible, and that is deliberate

A kernel stamps the time, host and user that built it into the image and into
every module's `vermagic`. Left alone, two rigs building the same job produce
different bytes under the *same* store path — Nix derives that path from the
inputs, not from the result — so a manifest fetched from one rig would not
match bytes fetched from another, which is a tedious thing to discover later.

`nix/kernel-build` pins `KBUILD_BUILD_TIMESTAMP`, `KBUILD_BUILD_USER` and
`KBUILD_BUILD_HOST`, so it does not happen. Verified: the same job built on a
64-thread AMD workstation and on `laptop-01` produced all 28 artifacts
byte-identical. A client may therefore treat the store path as an identity and
send the two halves of a comparison to two different rigs.

### The kernel mirror

A job carries a commit hash, not a source tree — 1.5GB per job over campus
wireless is exactly what this avoids. Each rig keeps a bare clone at
`/var/lib/rig/buildd/linux.git` and fetches a commit it does not have.

**The first job on a new rig clones the whole kernel before it builds
anything** — around ten minutes and 6.8GB for the stable tree, plus about as
much again the first time Nix imports a revision into its own git cache. Watch
`mirror.present` in `/v1/status`, or seed the mirror by hand, before concluding
that a rig is slow.

## Test jobs

A kernel that compiles is not a kernel that works. `kind: test` boots one in a
QEMU virtual machine, runs something in it, and tears the machine down.

```
curl -sX POST http://laptop-01:9000/v1/jobs -d '{
  "kind": "test",
  "kernel": { "commit": "7d0a66e4bb9081d75c82ec4957c50034cb0ea449" },
  "config": { "base": "defconfig", "set": { "CONFIG_ZRAM": "m" } },
  "test":   { "modules": ["zram"] }
}'
```

A test names its kernel in one of two ways, and both exist because they answer
different needs:

- **A full spec**, exactly as a build job describes one. Self-contained, means
  the same thing on any rig, and costs nothing extra when that rig has already
  built it — the store answers the build stage and the job goes straight to
  booting.
- **`"from": "<job-id>"`**, which boots a job you already ran. The short way to
  say "boot what I just built", and the only way to test a kernel whose spec
  you no longer have. Through the broker the id is `<rig>.<id>`, and the rig
  named in it is where the test runs, because that is where the kernel is.

The result is judged the same way a build is. **A kernel that panics is a
successful job** with `passed: false`; `failed` still means the rig broke.

### What runs inside

With no `test.script`, a test boots the kernel and runs the kernel's own
selftests on it. With one, it runs that instead — Python 3, as an ordinary
program, its exit status the test's verdict and everything it printed kept as
an artifact:

```
curl -sX POST http://laptop-01:9000/v1/jobs -d '{
  "kind": "test",
  "from": "20260908T091828-0221e5cfee685827",
  "test": {
    "modules": ["zram"],
    "script": "import os\nassert os.path.exists(\"/sys/block/zram0\")\n..."
  }
}'
```

The guest is a plain Linux userspace — busybox, coreutils, bash, python3, and
the kernel's own modules under `/lib/modules` — with **no network**, because
the build sandbox has none to give it. It is gone the moment the script
returns.

A test fails if the kernel does not reach userspace, if a requested module will
not load, if the script or a selftest fails, if the run exceeds
`timeout_seconds`, or if the kernel log contains an oops, a `BUG:` or a panic.
A bare `WARNING:` does not, unless `fail_on_warning` is set — a clean kernel
warns about something on QEMU's hardware often enough that failing on it would
only teach people to disregard the verdict.

Alongside the usual log and artifacts, a test job produces `result.json` with
the verdict and the per-test results, `console.log` — the serial console from
firmware to power-off, which is where a panic is — and `dmesg.log`,
`kselftest.log`, `script.log`, `lsmod.log`, `uname.log`.

### About kselftest

`test.kselftest.targets` names collections under `tools/testing/selftests`. The
default is `exec`, `kcmp` and `size` — deliberately small: these are the ones
observed to pass, every subtest, on a plain x86_64 `defconfig`. A default that
goes red on a pristine upstream kernel teaches people to disregard the whole
thing.

**Most other collections need configuration symbols `defconfig` does not set,
and kselftest reports a missing symbol as a failure rather than a skip.** So
naming a target usually means naming the symbols it wants in `config.set` as
well — `prctl` wants `CONFIG_ANON_VMA_NAME`, `splice` wants `CONFIG_TEST_LKM`,
`memfd` wants `CONFIG_FUSE_FS`. A target that does not compile at all is
reported in `result.json` under `kselftest.targets_missing` and fails the test,
rather than being quietly dropped.

Selftests are built from the **unpatched** source, once per (commit, target
set), so every candidate patch against a commit shares them — the same trade
the kernel build makes. A patch that edits `tools/testing/selftests` itself is
therefore not reflected.

### How a test is put together

Three cached derivations and one that is not:

```
linux-<commit>-selftests   the kernel's own tests, per (source, targets)
linux-<commit>-image       bzImage and a depmod'd module tree
linux-<commit>-guest       an uncompressed initramfs: userspace + the above
linux-<commit>-test        the QEMU run itself
```

Only the last is per-test, so a second test against the same kernel starts
booting immediately. The guest image is **uncompressed** on purpose: a
compressed initramfs would make the guest's decompressor part of this harness's
requirements, in a configuration the caller chose and we do not control.

Nothing is shared with the host — no 9p, no virtio-fs, for the same reason.
Results leave over a second serial port, framed and base64'd, while `ttyS0`
carries the console. That is what `GET /v1/jobs/{id}/log?follow=1` streams, so
you can watch a kernel boot as it happens.

Test jobs need `/dev/kvm`: the derivation asks for the `kvm` system feature, so
a rig without it refuses to schedule the job rather than silently emulating
every instruction and turning a ten second boot into several minutes.

Unlike builds, a test's output is **not** byte-reproducible — it contains
`dmesg` timestamps and real timings. The reproducibility guarantee below is
about build artifacts.

### State

Everything is under `/var/lib/rig/buildd/jobs/<id>/` as plain files — no
database:

```
job.json         the job and its state
job.nix          the generated expression; `nix build -f` it to reproduce by hand
log              the build log
candidate.patch  the patch, if there was one
test.py          the test script, if there was one
result           symlink into the store — and the GC root
```

That `result` link is load-bearing. It is what stops the nightly garbage
collection removing a finished job's artifacts before anybody fetched them, so
deleting a job is what releases its disk space.

## Fuzz jobs

A kernel that boots and passes its tests is a kernel nobody has attacked yet.
`kind: fuzz` runs [syzkaller](https://github.com/google/syzkaller) against one
for a few hours and hands back whatever it broke.

```
curl -sX POST http://laptop-01:9000/v1/jobs -d '{
  "kind": "fuzz",
  "kernel": { "commit": "7d0a66e4bb9081d75c82ec4957c50034cb0ea449" },
  "fuzz":   { "duration_seconds": 14400 }
}'
```

Enable it per rig with `rigs.buildd.fuzz.enable`; it is off by default, because
unlike a build or a test the VMs are started by the daemon itself rather than
inside a Nix sandbox, so the unit needs `/dev/kvm` of its own. Check `kinds` in
`/v1/status` rather than assuming.

**This is the one kind that is not a derivation, and the API says so.** A build
and a test are pure functions of their inputs, so the store answers the second
identical request without doing the work. A fuzzing run is a long-lived process
whose findings depend on how long it ran and how lucky it was. So only its
*inputs* are built by Nix — the kernel, the guest image and the fuzzer,
gathered by `nix/kernel-fuzz` into one store path — and the run itself is a
child process the daemon supervises against a deadline.

The kernel is built the same way every other kernel here is, with syzkaller's
requirements forced into the configuration: `KCOV` and `KCOV_INSTRUMENT_ALL`
for coverage feedback, real DWARF for symbolising it, `KASAN` by default, and
the handful of symbols without which the VM will not boot or crash detection
goes blind. `nix/kernel-fuzz/config.nix` has the list and the reason for each.
Two consequences worth knowing: a fuzz kernel shares no build with a `build` or
`test` job against the same commit, and KASAN roughly doubles the build.

### Directed fuzzing

`fuzz.focus` weights how often the fuzzer picks programs that cover a region:

```
curl -sX POST http://laptop-01:9000/v1/jobs -d '{
  "kind": "fuzz",
  "kernel": { "commit": "7d0a66e4bb9081d75c82ec4957c50034cb0ea449" },
  "fuzz": {
    "duration_seconds": 14400,
    "focus": [
      { "name": "e1000",
        "files": ["drivers/net/ethernet/intel/e1000/"], "weight": 10.0 },
      { "name": "netdev-core",
        "files": ["net/ethtool/", "net/core/dev_ioctl.c"], "weight": 3.0 }
    ]
  }
}'
```

Weights are relative to `background_weight`, which covers everything the
filters do not and defaults to `1.0`. **You cannot write that entry yourself,
and that is deliberate.** syzkaller's own configuration format requires it, and
a `focus_areas` list without it makes the fuzzer *ignore* every part of the
kernel outside the filters rather than merely deprioritise it — silently, with
the run continuing to look healthy. The daemon always appends it. Set
`background_weight` to `0` if you genuinely mean "only this".

A filter that matches nothing is rejected before the kernel is built, by
checking it against the tree in the mirror. syzkaller performs the real check
at startup and refuses to run; this one just does it half an hour earlier.

### The part that is not weighting

Some targets are not reachable at all, and no weight will fix it. The e1000
driver is the worked example, and `nix/kernel-fuzz/focus/e1000.json` is the
profile for it: the VM's only e1000 is `eth0`, which the descriptions exclude
because it is the SSH lifeline, and which stays in the init network namespace
when the executor unshares `CLONE_NEWNET`. The way in is syzkaller's NIC-VF
pass-through, which needs no real hardware — just a second emulated NIC parked
at PCI `0000:00:11.0`, which the executor then moves into the fuzzing namespace
and renames `nicvf0`.

That is what a profile is for: everything a campaign needs beyond weights,
reviewed in git rather than retyped into a request.

```
curl -s http://laptop-01:9000/v1/fuzz/profiles
curl -sX POST http://laptop-01:9000/v1/jobs -d '{
  "kind": "fuzz",
  "kernel": { "commit": "7d0a66e4bb9081d75c82ec4957c50034cb0ea449" },
  "fuzz":   { "profile": "e1000", "duration_seconds": 14400 }
}'
```

Anything inline merges over the profile, focus areas by name — so "the e1000
profile, but lean on the driver harder" is two lines rather than a copy.

Extra devices are named as `fuzz.vm.devices` from a rig-configured allow-list
(`rigs.buildd.fuzz.allowedDevices`) rather than as raw QEMU arguments. Raw
`qemu_args` would turn "submit a patch to be fuzzed" into "run QEMU on this rig
with my argv", which `-fsdev local,path=/` makes short work of. Their PCI
address must be `0x10` or above: syzkaller appends its own NIC afterwards, so a
device at a low slot is enumerated first, takes `eth0`, and leaves every VM
unreachable.

### Watching one, and what comes out

A four-hour job that says nothing until it finishes is a job nobody trusts.

```
curl -sN 'http://laptop-01:9000/v1/jobs/{id}/log?follow=1'   # the fuzzer, live
curl -s   'http://laptop-01:9000/v1/jobs/{id}' | jq .fuzz     # counters
curl -s   'http://laptop-01:9000/v1/jobs/{id}/crashes'
curl -s   'http://laptop-01:9000/v1/jobs/{id}/artifacts/crashes/<id>/repro.cprog'
```

The artifact endpoints answer **while the run is going**, unlike a build's — so
a reproducer can be pulled out of a campaign that has four hours left on it.

`fuzz.focus_corpus` is the field to read on a directed run. It is the corpus
size per focus area, and an area sitting at zero while the total climbs means
the fuzzer cannot reach that code at all — a different problem from weighting
it too low, and not otherwise visible.

**A crash means `passed: false`.** This reads backwards from what you wanted,
and it is the same rule as everywhere else here: `passed` describes the kernel,
not whether you got the result you were hoping for. The job did exactly what
was asked; the kernel did not survive it. `state: failed` still means the rig
broke.

### The corpus

syzkaller's corpus — the programs that reached somewhere interesting — is worth
more than any one run. A rig keeps one per campaign under
`/var/lib/rig/buildd/fuzz-corpus/`, seeds each run from it and writes it back,
so a bounded four-hour job is not four hours of rediscovery. It is keyed on the
focus areas and syscall filters, **not** on the kernel: programs stay useful
across rebuilds, which is how a corpus is actually used. `"corpus": "cold"`
opts out.

### The guest

The VM's userspace is an initramfs built by `nix/kernel-fuzz/guest.nix` —
busybox and sshd, about 45MB — rather than the Debian image syzkaller's own
`create-image.sh` produces with debootstrap. That script needs root and a
network and can never be a derivation, which makes it the wrong shape for a
machine nobody walks up to. The trade is real: an initramfs guest reaches less
of the syscall surface than a distribution on an emulated disk, because some
descriptions want a real block device or a normal filesystem layout. What it
buys is a guest that is cached, identical on every rig, rebuilt when the
kernel's modules change, and pristine on every one of the many hundreds of
boots a run performs.

## The fleet broker

Talking to a rig directly is fine while there is one rig and a person choosing
it. `rig-fleetd` puts a single endpoint in front of all of them: it decides
which rig a job runs on, forwards it, and proxies everything afterwards, so a
client sees a fleet rather than a list of machines.

Unlike a rig, it is meant to be **published** — reverse-proxied from a host
with a public address — so every request carries an API key and there is no
unauthenticated path through it, not even `/v1/status`, which would otherwise
enumerate the fleet. It still listens on the mesh only; exposing it is the
reverse proxy's job.

| Method   | Path                       |                                          |
|----------|----------------------------|------------------------------------------|
| `POST`   | `/v1/jobs`                 | place and submit                         |
| `GET`    | `/v1/jobs`                 | every rig's jobs, merged, newest first   |
| `GET`    | `/v1/rigs`                 | the fleet: liveness, system, queue, disk |
| `GET`    | `/v1/status`               | the broker itself                        |

Browsable at the root of that name: `pkgs/rig-api-docs` builds a Swagger UI and
the OpenAPI spec into a directory, which the publishing host serves as the
vhost's root while `/v1/` continues to proxy. The docs are unauthenticated —
somebody who lands on the fleet's one public name should find out what it does
rather than a `401` — and everything they describe is not.

The spec is written by hand and checked in, so it is reviewed like the code it
describes rather than generated from it and believed; the build parses it, so a
typo fails there rather than rendering an empty page. Read it without deploying
anything:

```
nix build .#rig-api-docs && xdg-open result/index.html
```
| *others* | `/v1/jobs/{id}/…`          | as `rig-buildd`, proxied                 |

```
curl -H "Authorization: Bearer $KEY" -X POST https://…/v1/jobs -d '{
  "kernel": { "commit": "7d0a66e…" },
  "config": { "base": "defconfig", "set": { "CONFIG_ZRAM": "m" } },
  "system": "x86_64-linux",
  "patch":  "--- a/drivers/…"
}'
```

Two optional fields on top of a rig's job envelope: `rig` pins it to a machine
by name, and `system` constrains it to an architecture (matched against what
each rig reports, so it stays right when the fleet is no longer all x86). Both
fail with `409` rather than quietly substituting — somebody who named a rig
wants that rig.

**Job ids are `<rig>.<id-on-that-rig>`.** That is the whole routing table: the
broker keeps no job state, so there is nothing to hold in step with the rigs
and nothing lost when it restarts.

### How a rig is chosen

When neither `rig` nor a constraint decides it, among live rigs of the right
system:

1. **The rig that last built this `(commit, configuration)`** — it already has
   the expensive base in its store, which is a 22s job against a 5m28s one, and
   is by far the largest lever available. Skipped if that rig is already
   backed up: a warm store is not worth queueing behind other builds.
2. Otherwise the shortest queue, with free disk breaking ties.

That hint lives in `fleetd/affinity.json` and is only ever a hint — losing it
costs one slow build.

### Keys

One per line in a sops secret, `#` comments and blank lines ignored so each key
can record whose it is, which is the difference between revoking one and
rotating all of them. The service restarts when the secret changes, so removing
a key takes effect without anybody remembering to.

### Publishing it

The broker is published at **`https://rigs.fleeti.ae`**, served by `hisn` and
proxied to `10.10.0.60:9100` over the mesh.

Two halves, in two repositories, and it is a 504 without both:

- **The vhost**, on `hisn`, which is configured from the personal dotfiles
  repository and not from here.
- **The mesh rule**, here. Rigs admit `trusted` and `ai4os`, and `hisn` is
  deliberately in neither — it is a rented machine with a public address, and
  giving it what a personal laptop has to reach a rig would be a poor trade
  for one vhost. `rigs.fleetd.reverseProxy = "hisn"` admits that one node to
  the broker's port and nothing else. It is matched on the peer's signed
  certificate name rather than its address, so naming it grants nothing to
  anybody else who reaches the port.

The vhost needs three things that are not nginx defaults, all for the same
reason — a build is long and its output is a stream: `proxy_read_timeout` well
past an hour, `proxy_buffering off` so a followed log arrives as it is
produced, and `client_max_body_size` above 1M so a large patch is not rejected
before the broker sees it.

## Laptop rigs

A rig that is a laptop should say so:

```nix
rigs.rig.isLaptop = true;
```

Its charger is the nearest one for somebody to borrow, and its lid is the
obvious thing to close on a machine that looks idle — and a rig ignores both,
so a job keeps running with a hidden screen and a draining battery. With this
on, `rig-alarm` beeps out of the laptop's own speaker every few seconds while
the rig is off mains or has its lid shut. Nothing suspends or throttles; the
alarm is the whole of it.

One episode of beeping lasts five minutes and then goes quiet, so a rig nobody
came to is not left making a noise all weekend — it keeps watching, and a new
fault, or the same one clearing and returning, gets a fresh five minutes.
`RIG_ALARM_TIMEOUT` in the unit's environment changes that; 0 never stops.

To check what the sensors say without waiting for a beep:

```
RIG_ALARM_DRY_RUN=1 rig-alarm     # prints "ok", "unplugged", "lid closed"
systemctl stop rig-alarm          # silence it while working on the machine
```

## Upgrades

Every rig runs `nixos-upgrade` nightly at 01:30 (plus up to 45 minutes of
jitter), pulling `github:ai4os-ae/rigs#<hostname>`. It switches the running
system in place — there is no A/B partitioning — and reboots inside a 01:00–05:00
window when the kernel or initrd changed.

**A reboot in that window kills any job running on the rig.** A rig with jobs
that cannot be interrupted should set:

```nix
rigs.autoupgrade.allowReboot = false;
```

which stages kernel updates but leaves the reboot to a human.

To rebuild a rig by hand:

```
sudo nixos-rebuild switch --flake github:ai4os-ae/rigs#<hostname> --refresh
```

or remotely, from a checkout:

```
nixos-rebuild switch --flake .#<hostname> --target-host root@<address>
```
