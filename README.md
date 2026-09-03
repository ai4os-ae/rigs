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
