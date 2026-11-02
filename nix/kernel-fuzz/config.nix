# What a kernel must be configured with before syzkaller can fuzz it.
#
# The tiers, and the reasons in them, come from syz-checkconfig — a script that
# was written by working backwards from fuzzers that appeared to run and were
# not actually finding anything. Almost nothing here fails loudly. A kernel
# missing KCOV fuzzes at random forever; one with SERIAL_8250_CONSOLE=m boots
# silently and crash detection is blind; one built with DEBUG_INFO_NONE reports
# coverage as raw addresses. All four look like a working campaign from the
# outside.
#
# Three tiers, and the difference between them is who wins:
#
#   forced   the caller cannot override it, because a job that overrode it
#            would spend half an hour building and four hours running and
#            learn nothing. `kernel-build` forces MODULES=y for the same
#            reason and in the same way.
#   advised  a default the caller may override, because there are legitimate
#            reasons to want a kernel without KASAN (it is roughly a doubling
#            of build time and image size) and the caller knows theirs.
#   banned   forced off. These are all real debug features and all actively
#            hostile to fuzzing.
#
# Nothing here needs enforcement machinery: `kernel-build`'s configure-verify
# step re-reads the resulting .config and fails a build that lost a requested
# symbol, which is exactly the guard a KASAN-plus-KCOV configuration wants —
# these symbols have dependencies, and a silently dropped one is a fuzzer with
# no feedback.
#
# THE GUEST IS AN INITRAMFS, and that removes a whole tier of requirements.
# syzkaller's usual guest is a Debian image on an emulated IDE disk, so its
# documented config list wants ATA, ATA_PIIX, SCSI, BLK_DEV_SD and EXT4_FS to
# find a root filesystem, and BINFMT_MISC because Debian's systemd drops to an
# emergency shell when that mount fails. nix/kernel-fuzz/guest.nix hands the
# kernel its entire userspace in an initramfs instead, so none of those are
# needed and none are demanded here. What survives is what a VM needs to reach
# userspace, get an address, and be reachable over SSH.
{ lib }:
let
  # syzkaller cannot fuzz at all without these.
  required = {
    # Coverage feedback is the whole mechanism. Without it syzkaller degrades
    # to random syscall generation and never reports that it has.
    KCOV = "y";
    # Without INSTRUMENT_ALL, coverage is restricted to a handful of files
    # chosen at build time — which is emphatically not the same thing as a
    # focus area, and cannot be changed per job.
    KCOV_INSTRUMENT_ALL = "y";
    # syz-executor reaches KCOV through debugfs.
    DEBUG_FS = "y";
    # syz-manager symbolises coverage PCs out of vmlinux's DWARF. The usual
    # reason coverage comes back empty is CONFIG_DEBUG_INFO_NONE=y, which is
    # what this displaces.
    DEBUG_INFO_DWARF_TOOLCHAIN_DEFAULT = "y";
  };

  # The VM will not boot, or syz-manager will not get into it. Each of these
  # must be =y and not =m: there is no second initramfs to load a module from
  # before it is needed.
  boot = {
    # syzkaller's amd64 arch config hardcodes `-device e1000` and builds the
    # netdev argument itself. No NIC driver means no address, no SSH, and a
    # pool of VMs that boot and are then declared lost.
    E1000 = "y";
    # syz-manager reads each VM's serial console to detect oopses. As a module
    # this is useless — nothing can load it before the console is needed — so
    # the boot is silent and crash detection is blind. CONFIG_EARLY_PRINTK
    # plus an earlyprintk= argument is NOT a substitute: it stops the moment
    # the real console would take over.
    SERIAL_8250 = "y";
    SERIAL_8250_CONSOLE = "y";
    NET_CORE = "y";
    INET = "y";
    # AF_UNIX. sshd wants it, and a large share of the syscall descriptions do
    # too.
    UNIX = "y";
    # The guest's /dev, and the automatic mount of it. guest-init.sh mounts
    # devtmpfs itself as a fallback, but the kernel doing it means the console
    # exists before PID 1 does, which is the difference between a diagnosable
    # early failure and a blank screen.
    DEVTMPFS = "y";
    DEVTMPFS_MOUNT = "y";
    # Running any userspace binary at all.
    BINFMT_ELF = "y";
    # Shebang scripts. Without it every "#!" fails with "Exec format error",
    # which surfaces as a guest that boots and then does nothing.
    BINFMT_SCRIPT = "y";
    # The initramfs the guest *is*.
    BLK_DEV_INITRD = "y";
  };

  # Defaults, overridable. You will find meaningfully fewer bugs without these,
  # but there are real reasons to turn one off.
  advised = {
    # The single highest-yield bug detector for kernel fuzzing, and the reason
    # a fuzz kernel is roughly twice the size and twice the build time of a
    # test kernel.
    KASAN = "y";
    # Comparison-based input mutation: syzkaller learns the magic numbers a
    # driver checks for rather than guessing them.
    KCOV_ENABLE_COMPARISONS = "y";
    # Error paths are where the bugs are, and are otherwise nearly unreachable
    # from userspace: a fuzzer cannot ask an allocator to fail.
    #
    # THREE SEPARATE THINGS ARE NEEDED and missing any of them produces the
    # identical symptom — "Fault: failed to write fault injection file" in
    # syzkaller's feature probe, and a run that is otherwise entirely healthy.
    #
    #   FAULT_INJECTION           the machinery
    #   FAULT_INJECTION_DEBUG_FS  the controls, under /sys/kernel/debug
    #   FAIL*/FAILSLAB            the injection *sites* themselves
    #
    # The last is the one that is easy to miss, because the first two look like
    # the whole feature. Without a site there is simply nothing under
    # /sys/kernel/debug to write to. (guest-init.sh mounting debugfs is a
    # fourth requirement, and has the same symptom again.)
    FAULT_INJECTION = "y";
    FAULT_INJECTION_DEBUG_FS = "y";
    FAILSLAB = "y";
    FAIL_PAGE_ALLOC = "y";
    FAIL_MAKE_REQUEST = "y";
    FAIL_IO_TIMEOUT = "y";
    FAIL_FUTEX = "y";
    FAULT_INJECTION_USERCOPY = "y";
    # A large share of the syscall descriptions do nothing useful without
    # namespaces, and syzkaller's sandbox=namespace mode needs USER_NS.
    NAMESPACES = "y";
    USER_NS = "y";
    # /dev/net/tun, which gates syzkaller's NetInjection feature. Without it
    # the feature probe reports "tun: can't open /dev/net/tun" and the whole
    # packet-injection half of the network descriptions goes unused — a large
    # fraction of the surface for a symbol that costs nothing.
    TUN = "y";
    # Somewhere to put a process. Several descriptions want a hierarchy, and
    # guest-init.sh mounts cgroup2 when this is present.
    CGROUPS = "y";
    # AF_PACKET and IPv6 between them carry most of the network surface.
    PACKET = "y";
    IPV6 = "y";
    # Named by several subsystems' descriptions.
    CONFIGFS_FS = "y";
    SECURITYFS = "y";
    # syz_mount_image$* builds a filesystem image in memory and mounts it
    # through a loop device. On an initramfs guest this is the only way the
    # filesystem descriptions reach anything at all.
    BLK_DEV_LOOP = "y";
  };

  # Legitimate debug features, all of them hostile here.
  banned = {
    # Probes, removes and re-probes every driver at boot. Under syzkaller's
    # own guest that tears down the root disk mid-boot; here it is merely a
    # reliable way to lose the NIC.
    DEBUG_TEST_DRIVER_REMOVE = "n";
    # Runs the tracer self-tests at every boot, adding minutes per VM — paid
    # once per VM per reboot, and syzkaller reboots VMs constantly.
    FTRACE_STARTUP_TEST = "n";
    # Both flood the serial console that syz-manager parses for crash reports,
    # which costs crash detection accuracy on top of the noise.
    DEBUG_KOBJECT = "n";
    DEBUG_KOBJECT_RELEASE = "n";
  };

  # `scripts/config` names symbols without the CONFIG_ prefix and adds it back
  # itself. kernel-build normalises either spelling, but a caller's overrides
  # arrive here first and have to be comparable to ours before they are merged.
  stripPrefix = lib.removePrefix "CONFIG_";
  normalise = lib.mapAttrs' (
    name: value: {
      name = stripPrefix name;
      inherit value;
    }
  );
in
{
  inherit
    required
    boot
    advised
    banned
    ;

  # The configSet to hand `kernelBuild.build`, given whatever the caller asked
  # for. The merge order is the policy: advised is a floor the caller stands
  # on, and required/boot/banned are a ceiling they cannot raise.
  configSetFor = configSet: advised // normalise configSet // required // boot // banned;

  # Every symbol this file has an opinion about, for the daemon to report and
  # for `syz-checkconfig`-style diagnostics. Useful because "which of these did
  # my job actually get" is the first question when a run finds nothing.
  opinionatedSymbols = lib.attrNames (advised // required // boot // banned);
}
