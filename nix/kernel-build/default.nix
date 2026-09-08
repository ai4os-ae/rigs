# Parameterised Linux kernel builds, in two stages.
#
# The workload this exists for is many different candidate patches against the
# *same* kernel commit — one driver's source changes, everything else does not.
# Building each of those from scratch is around half an hour, almost all of it
# recompiling files no patch touched. So a build is split in two:
#
#   linux-base-<commit>-<confighash>     full build; once per (commit, config)
#           ├── linux-<commit>-<patchhash>   only the touched files, then relink
#           └── linux-<commit>-<patchhash>
#
# The second stage takes the first stage's finished build tree as an input,
# applies the patch, and re-runs make. Kbuild rebuilds only what the patch
# touched: everything arrives from the store with an mtime of 1970, `patch(1)`
# sets the files it edits to now, and make's ordinary prerequisite comparison
# does the rest.
#
# THE PART THAT IS EASY TO BREAK. Kbuild records *absolute* paths in the .cmd
# dependency files it writes next to every object. If the second stage unpacks
# the tree at a different path than the first stage built it at, every one of
# those recorded dependencies is stale, make rebuilds the entire tree, and the
# split silently buys nothing at all — no error, just a slow build. So both
# stages build at the same fixed path, /build/linux and /build/obj, which is
# deterministic because it is the Nix sandbox's own build root. The guard in
# `preamble` below refuses to build outside a sandbox for exactly this reason.
#
# The other consequence of that fixed path is a copy: the second stage has to
# get the base tree into /build before it can touch it. For x86_64 defconfig at
# v6.18 that is about 2.4GB (1.5GB source, 0.9GB objects). On the rigs /nix is
# btrfs with compression, so `cp --reflink=auto` makes that a metadata
# operation rather than 2.4GB of writes — provided Nix's build-dir is on that
# same filesystem, which is what `rigs.buildd` configures and why it does.
#
# Measured on x86_64 defconfig, v6.18: base 5m56s, patched 36s, and a patched
# build recompiles one source file. Numbers from a 64-thread builder; the
# ratio is what matters, not the absolute figures.
#
# Build failure is data, not an error. A candidate patch that does not compile
# is a result the caller asked for, and a derivation that fails produces
# nothing to hand back — no logs, no report, and a fresh rebuild every time the
# same doomed patch is submitted again. So make's exit status is captured
# rather than propagated, and both stages exit 0 with `passed` recorded in
# report.json. A derivation failure here means the infrastructure broke.
{
  lib,
  stdenv,
  bc,
  bison,
  cpio,
  elfutils,
  flex,
  gmp,
  kmod,
  libmpc,
  mpfr,
  ncurses,
  openssl,
  pahole,
  perl,
  python3,
  rsync,
  writeText,
  zstd,
}:
let
  reportPy = ./report.py;

  # Everything the kernel's own build system shells out to. stdenv brings the
  # compiler, make, coreutils and gawk; these are the rest.
  kernelToolchain = [
    bc # kernel/time and the version scripts
    bison
    cpio # certs and initramfs plumbing
    elfutils # libelf, for objtool
    flex
    gmp # GCC plugins
    kmod # depmod, run by modules_install
    libmpc
    mpfr
    ncurses # kconfig
    openssl # module signing, certs
    pahole # BTF generation, when the config asks for it
    perl
    python3
    rsync
    zstd
  ];

  # `scripts/config` names symbols without the CONFIG_ prefix and adds it back
  # itself, so accept either spelling from the caller and normalise here.
  stripPrefix = lib.removePrefix "CONFIG_";

  flagFor = {
    y = "--enable";
    m = "--module";
    n = "--disable";
  };

  # Shell shared by both stages: the sandbox guard, and the command recorder
  # that turns each step into a line of report.json instead of an exit status.
  preamble = ''
    # See the header comment. Outside a sandbox NIX_BUILD_TOP is a random
    # temporary directory, the two stages would build at different paths, and
    # the incremental stage would quietly become a full rebuild.
    if [ "$NIX_BUILD_TOP" != "/build" ]; then
      echo "kernel-build requires a sandboxed build so that both stages share" >&2
      echo "one build path; expected NIX_BUILD_TOP=/build, got '$NIX_BUILD_TOP'" >&2
      exit 1
    fi

    # A kernel records when, where and by whom it was built, and stamps that
    # into the image and into every module's vermagic. Without pinning them,
    # two rigs building the same job produce different bytes under the *same*
    # store path — Nix derives that path from the inputs, not from the result —
    # so a caller comparing artifact hashes across rigs, or trusting a manifest
    # fetched from one rig against bytes fetched from another, would be wrong
    # in a way that is tedious to discover.
    export KBUILD_BUILD_TIMESTAMP="@$SOURCE_DATE_EPOCH"
    export KBUILD_BUILD_USER=rig
    export KBUILD_BUILD_HOST=rig

    logs="$NIX_BUILD_TOP/logs"
    records="$NIX_BUILD_TOP/records.jsonl"
    configCheck="$NIX_BUILD_TOP/config-check.json"
    mkdir -p "$logs"

    # Runs one command, streams its output so `nix build -L` shows progress,
    # keeps a copy for the report, and records how it went. Never returns
    # non-zero: the first failure sets buildFailed, and every later step then
    # skips itself, so the report describes where the build stopped rather than
    # a cascade of errors caused by the first one.
    record() {
      local name="$1"; shift

      if [ -n "''${buildFailed:-}" ]; then
        echo "--- skipping $name (an earlier step failed)"
        return 0
      fi

      echo "--- $name: $*"
      local rc=0
      set +e
      "$@" 2>&1 | tee "$logs/$name.log"
      rc=''${PIPESTATUS[0]}
      set -e

      python3 ${reportPy} add-command "$records" "$name" "$rc" "logs/$name.log" "$@"

      if [ "$rc" -ne 0 ]; then
        echo "--- $name failed with status $rc" >&2
        buildFailed=1
      fi
      return 0
    }
  '';

  mkBase =
    {
      src,
      commit,
      configBase ? "defconfig",
      configSet ? { },
      arch ? "x86_64",
    }:
    let
      # CONFIG_MODULES is not optional: the whole point is a driver built as a
      # module, and `make modules` needs it regardless of what the caller asked
      # for. Forced rather than defaulted so a caller cannot turn it off and
      # get a confusing failure much later.
      requested = {
        MODULES = "y";
      }
      // lib.mapAttrs' (name: value: {
        name = stripPrefix name;
        inherit value;
      }) configSet;

      requestedFile = writeText "required-config.json" (builtins.toJSON requested);

      configArgs = lib.concatStringsSep " " (
        lib.mapAttrsToList (symbol: value: "${flagFor.${value}} ${lib.escapeShellArg symbol}") requested
      );

      # Names the derivation after what actually determines its content, so two
      # jobs asking for the same commit and configuration land on the same
      # store path and the expensive stage happens once.
      configHash = builtins.substring 0 12 (
        builtins.hashString "sha256" (builtins.toJSON { inherit configBase requested arch; })
      );

      metaFile = writeText "meta.json" (
        builtins.toJSON {
          stage = "base";
          inherit commit arch configBase;
          config_requested = requested;
        }
      );
    in
    assert lib.assertMsg (arch == "x86_64") "kernel-build: only x86_64 is implemented, got ${arch}";
    assert lib.assertMsg (lib.all (v: builtins.hasAttr v flagFor) (
      lib.attrValues requested
    )) "kernel-build: config values must be one of y, m, n";
    stdenv.mkDerivation {
      pname = "linux-base";
      version = "${builtins.substring 0 12 commit}-${configHash}";

      inherit src;

      nativeBuildInputs = kernelToolchain;

      # A kernel build is not an ordinary package build: it manages its own
      # source tree, must not be stripped or patchelf'd, and breaks under the
      # hardening flags nixpkgs applies by default.
      dontUnpack = true;
      dontConfigure = true;
      dontFixup = true;
      hardeningDisable = [ "all" ];
      enableParallelBuilding = true;

      # Half an hour of one machine's whole CPU. Only offer this to a builder
      # that has advertised it can take that.
      requiredSystemFeatures = [ "big-parallel" ];

      buildPhase = ''
        runHook preBuild
        ${preamble}

        mkdir -p /build/linux /build/obj
        cp -a --reflink=auto "$src"/. /build/linux/
        chmod -R u+w /build/linux

        patchShebangs /build/linux/scripts

        record configure-defconfig \
          make -C /build/linux O=/build/obj ${configBase}
        record configure-symbols \
          /build/linux/scripts/config --file /build/obj/.config ${configArgs}
        record configure-olddefconfig \
          make -C /build/linux O=/build/obj olddefconfig
        record configure-verify \
          python3 ${reportPy} check-config /build/obj/.config ${requestedFile} "$configCheck"

        record build \
          make -C /build/linux O=/build/obj -j"$NIX_BUILD_CORES" bzImage modules

        runHook postBuild
      '';

      # Both trees are kept, and both are needed: the patched stage has to
      # start from the exact source this stage compiled, because the .cmd files
      # in obj/ refer to it.
      installPhase = ''
        runHook preInstall

        mkdir -p $out
        cp -a --reflink=auto /build/linux $out/source
        cp -a --reflink=auto /build/obj $out/obj
        cp -a "$logs" $out/logs

        if [ -f /build/obj/include/config/kernel.release ]; then
          cp /build/obj/include/config/kernel.release $out/kernel-release
        fi

        # No --artifacts here: hashing 18GB of object files would cost minutes
        # and nothing consumes it. The patched stage produces the manifest.
        python3 ${reportPy} finalise $out "$records" ${metaFile} --config-check "$configCheck"

        runHook postInstall
      '';

      passthru = {
        inherit
          commit
          requested
          configBase
          arch
          ;
      };

      meta = {
        description = "Unpatched Linux ${builtins.substring 0 12 commit}, built for reuse across patches";
        platforms = [ "x86_64-linux" ];
      };
    };

  mkPatched =
    {
      base,
      patch,
      name ? "candidate",
    }:
    let
      patchFile = writeText "${name}.patch" patch;
      patchSha256 = builtins.hashString "sha256" patch;

      metaFile = writeText "meta.json" (
        builtins.toJSON {
          stage = "patched";
          inherit (base) commit arch configBase;
          config_requested = base.requested;
          patch_sha256 = patchSha256;
          base_store_path = toString base;
        }
      );
    in
    stdenv.mkDerivation {
      pname = "linux";
      version = "${builtins.substring 0 12 base.commit}-${builtins.substring 0 12 patchSha256}";

      nativeBuildInputs = kernelToolchain;

      dontUnpack = true;
      dontConfigure = true;
      dontFixup = true;
      hardeningDisable = [ "all" ];
      enableParallelBuilding = true;

      buildPhase = ''
        runHook preBuild
        ${preamble}

        # If the base never built, there is nothing to patch. Set the same flag
        # the recorder uses, so every step below skips itself and the report
        # says where this stopped instead of failing with a confusing error
        # from make about a tree that was never finished.
        if [ "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["passed"])' \
                ${base}/report.json)" != "True" ]; then
          echo "--- base build did not pass; not attempting the patched build" >&2
          buildFailed=1
        fi

        # The copy the fixed build path costs us. On btrfs this is a reflink and
        # is close to free; see the header comment.
        mkdir -p /build
        cp -a --reflink=auto ${base}/source /build/linux
        cp -a --reflink=auto ${base}/obj /build/obj
        chmod -R u+w /build/linux /build/obj

        # Kbuild writes one modules.order per directory and concatenates them.
        # Relinking a module makes its .o newer than its directory's
        # modules.order, and the rule that then fires to regenerate it is one
        # whose only prerequisite is the phony directory target — so it writes
        # an *empty* file, the module drops out of the concatenated list, and
        # its .ko is silently never linked or installed. The build still
        # succeeds; the module is just missing.
        #
        # A from-scratch build never hits this because nothing is stale. It is
        # specific to rebuilding one subtree, which is exactly what this stage
        # does. Deleting them first forces every one to be regenerated from
        # obj-m, which is correct and costs nothing.
        find /build/obj -name modules.order -delete

        # patch(1) rather than git apply: there is no git in the sandbox, and
        # this is also what sets the mtimes that make the rebuild incremental.
        record apply-patch \
          patch -p1 -d /build/linux --no-backup-if-mismatch -i ${patchFile}

        record build \
          make -C /build/linux O=/build/obj -j"$NIX_BUILD_CORES" bzImage modules

        runHook postBuild
      '';

      installPhase = ''
                runHook preInstall

                mkdir -p $out

                if [ -f /build/obj/include/config/kernel.release ]; then
                  cp /build/obj/include/config/kernel.release $out/kernel-release
                fi
                if [ -f /build/obj/arch/x86/boot/bzImage ]; then
                  cp /build/obj/arch/x86/boot/bzImage $out/bzImage
                fi
                if [ -f /build/obj/vmlinux ]; then
                  cp /build/obj/vmlinux $out/vmlinux
                fi
                if [ -f /build/obj/.config ]; then
                  cp /build/obj/.config $out/config
                fi

                # A flat list of .ko files is not enough for callers: resolving a
                # module's dependency closure needs modules.dep and modules.builtin
                # alongside them, which is what modules_install produces.
                record modules-install \
                  make -C /build/linux O=/build/obj \
                    INSTALL_MOD_PATH=$out/modules modules_install

                # modules_install leaves `build` and `source` links pointing into
                # /build, which does not outlive this derivation.
                find $out/modules -maxdepth 4 -type l \
                  \( -name build -o -name source \) -delete 2>/dev/null || true

                cp -a "$logs" $out/logs

                # Configuration is verified once, by the base stage, because that is
                # where it is decided. But a caller is only ever handed *this* output —
                # the base is an implementation detail of how it was built — so the
                # verification is carried forward rather than left somewhere the caller
                # cannot see. Without this, a patched report says nothing about whether
                # the kernel it describes actually has the driver under test.
                python3 -c 'import json, sys
        base = json.load(open(sys.argv[1]))
        json.dump(base.get("config"), open(sys.argv[2], "w"))' ${base}/report.json "$configCheck"

                python3 ${reportPy} finalise $out "$records" ${metaFile} \
                  --config-check "$configCheck" --artifacts

                runHook postInstall
      '';

      passthru = {
        inherit base patchSha256;
        inherit (base) commit arch;
      };

      meta = {
        description = "Linux ${builtins.substring 0 12 base.commit} with one candidate patch applied";
        platforms = [ "x86_64-linux" ];
      };
    };
in
{
  inherit mkBase mkPatched;

  # The whole job in one call: build the shared base for this commit and
  # configuration, then apply the patch to it. Callers that submit many patches
  # against one commit get the base from the store on every call after the
  # first, which is the entire point of the split.
  build =
    {
      src,
      commit,
      patch ? null,
      configBase ? "defconfig",
      configSet ? { },
      arch ? "x86_64",
      name ? "candidate",
    }:
    let
      base = mkBase {
        inherit
          src
          commit
          configBase
          configSet
          arch
          ;
      };
    in
    if patch == null then base else mkPatched { inherit base patch name; };
}
