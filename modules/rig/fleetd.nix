# The front door to the fleet.
#
# Every rig runs `rigs.buildd` and can be reached directly over the mesh, which
# is fine while there is one rig and a person choosing it. This puts a single
# endpoint in front of all of them: it decides which rig a job runs on,
# forwards it, and proxies everything afterwards, so a client sees a fleet
# rather than a list of machines.
#
# Unlike the rigs, this is meant to be published — reverse-proxied from a host
# with a public address — so every request carries an API key and there is no
# unauthenticated path through it. It still listens on the mesh only; reaching
# it from outside is the reverse proxy's job, not this module's.
#
# It holds no job state. A job id is `<rig>.<id-on-that-rig>`, which is enough
# to route every later request, so there is nothing here to keep in step with
# the rigs and nothing to lose when it restarts.
{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.rigs.fleetd;
  rig = config.rigs.rig;

  user = "rig-fleetd";

  membersFile = pkgs.writeText "rig-fleet.json" (builtins.toJSON cfg.members);
in
{
  options.rigs.fleetd = {
    enable = lib.mkEnableOption "the fleet broker that places build jobs across rigs";

    package = lib.mkOption {
      description = "The broker to run.";
      type = lib.types.package;
      default = pkgs.callPackage ../../pkgs/rig-fleetd/package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ../../pkgs/rig-fleetd/package.nix { }";
    };

    port = lib.mkOption {
      description = ''
        Port to serve on. Still mesh-only — the host firewall trusts `sifr0`
        and nothing else. Publishing this is a reverse proxy's job.
      '';
      type = lib.types.port;
      default = 9100;
    };

    members = lib.mkOption {
      description = ''
        The rigs this broker places work on, each with the address of its
        `rigs.buildd`.

        A static list rather than something rigs register into: a rig is
        already declared in this repository with a fixed mesh address, so the
        configuration is the truth and there is no enrollment flow to get
        wrong. The cost is that adding a rig means rebuilding this host.
      '';
      type = lib.types.listOf (
        lib.types.submodule {
          options = {
            name = lib.mkOption {
              description = "The rig's name, as it appears in job ids.";
              type = lib.types.str;
            };
            url = lib.mkOption {
              description = "Base URL of that rig's build daemon.";
              type = lib.types.str;
              example = "http://10.10.0.60:9000";
            };
          };
        }
      );
      default = [ ];
    };

    apiKeysFile = lib.mkOption {
      description = ''
        File of API keys, one per line; `#` comments and blank lines are
        ignored, so each key can record whose it is — which is the difference
        between revoking one and rotating all of them.

        Required, and deliberately has no default: this service is meant to be
        published, so there is no configuration of it that is unauthenticated.
        Point it at a sops secret.
      '';
      type = lib.types.path;
      example = lib.literalExpression ''config.sops.secrets."fleetd/api-keys".path'';
    };

    stateDir = lib.mkOption {
      description = ''
        Holds the placement hint — which rig last built a given commit and
        configuration. Only a hint: losing it costs one slow build.
      '';
      type = lib.types.path;
      default = "/var/lib/rig/fleetd";
    };

    pollInterval = lib.mkOption {
      description = "How often each rig is asked how it is doing.";
      type = lib.types.str;
      default = "10s";
    };

    reverseProxy = lib.mkOption {
      description = ''
        Nebula node name of the host that publishes this broker, admitted to
        this port and to nothing else.

        The standing mesh policy admits `trusted` and `ai4os`, and the public
        proxy is deliberately in neither — it is a rented machine with an
        address the whole internet can reach, and giving it what a personal
        laptop has to reach a rig would be a poor trade for one vhost. Null
        leaves the broker reachable only from the mesh's usual members.
      '';
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "hisn";
    };
  };

  config = lib.mkIf (rig.enable && cfg.enable) {
    assertions = [
      {
        assertion = cfg.members != [ ];
        message = "rigs.fleetd is enabled but rigs.fleetd.members is empty: it would have nowhere to place a job.";
      }
      {
        assertion = lib.allUnique (map (m: m.name) cfg.members);
        message = "rigs.fleetd.members has duplicate names; a name is what routes a job back to its rig.";
      }
    ];

    # The broker's rig is the one whose screen shows the whole fleet. Its own
    # jobs already come from loopback, so only the others are added.
    rigs.kiosk.peers = lib.mkIf config.rigs.kiosk.enable (
      lib.filter (m: m.name != config.networking.hostName) cfg.members
    );

    # Matched on the peer's signed certificate name, so this admits that one
    # node to that one port and grants nothing to anybody else who reaches it.
    rigs.nebula.extraInbound = lib.optional (cfg.reverseProxy != null) {
      host = cfg.reverseProxy;
      inherit (cfg) port;
      proto = "tcp";
    };

    users.users.${user} = {
      isSystemUser = true;
      group = user;
      home = cfg.stateDir;
      description = "rig fleet broker";
    };
    users.groups.${user} = { };

    systemd.tmpfiles.rules = [
      "d /var/lib/rig 0755 root root -"
      "d ${cfg.stateDir} 0750 ${user} ${user} -"
    ];

    # Socket-activated, so a client connecting while the broker is restarting
    # is queued rather than refused.
    systemd.sockets.rig-fleetd = {
      description = "Socket for the rig fleet broker";
      wantedBy = [ "sockets.target" ];
      socketConfig.ListenStream = toString cfg.port;
    };

    systemd.services.rig-fleetd = {
      description = "Places build jobs across the rigs";
      requires = [ "rig-fleetd.socket" ];
      wants = [ "network-online.target" ];
      after = [
        "network-online.target"
        "rig-fleetd.socket"
      ];

      serviceConfig = {
        ExecStart = lib.escapeShellArgs [
          (lib.getExe cfg.package)
          "-members"
          membersFile
          "-api-keys"
          cfg.apiKeysFile
          "-state"
          cfg.stateDir
          "-poll"
          cfg.pollInterval
        ];

        User = user;
        Group = user;

        Restart = "always";
        RestartSec = "10s";

        ReadWritePaths = [ cfg.stateDir ];

        # It talks to rigs and to its own clients and touches nothing else on
        # the machine, so it gets rather less than the build daemon does.
        CapabilityBoundingSet = [ "" ];
        NoNewPrivileges = true;
        PrivateDevices = true;
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
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [ "@system-service" ];

        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
      };
    };
  };
}
