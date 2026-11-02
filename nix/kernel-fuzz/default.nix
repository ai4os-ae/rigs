# Everything a syzkaller run needs, as one store path.
#
# A build is a derivation and a test is a derivation, because both are pure
# functions of their inputs and the store can therefore answer the second
# identical request without doing the work. A fuzzing run is not one of those.
# It is a long-lived process whose output depends on how long it ran and how
# lucky it was, and whose corpus has to outlive it. `pkgs/rig-buildd/job.go`
# says as much, and it is why this file stops where it does.
#
# So the split is: everything that *is* a pure function stays a derivation, and
# `env` below gathers all of it — the kernel, the guest, the fuzzer itself —
# into a single output. rig-buildd builds that once, registers one GC root over
# it, writes a syzkaller.cfg pointing into it, and supervises syz-manager
# itself. Nothing in this file knows that syz-manager exists.
#
# The other reason for one output rather than several: a run holds its inputs
# for hours, and a rig garbage-collects on a timer (`nix.settings.min-free` in
# modules/rig/buildd.nix). One GC root is one thing that cannot be got wrong.
{
  lib,
  callPackage,
  runCommand,
  writeText,
  syzkaller,
}:
let
  fuzzConfig = import ./config.nix { inherit lib; };

  # Reused rather than reimplemented. mkImage already knows that a base build
  # and a patched build do not have the same shape — the patched stage installs
  # a bzImage and a modules tree, the base stage keeps source/ and obj/ — and
  # normalising that in two places is how the two drift apart.
  kernelTest = callPackage ../kernel-test { };

  mkGuest = callPackage ./guest.nix { };

  # Where nix/kernel-build compiles every kernel. It is a fixed path on purpose
  # — see the header comment there — and that turns out to be exactly what
  # syzkaller's `kernel_build_src` wants: the DWARF in vmlinux records
  # /build/linux/..., the source is in the store somewhere else entirely, and
  # this is the string that lets syz-manager rewrite one to the other. Without
  # it, coverage and crash reports name files that do not exist and every
  # report has to be read with a mental substitution applied.
  kernelBuildSrc = "/build/linux";
in
{
  inherit (fuzzConfig)
    required
    boot
    advised
    banned
    configSetFor
    opinionatedSymbols
    ;

  inherit mkGuest;

  # env gathers a run's inputs.
  #
  #   kernel         a kernel-build output, either stage.
  #   source         its unpacked source tree, for symbolisation. Derived from
  #                  `kernel` when it carries the passthru to derive it from;
  #                  rig-buildd passes it explicitly, because a job naming an
  #                  earlier job's result holds a bare store path with no
  #                  passthru on it. Same arrangement as kernel-test's `run`.
  #   authorizedKey  the public half of the key syz-manager will use.
  #   modules        loaded in the guest before sshd starts, for a driver
  #                  under test that was configured =m.
  env =
    {
      kernel,
      source ? null,
      authorizedKey,
      modules ? [ ],
      name ? "linux",
    }:
    let
      sourceTree =
        if source != null then
          source
        else if kernel ? base then
          "${kernel.base}/source"
        else
          "${kernel}/source";

      image = kernelTest.mkImage { inherit kernel name; };

      guest = mkGuest {
        inherit
          image
          authorizedKey
          modules
          name
          ;
      };

      # Paths inside $out are relative, because a derivation cannot refer to
      # its own output path at evaluation time. Paths outside it are absolute.
      # rig-buildd joins the former against the store path it got back from
      # `nix build`.
      manifest = writeText "fuzz-env.json" (
        builtins.toJSON {
          kernel = "bzImage";
          initrd = "initrd";
          kernel_obj = "obj";
          image = "empty.img";

          kernel_src = sourceTree;
          kernel_build_src = kernelBuildSrc;
          syzkaller = "${syzkaller}";
        }
      );
    in
    runCommand "${name}-fuzz-env" { } ''
      mkdir -p $out/obj

      ln -s ${image}/bzImage $out/bzImage
      ln -s ${guest}/initrd $out/initrd
      cp ${image}/kernel-release $out/kernel-release
      cp ${manifest} $out/env.json

      # syzkaller symbolises coverage and crash reports out of vmlinux's DWARF,
      # and finds it by looking for `vmlinux` inside `kernel_obj`. The two
      # build stages keep it in different places, and it is a gigabyte or more
      # with KASAN and full debug info — so this is a symlink, not a copy. The
      # link is a store reference either way, which is what keeps the kernel
      # alive for as long as this environment is a GC root.
      if [ -f ${kernel}/vmlinux ]; then
        ln -s ${kernel}/vmlinux $out/obj/vmlinux
      else
        ln -s ${kernel}/obj/vmlinux $out/obj/vmlinux
      fi

      # The configuration the kernel was actually built with, for the record
      # and for anybody asking why a focus area was never reached.
      if [ -f ${kernel}/config ]; then
        ln -s ${kernel}/config $out/config
      elif [ -f ${kernel}/obj/.config ]; then
        ln -s ${kernel}/obj/.config $out/config
      fi

      # A zero-byte file, and it has to exist.
      #
      # This guest is an initramfs; there is no disk. But syzkaller's qemu
      # backend validates that `image` names an existing file before it will
      # start, so leaving the field empty fails at manager startup with
      # "image file ... does not exist" rather than doing what it looks like it
      # should. Upstream's sanctioned way out is exactly this: it stats the
      # image when booting a VM and, finding it zero bytes, drops it and adds
      # no -drive at all. See the size check in vm/qemu/qemu.go.
      : > $out/empty.img
    '';
}
