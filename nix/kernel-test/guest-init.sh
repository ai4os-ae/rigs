#!/bin/sh
# PID 1 in a rig test VM.
#
# There is no init system, no disk and no network. The kernel under test
# unpacks the guest image into a tmpfs, executes this, and everything that
# happens afterwards happens here. Nothing may be assumed about the kernel
# beyond what booting it at all already requires: it is the thing being tested,
# and a harness that needs 9p or virtio-fs is a harness that stops working the
# moment somebody submits a configuration without them.
#
# Two rules govern the whole script:
#
#   1. It must always reach `poweroff`. PID 1 exiting is a kernel panic, and a
#      panic here would be indistinguishable from one the patch caused.
#   2. It must never let a failing step stop the run. A module that will not
#      load and a script that exits 1 are both results, so they are recorded
#      and the run continues to the next thing.
#
# Results leave over ttyS1, framed:
#
#   --RIG-BEGIN <name> <rc>
#   <base64>
#   --RIG-END <name>
#
# and finally `--RIG-DONE <rc>`. base64 rather than raw because a test is
# entitled to print anything at all, including something that looks like a
# frame marker; the base64 alphabet has no '-', and every marker starts with
# one, so the framing cannot be forged. nix/kernel-test/collect.py is the other
# half of this.

set -u

CHANNEL=/dev/ttyS1

# Sourced before anything else runs, because it is what puts busybox on PATH
# and there is no `mount` to call until it has. It needs nothing mounted
# itself, so this is safe as the very first thing.
# shellcheck source=/dev/null
. /etc/rig/env

# Already-mounted is not an error: it is what happens when the configuration
# under test has CONFIG_DEVTMPFS_MOUNT, which most do.
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t tmpfs tmpfs /tmp 2>/dev/null || true
mount -t tmpfs tmpfs /run 2>/dev/null || true

# devtmpfs gives the device nodes the kernel knows about and nothing else. On
# an ordinary system udev or systemd adds these four, and a great deal of
# software assumes them without ever saying so: anything that passes a file
# descriptor to a child as /dev/fd/N, which is how bash implements process
# substitution, and every `> /dev/stderr`. Without them those constructs fail
# with ENOENT on a path that looks like it should obviously exist.
ln -sf /proc/self/fd /dev/fd
ln -sf /proc/self/fd/0 /dev/stdin
ln -sf /proc/self/fd/1 /dev/stdout
ln -sf /proc/self/fd/2 /dev/stderr

# Without the result channel the guest would run every phase and report none of
# it, and the job would look like a boot failure rather than a broken image.
if [ ! -c "$CHANNEL" ]; then
	echo "rig: $CHANNEL is missing; results cannot be reported" >/dev/console
	poweroff -f
	while true; do sleep 60; done
fi

RIG_MODULES=""
RIG_TARGETS=""
RIG_SCRIPT_TIMEOUT=300
RIG_HAS_SCRIPT=0
# shellcheck source=/dev/null
[ -f /test/spec.env ] && . /test/spec.env

# The status of the run as a whole: any phase that fails sets it, and no phase
# clears it.
verdict=0

frame() { # frame <name> <rc> <file>
	{
		echo "--RIG-BEGIN $1 $2"
		[ -s "$3" ] && base64 <"$3"
		echo "--RIG-END $1"
	} >>"$CHANNEL"
}

# Runs one phase, captures everything it said, reports it, and returns its
# status. Under `timeout` because a test that hangs should cost its own budget
# rather than the whole VM's — the host kills QEMU well after this fires, so a
# phase that overruns still gets to say so.
phase() { # phase <name> <limit> <command...>
	name=$1
	limit=$2
	shift 2

	echo "--- rig: $name" >/dev/console
	timeout -s KILL "$limit" "$@" >"/tmp/$name.out" 2>&1
	rc=$?
	frame "$name" "$rc" "/tmp/$name.out"
	return $rc
}

# ---------------------------------------------------------------- environment

phase uname 30 uname -a || true

# ------------------------------------------------------------------- modules
#
# Loaded before anything else runs, so a script or a selftest can assume the
# driver under test is present. depmod ran when the guest image was built, so
# modprobe resolves dependencies without further help.

if [ -n "$RIG_MODULES" ]; then
	: >/tmp/modules.out
	modrc=0
	for module in $RIG_MODULES; do
		if modprobe "$module" >>/tmp/modules.out 2>&1; then
			echo "loaded $module" >>/tmp/modules.out
		else
			echo "FAILED to load $module" >>/tmp/modules.out
			modrc=1
		fi
	done
	frame modules "$modrc" /tmp/modules.out
	[ "$modrc" -eq 0 ] || verdict=1
fi

# ----------------------------------------------------------------- kselftest
#
# Copied out of the store first, and made writable, because a great many
# selftests create files beside their own binaries — execveat writes the
# programs it then tries to exec, binfmt_script writes scripts, check-exec
# writes files with particular modes. `cp -a` faithfully preserves the store's
# r-xr-xr-x, and those tests then fail in a way that reads as a kernel fault:
# no output, a non-zero status, and nothing anywhere naming a permission.

if [ -n "$RIG_TARGETS" ] && [ -n "${RIG_KSELFTEST:-}" ] && [ -d "$RIG_KSELFTEST" ]; then
	cp -a "$RIG_KSELFTEST" /tmp/kselftest
	chmod -R u+w /tmp/kselftest
	if [ -x /tmp/kselftest/run_kselftest.sh ]; then
		(cd /tmp/kselftest && phase kselftest "$RIG_SCRIPT_TIMEOUT" ./run_kselftest.sh) || verdict=1
	else
		echo "no run_kselftest.sh was installed" >/tmp/kselftest.out
		frame kselftest 1 /tmp/kselftest.out
		verdict=1
	fi
fi

# -------------------------------------------------------------------- script
#
# The caller's own test. Python because a kernel test is mostly reading sysfs,
# poking a device node and comparing numbers, and doing that in shell is how
# tests come to pass for the wrong reason.

if [ "$RIG_HAS_SCRIPT" = "1" ] && [ -f /test/script.py ]; then
	if [ "${RIG_PYTHON:-0}" = "1" ]; then
		phase script "$RIG_SCRIPT_TIMEOUT" python3 /test/script.py || verdict=1
	else
		echo "the guest image was built without python3" >/tmp/script.out
		frame script 1 /tmp/script.out
		verdict=1
	fi
fi

# ---------------------------------------------------------------- aftermath
#
# Last, so that anything the phases above provoked is in it. The taint word is
# the cheapest question worth asking: a kernel that tainted itself during a
# test did something it is not supposed to have done, whatever the tests said.

phase lsmod 30 lsmod || true
cat /proc/sys/kernel/tainted >/tmp/tainted.out 2>&1 || true
frame tainted 0 /tmp/tainted.out
dmesg >/tmp/dmesg.out 2>&1 || true
frame dmesg 0 /tmp/dmesg.out

echo "--RIG-DONE $verdict" >>"$CHANNEL"

# reboot(2) returns before the machine is gone, and PID 1 falling off the end
# of this script would panic the kernel we just finished testing.
sync
poweroff -f
while true; do sleep 60; done
