# Boot a kernel that ./kernel-build produced inside a QEMU virtual machine,
# run something in it, and hand back what happened.
#
# A test is a derivation, like a build, for the same reason: it is a pure
# function of a kernel, a script and a set of knobs, so the store answers the
# second identical request without booting anything. `nix/kernel-build` has the
# long version of why that matters.
#
# WHY A VM AND NOT A CONTAINER. The thing under test *is the kernel*. A
# container shares the rig's kernel and would test nothing; the rig's own
# kernel is also the one holding the mesh, the daemon and everybody's SSH
# session up, and a candidate patch is exactly the sort of code that panics.
# The VM is the isolation boundary and it is also the point.
#
# HOW ANYTHING GETS OUT. The guest has no network — the Nix sandbox has none to
# give it — and no shared filesystem, because a shared filesystem would mean
# requiring 9p or virtio-fs to be in the configuration under test, which is the
# caller's to choose and not ours to demand. What is left is a serial port,
# which is already there: the console has to work or nothing is debuggable
# anyway.
#
# So there are two of them. ttyS0 is the console, kernel messages and all,
# streamed to the build log as it happens — this is what `GET
# /v1/jobs/{id}/log?follow=1` shows a caller. ttyS1 carries results, framed and
# base64'd, and is parsed afterwards by ./collect.py. Two ports rather than one
# because the kernel writes to the console whenever it likes, and a printk
# landing in the middle of a result is a corrupt result.
#
# NOTHING HERE IS KNOWN AT EVALUATION TIME. A rig evaluates with
# allow-import-from-derivation off, and the kernel release string only exists
# once the kernel has been built, so every derivation below reads it out of
# ${kernel}/kernel-release in its own shell rather than taking it as an
# argument. The same goes for which modules exist. This is why `run` needs
# nothing but a kernel.
#
# WHAT COUNTS AS A FAILURE. Same rule as a build: a kernel that panics is a
# successful *job* — the caller asked what this patch does and found out — so
# the derivation exits 0 and `passed` in report.json is false. A derivation
# that fails means QEMU or the harness broke and the caller learned nothing.
{
  lib,
  stdenv,
  bash,
  busybox,
  closureInfo,
  coreutils,
  cpio,
  kmod,
  python3,
  qemu_kvm,
  runCommand,
  writeText,
  writeTextFile,

  # The kernel's own build system, again: building tools/testing/selftests runs
  # `make headers` in the source tree first, so this needs what a kernel build
  # needs rather than what an ordinary C program needs.
  bc,
  bison,
  elfutils,
  flex,
  openssl,
  perl,
  rsync,

  # And these are what the selftests themselves want, discovered the only way
  # this is ever discovered — by building them and reading the link errors.
  # `glibc.static` is the surprising one: several targets link -static to test
  # what the kernel does with a static binary, which needs libc.a and not just
  # libc.so.
  glibc,
  libcap,
  pkg-config,
}:
let
  # Shared with nix/kernel-build: same JSON, same report.json schema, so a test
  # job and a build job are read by the same client code. Reached by a relative
  # path, which is why the daemon is handed one copy of nix/ rather than each
  # directory separately — see modules/rig/buildd.nix.
  reportPy = ../kernel-build/report.py;
  collectPy = ./collect.py;
  guestInit = ./guest-init.sh;

  # A leaner cousin of kernel-build's preamble. Deliberately not shared: that
  # one also pins KBUILD_BUILD_* and guards the fixed build path, neither of
  # which means anything here, and importing it for twenty lines of shell would
  # couple two derivations that have no other reason to know about each other.
  #
  # `record` latches: the first failure makes every later step skip itself, so
  # the report says where things stopped. `attempt` does not, and returns the
  # status, for the one case that wants to keep going — building selftest
  # targets, where one target that will not compile should not cost the rest.
  recorder = ''
    logs="$NIX_BUILD_TOP/logs"
    records="$NIX_BUILD_TOP/records.jsonl"
    mkdir -p "$logs"

    attempt() {
      local name="$1"; shift

      echo "--- $name: $*"
      local rc=0
      set +e
      "$@" 2>&1 | tee "$logs/$name.log"
      rc=''${PIPESTATUS[0]}
      set -e

      python3 ${reportPy} add-command "$records" "$name" "$rc" "logs/$name.log" "$@"
      return "$rc"
    }

    record() {
      local name="$1"

      if [ -n "''${buildFailed:-}" ]; then
        echo "--- skipping $name (an earlier step failed)"
        return 0
      fi

      if ! attempt "$@"; then
        echo "--- $name failed" >&2
        buildFailed=1
      fi
      return 0
    }
  '';

  selftestToolchain = [
    bc
    bison
    elfutils
    flex
    libcap
    openssl
    perl
    pkg-config
    python3
    rsync
  ];

  # The default set is small, and it is small on purpose.
  #
  # One rule decided it: a default that goes red on a pristine upstream kernel
  # is worse than no default at all, because the first thing everybody learns
  # is to disregard the result. These are the collections that were built and
  # observed to pass, every subtest, on a plain x86_64 defconfig at v6.18.
  #
  # What is *not* here, and why, because each looks like an obvious addition:
  #
  #   prctl   set-anon-vma-name needs CONFIG_ANON_VMA_NAME
  #   splice  short_splice_read needs CONFIG_TEST_LKM's test_module
  #   memfd   its fuse tests need CONFIG_FUSE_FS, and libfuse to build at all
  #   ptrace  vmaccess's second half times out under a two-processor guest
  #
  # The first three are the same story: kselftest reports a missing
  # configuration symbol as a *failure*, not a skip, so a target whose symbols
  # defconfig does not set will always fail. That is the caller's to fix by
  # asking for the symbols in config.set alongside the target, which is why
  # naming a target is a deliberate act and not a default.
  defaultTargets = [
    "exec"
    "kcmp"
    "size"
  ];

  targetRE = "[a-z0-9_][a-z0-9_/-]*";

  # glibc's shared and static halves in one directory, because the selftest
  # tree needs both and the linker searches -L directories rather than picking
  # per link.
  #
  # Several targets link -static, to test what the kernel does with a static
  # binary, and that needs libc.a — which lives in glibc's `static` output and
  # nowhere else. Putting that output on the link path by itself is the obvious
  # move and it is wrong in a way that takes a while to see: the directory
  # holds libc.a and no libc.so, so an ordinary *dynamic* link resolves -lc to
  # the static archive too. The binary then carries its own memset, libgcc_s
  # references memset as an IFUNC, and every dynamically linked selftest dies
  # at exec with a circular-dependency message naming neither libc nor the
  # linker flag that caused it. From the outside it looks like the whole guest
  # image is broken.
  #
  # One directory holding both means -lc finds libc.so, -static finds libc.a,
  # and neither depends on what order anything was added in.
  glibcBoth = runCommand "glibc-shared-and-static" { } ''
    mkdir -p $out/lib
    ln -s ${glibc.out}/lib/* $out/lib/
    for archive in ${glibc.static}/lib/*.a; do
      [ -e "$out/lib/$(basename "$archive")" ] || ln -s "$archive" $out/lib/
    done
  '';

  # Both stages of kernel-build are valid input here and they do not have the
  # same shape. The patched stage installs bzImage and a modules tree because
  # that is what a caller is handed; the base stage keeps source/ and obj/,
  # because what a caller is handed is a *patched* build and the base is an
  # implementation detail of how it got there.
  #
  # Rather than teach everything downstream about both, normalise once. The
  # base branch is `modules_install` reduced to what it actually does for us:
  # collect the .ko files under the path they were built at and run depmod over
  # them. Doing it with make instead would mean copying an 18GB object tree out
  # of the store to give make somewhere writable to work.
  mkImage =
    { kernel, name }:
    runCommand "${name}-image" { nativeBuildInputs = [ kmod ]; } ''
      mkdir -p $out
      cp ${kernel}/kernel-release $out/kernel-release
      release=$(cat $out/kernel-release)
      modules=$out/lib/modules/$release
      mkdir -p "$modules"

      if [ -f ${kernel}/bzImage ]; then
        # The patched stage: everything is already where it should be.
        cp ${kernel}/bzImage $out/bzImage
        if [ -d ${kernel}/modules/lib/modules/$release ]; then
          cp -a ${kernel}/modules/lib/modules/$release/. "$modules"/
          chmod -R u+w "$modules"
        fi
      else
        # The base stage: bzImage and the modules are in the object tree, laid
        # out the way kbuild left them.
        cp ${kernel}/obj/arch/x86/boot/bzImage $out/bzImage

        ( cd ${kernel}/obj && find . -name '*.ko' -print ) | while read -r ko; do
          install -Dm444 "${kernel}/obj/$ko" "$modules/kernel/''${ko#./}"
        done

        # depmod needs these to know what is built in; without them every
        # module looks like it depends on symbols nothing provides.
        for f in modules.builtin modules.builtin.modinfo modules.order System.map; do
          if [ -f ${kernel}/obj/$f ]; then cp ${kernel}/obj/$f "$modules/$f"; fi
        done

        depmod -b $out "$release" || echo "depmod found nothing to do"
      fi
    '';

  # The kernel's own tests, built from the source tree the kernel was compiled
  # from and installed as an ordinary directory of programs.
  #
  # Built from the *base* source, deliberately. That keeps this cached across
  # every candidate patch against a commit — which is the workload — at the
  # cost of not seeing a patch that edits tools/testing/selftests itself. That
  # is a real limitation and it is the right trade: patches under test here are
  # driver code, and paying a selftest rebuild per candidate would undo the
  # entire reason the kernel build is split in two.
  mkSelftests =
    {
      source,
      targets,
      name,
    }:
    stdenv.mkDerivation {
      inherit name;

      nativeBuildInputs = selftestToolchain;

      dontUnpack = true;
      dontConfigure = true;
      dontFixup = true;
      hardeningDisable = [ "all" ];
      enableParallelBuilding = true;

      buildPhase = ''
        runHook preBuild
        ${recorder}

        # Writable, because the selftest build runs `make headers` in the tree
        # and drops objects beside the sources.
        cp -a --reflink=auto ${source} /build/linux
        chmod -R u+w /build/linux
        patchShebangs /build/linux/scripts /build/linux/tools

        record headers \
          make -C /build/linux -j"$NIX_BUILD_CORES" headers

        # See glibcBoth. Added after `headers` and not to nativeBuildInputs
        # because `headers` is what builds the kernel's own `fixdep`, and
        # fixdep hits the same IFUNC failure if it is linked with any of this
        # on the path.
        export NIX_LDFLAGS="-L${glibcBoth}/lib ''${NIX_LDFLAGS:-}"

        # One target at a time, and not fatal. The selftest tree is a hundred
        # loosely related programs with their own dependencies; some fraction
        # of them fails to build on any given configuration, and a caller who
        # asked for six targets should get the five that work.
        built=""
        for target in ${lib.escapeShellArgs targets}; do
          if attempt "selftest-$target" \
              make -C /build/linux/tools/testing/selftests \
                TARGETS="$target" -j"$NIX_BUILD_CORES"; then
            built="$built $target"
          else
            echo "--- selftest target $target did not build; continuing without it" >&2
          fi
        done

        mkdir -p $out/kselftest
        if [ -n "$built" ]; then
          # Installed in one call rather than per target: `install` regenerates
          # run_kselftest.sh from the TARGETS of the invocation, so installing
          # one at a time would leave a runner that knows about only the last.
          record selftest-install \
            make -C /build/linux/tools/testing/selftests \
              TARGETS="$built" install INSTALL_PATH=$out/kselftest
        else
          echo "no selftest target built" > $out/kselftest/EMPTY
        fi
        echo "$built" > $out/targets

        # Undo patchShebangs over the installed tree, and only over that.
        #
        # These scripts do not run here. They run inside a VM whose userspace
        # is nix/kernel-test's guest image and which has never heard of this
        # builder's stdenv bash, so a #! rewritten to a store path is a script
        # that dies with ENOENT and a message naming a file that plainly
        # exists. The guest provides /bin/sh and /bin/bash for exactly this.
        #
        # patchShebangs still has to run before the build, because the same
        # tree's *build-time* helpers do run here and there is no /bin/bash in
        # a Nix sandbox either. The two requirements are opposite, which is why
        # this is a second pass rather than an option to the first.
        grep -rIl '^#!/nix/store/' $out/kselftest | while read -r script; do
          sed -i \
            -e '1s|^#!/nix/store/[^[:space:]]*/bin/bash|#!/bin/bash|' \
            -e '1s|^#!/nix/store/[^[:space:]]*/bin/sh|#!/bin/sh|' \
            "$script"
        done

        runHook postBuild
      '';

      # $out is written by buildPhase, because `install` is one of the steps
      # being recorded; this stage only has to close the books.
      installPhase = ''
        runHook preInstall
        cp -a "$logs" $out/logs
        python3 ${reportPy} finalise $out "$records" ${
          writeText "meta.json" (
            builtins.toJSON {
              stage = "selftests";
              inherit targets;
            }
          )
        }
        runHook postInstall
      '';

      meta = {
        description = "kselftest binaries built from a rig kernel's source";
        platforms = [ "x86_64-linux" ];
      };
    };

  # Everything the guest is, as one uncompressed cpio archive.
  #
  # Uncompressed on purpose. A compressed initramfs would make the guest's
  # decompressor part of the harness's requirements — RD_GZIP or RD_ZSTD in a
  # configuration the caller chose and we do not control — and buy nothing: the
  # archive is read out of the host's page cache, not off a wire.
  #
  # Cached per (kernel, selftests, tools), which is what makes a second test
  # against the same kernel start booting immediately.
  mkGuest =
    {
      image,
      selftests,
      withPython,
      name,
    }:
    let
      # kmod ahead of busybox on PATH on purpose: busybox has its own modprobe,
      # and it is not the one that reads the modules.dep depmod just wrote.
      # Order matters, and not only for PATH: it is also the order /bin is
      # populated in, and the first provider of a name wins.
      #
      # coreutils ahead of busybox is the important one, and it is not a
      # preference. busybox is a single multi-call binary that decides what to
      # be from argv[0], and two things here care:
      #
      #   kselftest's runner uses `/usr/bin/stdbuf --output=L` when it finds
      #   it. busybox's stdbuf does not take that flag, so every test in the
      #   run fails instantly with no output — which reads as a kernel that
      #   cannot execute anything.
      #
      #   The exec selftests copy or link a harmless binary (`true`) under
      #   another name and exec it, to see what the kernel does. busybox under
      #   another name says "applet not found" and exits non-zero, so the test
      #   fails on busybox's behaviour rather than the kernel's.
      #
      # busybox stays for what coreutils has never had: mount, poweroff, dmesg.
      tools = [
        kmod
        # Not the default coreutils, which nixpkgs builds as one binary that
        # dispatches on argv[0] — the same multi-call arrangement as busybox
        # and the same problem: a test that copies `true` under another name
        # and execs it gets "unknown program" instead of a working binary.
        # This is the one place where a normal system's shape is the feature.
        #
        # The override means building coreutils rather than fetching it, so its
        # own test suite is turned off: it is minutes of a rig's time spent
        # checking GNU's work rather than the kernel's, and nothing here is
        # testing coreutils.
        ((coreutils.override { singleBinary = false; }).overrideAttrs (_: {
          doCheck = false;
        }))
        bash
        busybox
        # For `getconf`, which is neither's. A selftest and any number of
        # ordinary shell scripts reach for it, and its absence surfaces as a
        # Python traceback about a missing file rather than as anything to do
        # with the guest being small.
        glibc.bin
      ]
      ++ lib.optional withPython python3;

      # The selftests are a root of the closure, not a directory to copy in.
      #
      # They are ordinary dynamically linked programs and they do not link only
      # against libc: several of the exec tests want libcap. Copying the tree
      # in by hand brings the binaries and none of what they need, and the
      # guest then reports every one of them as a failure of the kernel — exit
      # 127, no output, nothing naming the missing library. Letting Nix compute
      # the closure is what makes their dependencies come too, and it means the
      # RUNPATH the linker recorded resolves to the same path it was built as.
      closure = closureInfo {
        rootPaths = tools ++ lib.optional (selftests != null) selftests;
      };
    in
    runCommand "${name}-guest" { nativeBuildInputs = [ cpio ]; } ''
      root=$NIX_BUILD_TOP/root
      mkdir -p $root/nix/store $root/proc $root/sys $root/dev $root/tmp \
               $root/run $root/etc/rig $root/test $root/bin

      while read -r path; do
        cp -a --reflink=auto "$path" $root/nix/store/
      done < ${closure}/store-paths

      # A module's own tooling looks under /lib/modules, and so does anybody
      # who lands in this VM to work out by hand why a test failed.
      cp -a ${image}/lib $root/lib

      # Sourced by /init, which has no profile and no login shell to get a PATH
      # from. Written here rather than as a writeText because it is the one
      # place the guest is told where things are.
      #
      # The selftests are named at their store path rather than copied to a
      # tidier one: that path is what is baked into their RUNPATH, and moving
      # them would be undoing the closure above.
      {
        echo "export PATH=${lib.makeBinPath tools}"
        echo "export RIG_KSELFTEST=${if selftests == null then "" else "${selftests}/kselftest"}"
        echo "export RIG_PYTHON=${if withPython then "1" else "0"}"
      } > $root/etc/rig/env

      install -Dm755 ${guestInit} $root/init

      # /bin has to be populated, not merely present, and this is not about
      # convenience.
      #
      # A path is not a PATH lookup. Every #!/bin/sh in the selftest tree and
      # in a caller's own script is resolved by the kernel, which does not read
      # PATH; and a test that goes looking for /bin/true — several do, to have
      # something harmless to exec — finds nothing on a machine where the only
      # `true` is an applet on a search path. That failure reads as the kernel
      # refusing to execute the file.
      #
      # So: everything on PATH, under its own name, in the order `tools` puts
      # it — see the comment there for why that order is load-bearing. None of
      # this shadows a PATH lookup, because /bin is deliberately not on PATH.
      for dir in ${lib.concatStringsSep " " (map (tool: "${tool}/bin") tools)}; do
        [ -d "$dir" ] || continue
        for program in "$dir"/*; do
          [ -e "$root/bin/$(basename "$program")" ] ||
            ln -s "$program" "$root/bin/$(basename "$program")"
        done
      done

      # busybox's applets exist only as names it answers to, so they are not in
      # its bin/ and have to be asked for. Last, so that nothing already
      # provided by a real program is replaced by a multi-call stand-in.
      for applet in $(${busybox}/bin/busybox --list); do
        [ -e "$root/bin/$applet" ] || ln -s ${busybox}/bin/busybox "$root/bin/$applet"
      done

      # And /usr/bin, for the scripts and shebangs that spell it that way.
      mkdir -p $root/usr
      ln -s /bin $root/usr/bin

      mkdir -p $out
      ( cd $root && find . -mindepth 1 -print0 | LC_ALL=C sort -z |
          cpio --null --create --format=newc --reproducible ) > $out/initrd
    '';

  # Everything that changes from one run to the next, as a second archive.
  #
  # Separate from the guest above so that editing a test script does not
  # rebuild a couple of hundred megabytes of userspace. The kernel unpacks
  # concatenated initramfs archives in order, so appending this one to the
  # guest is the whole mechanism — the same trick early microcode loading uses.
  mkPayload =
    {
      script,
      modules,
      targets,
      scriptTimeout,
    }:
    runCommand "rig-test-payload" { nativeBuildInputs = [ cpio ]; } ''
      root=$NIX_BUILD_TOP/root
      mkdir -p $root/test

      {
        echo "RIG_MODULES=${lib.escapeShellArg (lib.concatStringsSep " " modules)}"
        echo "RIG_TARGETS=${lib.escapeShellArg (lib.concatStringsSep " " targets)}"
        echo "RIG_SCRIPT_TIMEOUT=${toString scriptTimeout}"
        echo "RIG_HAS_SCRIPT=${if script == null then "0" else "1"}"
      } > $root/test/spec.env

      ${lib.optionalString (script != null) ''
        cp ${
          writeTextFile {
            name = "rig-test-script.py";
            text = script;
          }
        } $root/test/script.py
      ''}

      mkdir -p $out
      ( cd $root && find . -mindepth 1 -print0 | LC_ALL=C sort -z |
          cpio --null --create --format=newc --reproducible ) > $out/payload
    '';
in
{
  inherit mkImage mkSelftests mkGuest;

  # The whole test in one call.
  #
  #   kernel     a kernel-build output, either stage.
  #   source     its unpatched source tree, needed only to build selftests.
  #              Derived from `kernel` when it carries the passthru to derive
  #              it from; rig-buildd passes it explicitly, because a job that
  #              names an earlier job's result holds a bare store path with no
  #              passthru on it.
  #   name       what the derivations are called. Cosmetic, but it must be
  #              stable for a given kernel or the guest image is rebuilt for
  #              every test rather than shared between them.
  run =
    {
      kernel,
      source ? null,
      name ? "linux",
      script ? null,
      modules ? [ ],
      kselftest ? true,
      kselftestTargets ? defaultTargets,
      timeoutSeconds ? 900,
      memoryMB ? 4096,
      cpus ? 2,
      failOnWarning ? false,
    }:
    let
      sourceTree =
        if source != null then
          source
        else if kernel ? base then
          "${kernel.base}/source"
        else
          "${kernel}/source";

      targets = if kselftest then kselftestTargets else [ ];

      image = mkImage { inherit kernel name; };

      selftests =
        if targets == [ ] then
          null
        else
          mkSelftests {
            source = sourceTree;
            inherit targets;
            name = "${name}-selftests";
          };

      guest = mkGuest {
        inherit image selftests name;
        # kselftest is not only C programs: several targets are Python, and a
        # guest without an interpreter reports those as failures of the kernel
        # rather than as an image that is missing something.
        withPython = script != null || selftests != null;
      };

      # The guest gets most of the budget but not all of it: the host timeout
      # has to outlast the guest's own, or a script that overruns kills QEMU
      # before the guest can report that it overran, and the caller sees a
      # timeout with no output instead of a failed test with a log.
      scriptTimeout = timeoutSeconds * 3 / 5;

      payload = mkPayload {
        inherit
          script
          modules
          targets
          scriptTimeout
          ;
      };

      metaFile = writeText "meta.json" (
        builtins.toJSON {
          stage = "test";
          kernel_store_path = toString kernel;
          test = {
            has_script = script != null;
            inherit
              modules
              timeoutSeconds
              memoryMB
              cpus
              failOnWarning
              ;
            kselftest_targets = targets;
          };
        }
      );
    in
    assert lib.assertMsg (lib.all (
      t: builtins.match targetRE t != null
    ) kselftestTargets) "kernel-test: kselftest targets must match ${targetRE}";
    assert lib.assertMsg (
      timeoutSeconds > 0 && timeoutSeconds <= 7200
    ) "kernel-test: timeoutSeconds must be between 1 and 7200";
    stdenv.mkDerivation {
      name = "${name}-test";

      dontUnpack = true;
      dontConfigure = true;
      dontFixup = true;

      nativeBuildInputs = [
        qemu_kvm
        python3
      ];

      # Without this the derivation is scheduled onto a builder with no
      # /dev/kvm, QEMU quietly falls back to emulating every instruction, and a
      # ten second boot becomes several minutes. Nix binds /dev/kvm into the
      # sandbox when the feature is present; see modules/rig/buildd.nix.
      requiredSystemFeatures = [ "kvm" ];

      buildPhase = ''
        runHook preBuild
        ${recorder}

        cat ${guest}/initrd ${payload}/payload > $NIX_BUILD_TOP/initrd

        # -no-reboot together with panic=-1 is what turns a panic into an exit
        # rather than a machine that reboots into the same panic until the
        # timeout expires. oops=panic extends that to an oops, which otherwise
        # leaves a wedged guest to be killed by the timeout with no verdict.
        #
        # The order of the two -serial options is the whole contract with the
        # guest: first is ttyS0, second is ttyS1. signal=off keeps QEMU from
        # claiming Ctrl-C on a stdin that is a pipe here anyway.
        #
        # There is no -nic: the sandbox has no network to give the guest, and a
        # test that reached one would not be reproducible anyway.
        record boot \
          timeout --kill-after=10 ${toString timeoutSeconds} \
          qemu-system-x86_64 \
            -nodefaults -no-reboot -display none \
            -machine q35,accel=kvm:tcg -cpu max \
            -smp ${toString cpus} -m ${toString memoryMB} \
            -kernel ${image}/bzImage \
            -initrd $NIX_BUILD_TOP/initrd \
            -append "console=ttyS0,115200 panic=-1 oops=panic loglevel=7" \
            -chardev stdio,id=rigconsole,signal=off \
            -serial chardev:rigconsole \
            -chardev file,id=rigresult,path=$NIX_BUILD_TOP/result.raw \
            -serial chardev:rigresult

        runHook postBuild
      '';

      installPhase = ''
        runHook preInstall

        mkdir -p $out
        cp ${image}/kernel-release $out/kernel-release
        if [ -f "$logs/boot.log" ]; then
          cp "$logs/boot.log" $out/console.log
        else
          echo "the guest produced no console output" > $out/console.log
        fi

        # The result channel exactly as the guest wrote it. Kept because when
        # this harness is what is broken — a frame that never closed, a guest
        # that died mid-sentence — the parsed artifacts are precisely the thing
        # that cannot be trusted to show it.
        cp $NIX_BUILD_TOP/result.raw $out/channel.raw 2>/dev/null || true

        # Deliberately outside `record`: a run that timed out or panicked has
        # already set buildFailed, and its partial output is the most valuable
        # thing in the whole job. Extract first, judge second.
        python3 ${collectPy} \
          --raw $NIX_BUILD_TOP/result.raw \
          --console $out/console.log \
          --out $out \
          --kselftest-requested ${lib.escapeShellArg (lib.concatStringsSep " " targets)} \
          ${lib.optionalString (selftests != null) "--kselftest-built ${selftests}/targets"} \
          ${lib.optionalString failOnWarning "--fail-on-warning"} \
          || echo "--- collecting results failed; see result.json" >&2

        # The verdict, as a recorded command, so that report.json's `passed`
        # comes out of the same rule builds use — every recorded step passed —
        # with no special case anywhere downstream.
        record verdict \
          python3 ${collectPy} --verdict $out/result.json

        cp -a "$logs" $out/logs
        python3 ${reportPy} finalise $out "$records" ${metaFile} --artifacts

        runHook postInstall
      '';

      passthru = {
        inherit
          kernel
          image
          guest
          selftests
          ;
      };

      meta = {
        description = "A boot test of a rig-built kernel under QEMU";
        platforms = [ "x86_64-linux" ];
      };
    };
}
