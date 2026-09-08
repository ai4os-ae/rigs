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
        the rig happens to have joined.
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
        File holding a bearer token every request must carry. Null relies on
        the mesh alone, which already authenticates and encrypts the connection
        and restricts it to the `ai4os` and `trusted` groups — that is the
        expected deployment, and this is for when it stops being enough.
      '';
      type = lib.types.nullOr lib.types.path;
      default = null;
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
  };

  config = lib.mkIf (rig.enable && cfg.enable) {
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
      path = [
        pkgs.git
        config.nix.package
      ];

      environment = {
        HOME = cfg.stateDir;
        XDG_CACHE_HOME = "${cfg.stateDir}/cache";
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
            # reference, so both paths are copied to the rig as dependencies of
            # this unit. `toString` yields the same text with no reference, and
            # the daemon then starts happily and fails at the first job with a
            # store path that was never copied.
            "-nixpkgs"
            "${pkgs.path}"
            "-kernel-build"
            "${../../nix/kernel-build}"
          ]
          ++ lib.optionals (cfg.tokenFile != null) [
            "-token-file"
            (toString cfg.tokenFile)
          ]
        );

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
      };
    };
  };
}
