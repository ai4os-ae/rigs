# The userspace a fuzzing VM boots into, as one uncompressed cpio archive.
#
# syzkaller's documented guest is a Debian rootfs built by its own
# tools/create-image.sh, which shells out to debootstrap and therefore needs
# root and a network. Neither is available to a rig: builds happen in a Nix
# sandbox on a machine nobody walks up to, and an image built by hand once per
# rig is an image that drifts. So the guest is built the way
# nix/kernel-test/default.nix builds its own — a closure, a cpio, and no
# distribution anywhere — and handed to QEMU through syzkaller's `initrd`
# option rather than as a disk.
#
# WHAT THAT COSTS, because it is a real trade and not a free win. syzkaller's
# descriptions assume a normal machine: some of them want a block device that
# is not a loop device, some want a distribution's shape. An initramfs guest
# reaches less of that surface than a Debian image on an emulated disk does.
# What it buys is that the image is a derivation — pure, cached, identical on
# every rig, rebuilt automatically when the kernel's modules change, and
# pristine on every one of the many hundreds of boots a fuzzing run performs.
#
# WHY IT IS SO MUCH LEANER THAN THE TEST GUEST. Every megabyte here is unpacked
# into a tmpfs inside every VM, and a rig runs several VMs at once, each of
# which must also hold syz-executor's data segment — see the memory sizing in
# nix/kernel-fuzz/default.nix. The test guest carries python3, coreutils and a
# selftest tree because a test script needs them. Nothing here does: syzkaller
# copies its own statically linked binaries in over SSH, and everything else
# that happens in this VM happens inside those binaries.
{
  lib,
  runCommand,
  writeText,
  closureInfo,
  busybox,
  cpio,
  kmod,
  openssh,
}:

{
  # A normalised kernel image directory: bzImage, kernel-release, and a
  # depmod'd lib/modules tree. nix/kernel-test's mkImage produces exactly this,
  # and is reused rather than reimplemented.
  image,
  # The public half of the key syz-manager will authenticate with. A string,
  # supplied by the daemon, so that no private key has to live in this
  # repository — see modules/rig/buildd.nix.
  authorizedKey,
  # Modules to load before sshd starts, for a driver under test configured =m.
  modules ? [ ],
  name ? "linux",
}:
let
  # Order is load-bearing, and for the same reason it is in nix/kernel-test:
  # this is the order /bin is populated in, and the first provider of a name
  # wins.
  #
  # kmod ahead of busybox because busybox has its own modprobe and it is not
  # the one that reads the modules.dep that depmod just wrote — busybox's would
  # load the named module and none of its dependencies.
  #
  # openssh ahead of busybox for `ssh-keygen`, which busybox does not have at
  # all, and to make sure nothing shadows `sshd`.
  #
  # Unlike the test guest there is no coreutils here: nothing in a fuzzing VM
  # copies a binary under another name and execs it, which is the reason that
  # guest cannot use busybox's multi-call applets.
  tools = [
    kmod
    openssh
    busybox
  ];

  # A root of the closure, not a directory to copy. sshd is dynamically linked
  # and does not link only against libc — copying the binaries in by hand
  # brings none of what they need, and the guest then fails to start sshd with
  # an error naming no library at all.
  closure = closureInfo { rootPaths = tools; };

  # sshd's whole configuration, and every line of it is either something
  # syz-manager requires or something a machine with no persistent state
  # cannot supply.
  sshdConfig = writeText "sshd_config" ''
    Port 22
    ListenAddress 0.0.0.0
    HostKey /etc/ssh/ssh_host_ed25519_key

    # syz-manager authenticates as root with a key and nothing else. There is
    # no password on this machine because there is no account anybody could
    # use one on, and it is reachable only through a loopback-bound QEMU port
    # forward on the rig itself.
    PermitRootLogin yes
    PubkeyAuthentication yes
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    AuthorizedKeysFile /root/.ssh/authorized_keys

    # An initramfs unpacks with the uids recorded in the archive, and the
    # archive was written by a Nix build user rather than by root. guest-init
    # chowns /root before sshd starts, so this is belt and braces — but a VM
    # that boots and then refuses the key is indistinguishable from a VM that
    # never booted, and that is an expensive thing to debug from a fuzzing log.
    StrictModes no

    UsePAM no
    PrintMotd no
    # Nothing resolves anything here: there is no DNS, and the reverse lookup
    # syzkaller's connection would otherwise trigger costs a timeout per SSH
    # session on a machine that opens a great many of them.
    UseDNS no

    # syz-manager copies syz-executor in with scp, and scp has spoken the SFTP
    # protocol rather than the legacy one since OpenSSH 9. Without this
    # subsystem every VM boots, answers SSH, and then fails to receive the
    # fuzzer.
    Subsystem sftp ${openssh}/libexec/sftp-server
  '';

  # sshd still separates privileges, and still insists the unprivileged user
  # exists before it will start. root is here because there has to be a shell
  # and a home directory for the account syz-manager logs in as.
  passwd = writeText "passwd" ''
    root:x:0:0:root:/root:/bin/sh
    sshd:x:74:74:Privilege-separated SSH:/var/empty:/bin/false
  '';

  group = writeText "group" ''
    root:x:0:
    sshd:x:74:
  '';

  authorizedKeys = writeText "authorized_keys" (lib.removeSuffix "\n" authorizedKey + "\n");
