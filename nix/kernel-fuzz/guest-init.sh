#!/bin/sh
# PID 1 in a rig fuzzing VM.
#
# This machine exists to be crashed. syz-manager boots it, copies syz-executor
# in over SSH, drives it until the kernel dies, and boots another one — many
# times an hour, for hours. So the only thing this script has to achieve is
# that sshd answers on 10.0.2.15:22, as fast as possible and without depending
# on anything the kernel under test might not have.
#
# It is a much smaller job than nix/kernel-test/guest-init.sh, which has to run
# a test and report a verdict over a serial channel. Here syz-manager is the
# one watching the console, and it knows how to read a panic. Nothing needs to
# be framed, and there is nothing to hand back: the crash *is* the result, and
# syz-manager already has it.
#
# WHY STATIC ADDRESSING AND NOT DHCP. QEMU's user-mode network is a fixed
# fiction — the guest is always 10.0.2.15, the gateway and DHCP server are
# always 10.0.2.2 — so a DHCP client here would spend a second of every boot
# negotiating a lease whose contents are known at build time. It would also
# drag in udhcpc's script directory, and a boot that depends on running a
# shebang script is a boot that fails confusingly on a kernel configured
# without BINFMT_SCRIPT.
#
# WHICH INTERFACE. syzkaller appends its own `-device <nic>,netdev=net0` after
# whatever a focus profile asked for, so its NIC lands on a low PCI slot and is
# enumerated first. With net.ifnames=0 that makes it eth0. This is why a focus
# profile's extra devices are required to sit at a high PCI address (the API
# validates `addr` >= 0x10 for exactly this reason): put one at a low slot and
# it becomes eth0, sshd answers on the wrong network, and every VM in the pool
# is reported as having failed to boot.

set -u

# Sourced first: it is what puts busybox and sshd on PATH, and there is no
# `mount` to call until it has been. It needs nothing mounted itself.
# shellcheck source=/dev/null
. /etc/rig/env

# Already-mounted is not an error: it is what happens when the kernel has
# CONFIG_DEVTMPFS_MOUNT, which nix/kernel-fuzz/config.nix forces on.
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t tmpfs tmpfs /tmp 2>/dev/null || true
mount -t tmpfs tmpfs /run 2>/dev/null || true

# DEBUGFS IS NOT OPTIONAL AND NOTHING ELSE HERE WILL MOUNT IT.
#
# syz-executor reaches KCOV by opening /sys/kernel/debug/kcov, so without this
# mount syz-manager gets all the way through booting a VM, SSHing into it and
# probing its features, and then exits with "coverage is not supported:
# open of /sys/kernel/debug/kcov failed". CONFIG_DEBUG_FS=y — which
# nix/kernel-fuzz/config.nix forces — only makes the filesystem available; it
# does not mount it. On syzkaller's usual Debian guest systemd does this, which
# is why upstream's documentation never mentions it.
#
# Fault injection lives under the same mount, so it fails the same way.
mount -t debugfs debugfs /sys/kernel/debug 2>/dev/null || true

# Same story, smaller stakes: syzkaller reads /sys/kernel/security/lsm during
# its feature probe and reports the machine as misconfigured without it.
mount -t securityfs securityfs /sys/kernel/security 2>/dev/null || true

# Several descriptions want a cgroup hierarchy to put processes into. Absent
# from the configuration under test is fine — this is a caller's choice, not a
# requirement — which is what the redirect is for.
mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null || true

# devtmpfs provides the device nodes the kernel knows about and nothing else.
# A great deal of software assumes these four without ever saying so — any
# `> /dev/stderr`, and bash's process substitution, which passes descriptors to
# children as /dev/fd/N.
ln -sf /proc/self/fd /dev/fd
ln -sf /proc/self/fd/0 /dev/stdin
ln -sf /proc/self/fd/1 /dev/stdout
ln -sf /proc/self/fd/2 /dev/stderr

say() { echo "rig-fuzz: $*" >/dev/console; }

# ------------------------------------------------------------------- modules
#
# A driver under test that was configured as =m has to be loaded before the
# fuzzer can reach it, and the fuzzer has no way to do it itself. depmod ran
# when the guest image was built, so modprobe resolves dependencies unaided.
#
# Not fatal. A module that will not load is worth saying loudly on a console
# syz-manager is recording, but it is not worth refusing to fuzz over — the
# rest of the kernel is still there.
for module in ${RIG_MODULES:-}; do
	if modprobe "$module" >/dev/null 2>&1; then
		say "loaded $module"
	else
		say "FAILED to load module $module"
	fi
done

# ------------------------------------------------------------------- network

ip link set dev lo up
ip link set dev eth0 up
ip addr add 10.0.2.15/24 dev eth0
ip route add default via 10.0.2.2

# ---------------------------------------------------------------------- sshd
#
# The host key is generated per boot rather than baked into the image. It costs
# a few milliseconds for ed25519, and it means this repository does not carry a
# private key. syz-manager connects with StrictHostKeyChecking=no, so a key
# that changes every boot is exactly what it expects.
mkdir -p /var/empty /run/sshd
chown 0:0 /var/empty
chmod 755 /var/empty

ssh-keygen -q -t ed25519 -N "" -f /etc/ssh/ssh_host_ed25519_key

# Written here rather than baked in, so that the ownership and modes are the
# ones sshd wants regardless of what uid the cpio archive recorded. An
# initramfs unpacks with the uids the archive holds, and the archive was built
# by a Nix build user, not by root.
mkdir -p /root/.ssh
cp /etc/rig/authorized_keys /root/.ssh/authorized_keys
chown -R 0:0 /root
chmod 700 /root/.ssh
chmod 600 /root/.ssh/authorized_keys

say "starting sshd on 10.0.2.15:22"

# Three things here, and every one of them is a VM that boots and is then
# reported as never having booted:
#
#   $RIG_SSHD, not `sshd`. It refuses to run when argv[0] is not an absolute
#   path — it re-executes itself per connection — and says so once, on a
#   console nobody reads until the whole pool has failed.
#
#   -f, because nixpkgs builds openssh with its sysconfdir inside the store.
#   Without it sshd reads the store's stock sshd_config instead of ours, which
#   names no host key we generated and no authorized_keys we installed.
#
#   -D to stay in the foreground, -e to log to stderr — which is the console,
#   which is what syz-manager records. When a VM is not coming up, that log is
#   the only evidence there is.
"$RIG_SSHD" -D -e -f /etc/ssh/sshd_config

# sshd exiting is the end of the VM's usefulness, and PID 1 falling off the end
# of this script would panic the kernel — a panic syz-manager would dutifully
# report as a kernel bug found by fuzzing. Say what happened, then stay alive
# and let the manager's boot timeout deal with it.
say "sshd exited; this VM is no longer reachable"
while true; do sleep 60; done
