# The job service. A rig exists to run work somebody sent it, and until now
# that meant a person with an SSH session typing make.
#
# `rig-buildd` publishes an HTTP API on the mesh: post a kernel commit, a patch
# and a configuration, get back artifacts, logs and a report. Nix is what
# actually builds — which is why the same work is never done twice, and why a
# patched build reuses the unpatched base it shares with every other patch
# against that commit — but a client sees none of that and needs no Nix of its
# own.
#
# There is no central server and no agent polling one. Rigs have fixed mesh
# addresses and accept inbound connections from the ai4os and trusted groups,
# so a client simply connects to the rig it wants.
{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.rigs.buildd;
  rig = config.rigs.rig;

  user = "rig-buildd";

  # The whole directory, copied once. See the comment on -kernel-build below.
  nixDir = ../../nix;
  # Separate because it lives under pkgs/ with the other packages, and a
  # relative path from inside nixDir could not reach it.
  syzkallerDir = ../../pkgs/syzkaller;
in
{
  options.rigs.buildd = {
    enable = lib.mkEnableOption "the build job daemon";

    package = lib.mkOption {
      description = "The daemon to run.";
      type = lib.types.package;
      default = pkgs.callPackage ../../pkgs/rig-buildd/package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ../../pkgs/rig-buildd/package.nix { }";
    };

    port = lib.mkOption {
      description = ''
        Port to serve on. Reached over the mesh: the host firewall trusts
        `sifr0` and nothing else, so this is not exposed to whatever network
        the rig happens to have joined unless `openFirewall` says so.
      '';
      type = lib.types.port;
      default = 9000;
    };

    stateDir = lib.mkOption {
      description = ''
        Where jobs and the kernel mirror live. Under `/var/lib/rig`, which is
        its own btrfs subvolume, so a runaway job fills something that can be
        reclaimed on its own rather than taking the root filesystem with it.
      '';
      type = lib.types.path;
      default = "/var/lib/rig/buildd";
    };

    remote = lib.mkOption {
      description = ''
        Where to fetch kernel commits a job asks for and the mirror does not
        have. A job may name its own, but this is what it gets otherwise.
      '';
      type = lib.types.str;
      default = "https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git";
    };

    maxConcurrent = lib.mkOption {
      description = ''
        Builds to run at once. A kernel build already uses every thread on the
        machine, so raising this makes each job slower without finishing more
        of them; it is here for a rig that is unusually large.
      '';
      type = lib.types.ints.positive;
      default = 1;
    };

    tokenFile = lib.mkOption {
      description = ''
        File holding a bearer token every request must carry. The fleet's
        shared `buildd/token` by default, which the broker and the kiosk send
        too; null relies on the mesh alone, which authenticates the peer but
        stops being enough as soon as a rig is reached any other way.
      '';
      type = lib.types.nullOr lib.types.path;
      default = if config.rigs.bootstrap then null else config.sops.secrets."buildd/token".path;
      defaultText = lib.literalExpression ''config.sops.secrets."buildd/token".path, or null in bootstrap mode'';
    };

    openFirewall = lib.mkOption {
      description = ''
        Also accept connections on every other interface — the campus
        network, for a rig the broker reaches there rather than over the mesh.
        Plain HTTP, so the token crosses that network in the clear; it keeps
        out anybody who does not have it, not anybody who can watch the wire.
      '';
      type = lib.types.bool;
      default = false;
    };

    buildDir = lib.mkOption {
      description = ''
        Where Nix unpacks and runs builds.

        This is not a tuning knob. The patched stage of a kernel build starts
        by copying the base build tree, and that copy is only cheap if it can
        be a btrfs reflink — which needs the build directory on the same
        filesystem as the store. Nix's default is the daemon's `$TMPDIR`,
        which on a host with a tmpfs `/tmp` would put a multi-gigabyte tree in
        RAM.
      '';
      type = lib.types.path;
      default = "/var/lib/rig/nix-builds";
    };

    fuzz = {
      enable = lib.mkEnableOption ''
        fuzz jobs: `kind: fuzz` runs syzkaller against a kernel this rig built.

        Unlike a build or a test, a fuzzing run is not a derivation — it is a
        long-lived process the daemon supervises — so it needs `/dev/kvm` in
        the unit itself rather than only inside the Nix sandbox, and it holds
        a slot for hours. Off by default for that reason
      '';

      maxConcurrent = lib.mkOption {
        description = ''
          Fuzz runs to allow at once. One is almost always right: the VM pool
          is already sized to most of the machine.
        '';
        type = lib.types.ints.positive;
        default = 1;
      };

      maxDurationSeconds = lib.mkOption {
        description = ''
          The longest run this rig will accept. A request for more is refused
          rather than silently shortened.
        '';
        type = lib.types.ints.positive;
        default = 24 * 3600;
      };

      vmCPU = lib.mkOption {
        description = "vCPUs per fuzzing VM, when a job does not say.";
        type = lib.types.ints.positive;
        default = 4;
      };

      vmMemoryMB = lib.mkOption {
        description = ''
          MiB per fuzzing VM.

          This is not a tuning knob below about 3 GiB. `syz-executor` maps its
          data segment at a fixed address, and against a KASAN kernel that
          mapping fails on a smaller guest: the VM dies with
          `SYZFAIL: mmap of data segment failed`, is rebooted, and dies again.
          Nothing reports an error — the fuzzer is up and the VM count is right
          — and the execution rate simply stays at zero.
        '';
        type = lib.types.ints.positive;
        default = 3072;
      };

      hostReserveMB = lib.mkOption {
        description = ''
          RAM held back from the VM pool for the host: syz-manager itself keeps
          the corpus and every VM's console in memory, and a build job may be
          running alongside.
        '';
        type = lib.types.ints.positive;
        default = 24576;
      };

      hostReserveCPU = lib.mkOption {
        description = "Cores held back from the VM pool for the host.";
        type = lib.types.ints.positive;
        default = 2;
      };

      allowedDevices = lib.mkOption {
        description = ''
          QEMU device types a fuzz job may ask for.

          A focus profile sometimes needs a device to make its target reachable
          at all — `nix/kernel-fuzz/focus/e1000.json` parks a second NIC where
          syzkaller's NIC-VF probe looks for one. Requests name a type from
          this list and the daemon renders the arguments; raw `qemu_args` are
          deliberately not exposed, because a string handed to QEMU's argv
          turns "submit a patch" into "read this rig's disk".
        '';
        type = lib.types.listOf lib.types.str;
        default = [
          "e1000"
          "virtio-net-pci"
          "virtio-rng-pci"
          "nvme"
        ];
      };

      memoryHigh = lib.mkOption {
        description = ''
          Soft memory limit on the whole daemon, VMs included.

          A fuzzing VM pool is the one workload here that can grow until the
          machine stops answering, and a rig that stops answering needs a site
          visit. This is what keeps nebula and sshd resident.
        '';
        type = lib.types.nullOr lib.types.str;
        default = "80%";
      };
    };
  };

  config = lib.mkMerge [
    # Outside the gate below: the broker and the kiosk send this token too,
    # and either can run on a rig that builds nothing. Root-owned and handed
    # to each service with LoadCredential, so the kiosk's DynamicUser can
    # read it as well.
    (lib.mkIf (rig.enable && !config.rigs.bootstrap) {
      sops.secrets."buildd/token".restartUnits =
        lib.optional cfg.enable "rig-buildd.service"
        ++ lib.optional config.rigs.fleetd.enable "rig-fleetd.service"
        ++ lib.optional config.rigs.kiosk.enable "rig-kiosk-server.service";
    })

    (lib.mkIf (rig.enable && cfg.enable) {
      # A rig with a screen should say whether it is busy. Wired here rather than
      # per-host so that turning the daemon on is the whole of it.
      rigs.kiosk.buildd = lib.mkIf config.rigs.kiosk.enable "http://127.0.0.1:${toString cfg.port}";

      users.users.${user} = {
        isSystemUser = true;
        group = user;
        home = cfg.stateDir;
        description = "rig build job daemon";
      };
      users.groups.${user} = { };

      # `/var/lib/rig` is a mount point owned by root and shared with whatever
      # else a rig keeps there, so the daemon is given its own directory inside
      # it rather than StateDirectory taking ownership of the parent.
      systemd.tmpfiles.rules = [
        "d /var/lib/rig 0755 root root -"
        "d ${cfg.stateDir} 0750 ${user} ${user} -"
        "d ${cfg.buildDir} 0755 root root -"
      ];

      # A test job boots the kernel it is testing inside QEMU, and the
      # derivation that does it asks for the `kvm` feature. Without the feature
      # advertised the job is not scheduled at all — which is the right failure,
      # and a clear one — and without /dev/kvm behind it QEMU would emulate every
      # instruction and turn a ten second boot into several minutes.
      #
      # NixOS advertises kvm by default on a machine that has it. Asserted rather
      # than assumed because the failure otherwise arrives at the first test job
      # somebody submits, on a rig nobody is standing in front of.
      assertions = [
        {
          assertion = lib.elem "kvm" config.nix.settings.system-features;
          message = ''
            rigs.buildd needs the "kvm" system feature: test jobs boot a kernel
            under QEMU. Either this rig has no /dev/kvm, in which case it cannot
            run test jobs, or nix.settings.system-features has been overridden
            without it.
          '';
        }
        {
          assertion = cfg.openFirewall -> cfg.tokenFile != null;
          message = "rigs.buildd.openFirewall would expose an unauthenticated build daemon; it needs rigs.buildd.tokenFile.";
        }
      ];

      nix.settings = {
        build-dir = cfg.buildDir;

        # A base build tree is a couple of gigabytes and every commit and
        # configuration gets its own, so a rig left alone will fill its disk.
        # Automatic collection keeps it working without anybody visiting; a
        # finished job's artifacts are protected from it by the GC root the
        # daemon registers, so this only ever reclaims what nothing points at.
        min-free = 20 * 1024 * 1024 * 1024;
        max-free = 60 * 1024 * 1024 * 1024;
      };

      networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ cfg.port ];

      # Socket-activated so that a client connecting while the daemon is still
      # starting is queued rather than refused.
      systemd.sockets.rig-buildd = {
        description = "Socket for the rig build job daemon";
        wantedBy = [ "sockets.target" ];
        socketConfig.ListenStream = toString cfg.port;
      };

      systemd.services.rig-buildd = {
        description = "Runs build jobs on this rig";
        requires = [ "rig-buildd.socket" ];
        # Wanted, not merely ordered after: the first job on a rig clones the
        # kernel, so starting before there is a network means failing that job
        # rather than waiting for one.
        wants = [ "network-online.target" ];
        after = [
          "network-online.target"
          "nix-daemon.socket"
        ];

        # git to maintain the kernel mirror, nix to run the builds. Neither is
        # baked into the package: the mirror is ordinary git, and the nix that
        # runs a build must be the machine's own.
        #
        # The rest are for fuzzing, and none of them are optional there. A build
        # or a test happens inside a Nix sandbox that brings its own tools;
        # syz-manager runs here, in this unit, and shells out constantly.
        path = [
          pkgs.git
          config.nix.package
        ]
        ++ lib.optionals cfg.fuzz.enable [
          pkgs.qemu # the VMs
          pkgs.openssh # ssh and scp into each one, and the key generated at first run
          pkgs.binutils # addr2line and nm, for symbolising coverage out of vmlinux
          # gcc is for C reproducers, which syzkaller compiles at *runtime* rather
          # than at build time. See LIBRARY_PATH below.
          pkgs.gcc
        ];

        environment = {
          HOME = cfg.stateDir;
          XDG_CACHE_HOME = "${cfg.stateDir}/cache";
        }
        // lib.optionalAttrs cfg.fuzz.enable {
          # BOTH OF THESE ARE ABOUT C REPRODUCERS, and getting either wrong costs
          # you every one of them without a single error message.
          #
          # A crash reported as a syz program is a crash a maintainer cannot act
          # on; the C reproducer is the artifact that matters. syzkaller builds
          # them by shelling out to gcc while it runs, so neither of these is
          # covered by anything pinned at build time.
          #
          # LIBRARY_PATH: syzkaller probes `gcc -static` and silently falls back
          # to a dynamic link when it fails. A dynamically linked reproducer
          # names a /nix/store ELF interpreter that does not exist inside the
          # guest, so it fails to exec; syzkaller can only observe that no crash
          # happened, concludes the bug does not reproduce in C, and throws the
          # reproducer away. Interpolated rather than passed through
          # lib.makeLibraryPath, which resolves glibc's "lib" output — the
          # *dynamic* one — leaving the static libc silently off the path.
          LIBRARY_PATH = "${pkgs.glibc.static}/lib";

          # CPATH must be empty, and it is set here so that nothing inherits one.
          # To build a reproducer syzkaller preprocesses its executor header with
          # `cpp -nostdinc -fdirectives-only` and relies on every #include
          # failing to resolve. -nostdinc does not clear CPATH, so a non-empty
          # one gives cpp somewhere to look and turns the expected non-fatal
          # diagnostic into a fatal error. cpp dies twenty lines in, and because
          # syzkaller only treats *empty* cpp output as failure it accepts the
          # truncated header and emits a reproducer with no main(). The daemon
          # also clears it per-process; this is the belt to that's braces.
          CPATH = "";
        };

        serviceConfig = {
          ExecStart = lib.escapeShellArgs (
            [
              (lib.getExe cfg.package)
              "-state"
              cfg.stateDir
              "-remote"
              cfg.remote
              "-max-concurrent"
              (toString cfg.maxConcurrent)

              # Pinned rather than read from NIX_PATH, so that what a rig builds
              # against is a property of this configuration and not of whatever
              # channel happens to be registered on the machine.
              #
              # Interpolated, not `toString`: interpolation records a store
              # reference, so the paths are copied to the rig as dependencies of
              # this unit. `toString` yields the same text with no reference, and
              # the daemon then starts happily and fails at the first job with a
              # store path that was never copied.
              #
              # One copy of nix/ rather than one per directory, because
              # kernel-test reads kernel-build's report.py by a relative path —
              # the two agree on report.json's schema, and sharing the file is
              # how they go on agreeing. Copied separately, that `..` would climb
              # out of the store path and find nothing.
              "-nixpkgs"
              "${pkgs.path}"
              "-kernel-build"
              "${nixDir}/kernel-build"
              "-kernel-test"
              "${nixDir}/kernel-test"
            ]
            ++ lib.optionals cfg.fuzz.enable [
              "-kernel-fuzz"
              "${nixDir}/kernel-fuzz"
              "-syzkaller"
              "${syzkallerDir}"
              "-max-concurrent-fuzz"
              (toString cfg.fuzz.maxConcurrent)
              "-max-fuzz-duration"
              (toString cfg.fuzz.maxDurationSeconds)
              "-fuzz-vm-cpu"
              (toString cfg.fuzz.vmCPU)
              "-fuzz-vm-memory-mb"
              (toString cfg.fuzz.vmMemoryMB)
              "-fuzz-host-reserve-mb"
              (toString cfg.fuzz.hostReserveMB)
              "-fuzz-host-reserve-cpu"
              (toString cfg.fuzz.hostReserveCPU)
              "-fuzz-devices"
              (lib.concatStringsSep "," cfg.fuzz.allowedDevices)
            ]
            ++ lib.optionals (cfg.tokenFile != null) [
              "-token-file"
              "%d/token"
            ]
          );

          LoadCredential = lib.optional (cfg.tokenFile != null) "token:${cfg.tokenFile}";

          User = user;
          Group = user;

          Restart = "always";
          RestartSec = "10s";

          # A kernel build is half an hour of every core. The daemon itself is
          # idle; it is the builds, run by the nix daemon, that do the work, so
          # the weight belongs on them and not here.
          Nice = 5;

          ReadWritePaths = [
            cfg.stateDir
            cfg.buildDir
          ];

          # Deliberately not DynamicUser, unlike the kiosk server: this daemon
          # owns persistent state and its GC roots have to survive a restart
          # under the same uid.
          CapabilityBoundingSet = [ "" ];
          NoNewPrivileges = true;
          PrivateTmp = true;
          ProtectHome = true;
          ProtectClock = true;
          ProtectControlGroups = true;
          ProtectKernelLogs = true;
          ProtectKernelModules = true;
          ProtectKernelTunables = true;
          ProtectSystem = "strict";
          RestrictNamespaces = true;
          RestrictRealtime = true;
          RestrictSUIDSGID = true;
          LockPersonality = true;
          SystemCallArchitectures = "native";
          SystemCallFilter = [ "@system-service" ];

          # AF_UNIX for the nix daemon socket, AF_INET/AF_INET6 to serve the API
          # and to let git fetch a commit the mirror does not have.
          RestrictAddressFamilies = [
            "AF_UNIX"
            "AF_INET"
            "AF_INET6"
          ];
        }
        // lib.optionalAttrs cfg.fuzz.enable {
          # A test job's VM is started by Nix, inside a sandbox that Nix binds
          # /dev/kvm into. A fuzzing run's VMs are started *here*, by this unit,
          # so the unit needs the device itself. Without it QEMU falls back to
          # emulating every instruction: the pool boots, the fuzzer runs, and the
          # execution rate is roughly fifty times lower than it should be, with
          # nothing anywhere saying why.
          DeviceAllow = [ "/dev/kvm rw" ];
          SupplementaryGroups = [ "kvm" ];

          MemoryHigh = lib.mkIf (cfg.fuzz.memoryHigh != null) cfg.fuzz.memoryHigh;
        };
      };
    })
  ];
}