in
runCommand "${name}-fuzz-guest" { nativeBuildInputs = [ cpio ]; } ''
  root=$NIX_BUILD_TOP/root
  # sys/kernel/debug and sys/kernel/security are mount points the kernel
  # creates itself once sysfs is up, but sys/fs/cgroup is not always one, and a
  # mount onto a directory that does not exist fails silently past
  # guest-init.sh's redirect.
  mkdir -p $root/nix/store $root/proc $root/sys/fs/cgroup $root/dev $root/tmp \
           $root/run $root/var/empty $root/root $root/etc/ssh $root/etc/rig $root/bin

  while read -r path; do
    cp -a --reflink=auto "$path" $root/nix/store/
  done < ${closure}/store-paths

  # modprobe looks under /lib/modules, and so does anybody who ends up in this
  # VM by hand trying to work out why a driver was never reached.
  cp -a ${image}/lib $root/lib

  install -Dm644 ${sshdConfig} $root/etc/ssh/sshd_config
  install -Dm644 ${passwd} $root/etc/passwd
  install -Dm644 ${group} $root/etc/group
  install -Dm600 ${authorizedKeys} $root/etc/rig/authorized_keys

  # Sourced by /init, which has no profile and no login shell to get a PATH
  # from. This is the one place the guest is told where anything is.
  #
  # RIG_SSHD is an absolute store path and both halves of that matter. sshd
  # refuses to start when argv[0] is not absolute — it re-executes itself to
  # handle a connection, so it has to know its own path — and fails with
  # "sshd requires execution with an absolute path", after which the VM
  # listens on nothing and syz-manager reports it as a boot failure. And it
  # must be the store path rather than /bin/sshd because sshd derives where its
  # helpers live from where it is.
  {
    echo "export PATH=${lib.makeBinPath tools}"
    echo "export RIG_SSHD=${openssh}/bin/sshd"
    echo "export RIG_MODULES=${lib.escapeShellArg (lib.concatStringsSep " " modules)}"
  } > $root/etc/rig/env

  install -Dm755 ${./guest-init.sh} $root/init

  # /bin has to be populated, not merely present, and it is not about
  # convenience: a path is not a PATH lookup. Every "#!/bin/sh" is resolved by
  # the kernel, which does not read PATH, and sshd's own default shell for root
  # is spelled as a path. None of this shadows a PATH lookup, because /bin is
  # deliberately not on PATH.
  for dir in ${lib.concatStringsSep " " (map (tool: "${tool}/bin") tools)}; do
    [ -d "$dir" ] || continue
    for program in "$dir"/*; do
      [ -e "$root/bin/$(basename "$program")" ] ||
        ln -s "$program" "$root/bin/$(basename "$program")"
    done
  done

  # busybox's applets exist only as names it answers to, so they are not in its
  # bin/ and have to be asked for. Last, so nothing already provided by a real
  # program is replaced by a multi-call stand-in.
  for applet in $(${busybox}/bin/busybox --list); do
    [ -e "$root/bin/$applet" ] || ln -s ${busybox}/bin/busybox "$root/bin/$applet"
  done

  # sshd resolves root's shell through /etc/passwd, and several things reach
  # for /usr/bin by name.
  mkdir -p $root/usr
  ln -s /bin $root/usr/bin

  # Uncompressed, deliberately. A compressed initramfs would make the guest's
  # decompressor part of this harness's requirements — RD_GZIP or RD_ZSTD in a
  # configuration the caller chose and we do not control — and buy nothing: the
  # archive is read out of the host's page cache, not off a wire.
  mkdir -p $out
  ( cd $root && find . -mindepth 1 -print0 | LC_ALL=C sort -z |
      cpio --null --create --format=newc --reproducible ) > $out/initrd
''
