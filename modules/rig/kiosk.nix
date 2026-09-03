# A rig with a screen attached says what it is. Cage takes tty1 and runs a
# single fullscreen Chromium against a small local webserver, so an unattended
# machine on a desk is identifiable — and its address readable — without
# logging into it.
#
# This is display only: the browser runs as its own unprivileged account with
# no keys, no wheel, and no shell, the server runs under a dynamic user on
# loopback, and nothing here touches how jobs run.
{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.rigs.kiosk;
  inherit (lib)
    mkOption
    mkEnableOption
    mkIf
    types
    ;

  # Drawn at build time rather than by the server: keeping a QR encoder out of
  # a program that otherwise has no dependencies is worth one derivation.
  qr = pkgs.runCommand "kiosk-qr.svg" { nativeBuildInputs = [ pkgs.qrencode ]; } ''
    qrencode --type=SVG --level=M --margin=0 --output=$out ${lib.escapeShellArg cfg.link}
  '';

  # `services.cage.program` is a single path, so the flags live in a wrapper
  # rather than being smuggled into it as a string with spaces.
  browser = pkgs.writeShellScript "kiosk-chromium" ''
    exec ${lib.getExe cfg.browser} \
      --kiosk \
      --ozone-platform=wayland \
      --user-data-dir=${cfg.stateDir}/chromium \
      --disk-cache-dir=${cfg.stateDir}/cache \
      --no-first-run \
      --noerrdialogs \
      --disable-infobars \
      --disable-features=TranslateUI \
      --disable-session-crashed-bubble \
      --disable-restore-session-state \
      --hide-crash-restore-bubble \
      ${lib.escapeShellArg cfg.url}
  '';
in
{
  options.rigs.kiosk = {
    enable = mkEnableOption "a fullscreen browser on the rig's own screen";

    user = mkOption {
      description = ''
        Account the compositor and browser run as. Deliberately not one of the
        people in `rigs.users`: whatever is on screen is reachable by anyone
        standing at the machine, so it should not be sitting in a researcher's
        session.
      '';
      type = types.str;
      default = "kiosk";
    };

    stateDir = mkOption {
      description = ''
        Home for the kiosk account, and where Chromium keeps its profile and
        cache. Disposable — deleting it costs nothing but a fresh profile.
      '';
      type = types.path;
      default = "/var/lib/kiosk";
    };

    browser = mkOption {
      description = "Browser to run. Anything that takes a URL as its last argument and understands the Chromium flags above.";
      type = types.package;
      default = pkgs.chromium;
      defaultText = lib.literalExpression "pkgs.chromium";
    };

    heading = mkOption {
      description = "Large line on the page.";
      type = types.str;
      default = "AI4OS Test Rig";
    };

    subheading = mkOption {
      description = "Smaller line under the heading. The hostname, so a room of rigs can be told apart at a glance.";
      type = types.str;
      default = config.networking.hostName;
      defaultText = lib.literalExpression "config.networking.hostName";
    };

    warning = mkOption {
      description = ''
        Red line along the bottom of the page. A rig looks idle when it is
        halfway through a week-long job, and the nearest power socket is the
        obvious thing for somebody to reclaim. Empty string leaves it off.
      '';
      type = types.str;
      default = "Do not turn off or unplug this device";
    };

    link = mkOption {
      description = ''
        Address behind the QR code in the corner of the page, captioned with
        the same address in case the code will not scan. Somewhere a person
        standing at the rig can find out what it is and who runs it.
      '';
      type = types.str;
      default = "https://github.com/ai4os-ae";
    };

    screensaver = {
      idle = mkOption {
        description = ''
          How long without a keypress or a mouse movement before the readings
          fade out and "AI4OS" starts drifting around the panel. Any input ends
          it and starts this wait again.
        '';
        type = types.str;
        default = "10m";
        example = "30m";
      };

      duration = mkOption {
        description = ''
          How long a run lasts before the readings come back on their own. Also
          the length of one pass through the colours, so a run leaves blue and
          arrives back at blue. Zero leaves the screensaver off entirely.
        '';
        type = types.str;
        default = "1m";
      };
    };

    url = mkOption {
      description = ''
        What to display. Defaults to this rig's own status page; point it at a
        dashboard once there is one to point at.
      '';
      type = types.str;
      default = "http://${cfg.server.listen}/";
      defaultText = lib.literalExpression ''"http://''${config.rigs.kiosk.server.listen}/"'';
    };

    server = {
      package = mkOption {
        description = "The status page server itself.";
        type = types.package;
        default = pkgs.callPackage ../../pkgs/rig-kiosk-server/package.nix { };
        defaultText = lib.literalExpression "pkgs.callPackage ../../pkgs/rig-kiosk-server/package.nix { }";
      };

      listen = mkOption {
        description = ''
          Address the page is served on. Loopback by design: the page is
          unauthenticated and is meant for the panel in front of you, not for
          the network. Widening it means opening the firewall as well, and
          means anyone on the campus wireless can read the rig's addresses and
          load.
        '';
        type = types.str;
        default = "127.0.0.1:8000";
        example = "[::1]:8000";
      };
    };
  };

  config = mkIf cfg.enable {
    # Socket activation, for the boot ordering rather than for laziness:
    # systemd binds the port at sockets.target, well before the compositor
    # starts, so the browser's first connection is queued instead of refused
    # and the rig never comes up displaying a connection-error page.
    systemd.sockets.rig-kiosk-server = {
      description = "Socket for the rig kiosk status page";
      wantedBy = [ "sockets.target" ];
      socketConfig.ListenStream = cfg.server.listen;
    };

    systemd.services.rig-kiosk-server = {
      description = "Rig kiosk status page";
      requires = [ "rig-kiosk-server.socket" ];
      after = [
        "rig-kiosk-server.socket"
        "network.target"
      ];

      serviceConfig = {
        ExecStart = lib.escapeShellArgs [
          (lib.getExe cfg.server.package)
          "-listen"
          cfg.server.listen
          "-heading"
          cfg.heading
          "-subheading"
          cfg.subheading
          "-warning"
          cfg.warning
          "-link"
          cfg.link
          "-qr"
          qr
          "-idle"
          cfg.screensaver.idle
          "-screensaver"
          cfg.screensaver.duration
        ];

        Restart = "always";
        RestartSec = "2s";

        # It reads /proc and enumerates interfaces, and needs nothing else: no
        # state, no writable path, no privileges, no user of its own.
        DynamicUser = true;
        CapabilityBoundingSet = [ "" ];
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateDevices = true;
        PrivateTmp = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [ "@system-service" ];

        # AF_NETLINK is not optional here: listing the rig's addresses is a
        # netlink dump, and without it the page shows "no network" on a machine
        # that is perfectly well connected.
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_NETLINK"
          "AF_UNIX"
        ];
      };
    };

    services.cage = {
      enable = true;
      inherit (cfg) user;
      program = browser;
    };

    # The rig module turns the font cache off because a rig normally has no
    # display. This one does, and Chromium with no fonts renders the page as
    # boxes.
    fonts = {
      fontconfig.enable = true;
      packages = [ pkgs.dejavu_fonts ];
    };

    systemd.services."cage-tty1" = {
      after = [ "rig-kiosk-server.socket" ];
      wants = [ "rig-kiosk-server.socket" ];

      # Nobody is going to notice a browser that died at 3am, let alone restart
      # it, so the unit does that itself. The compositor holds tty1 either way.
      serviceConfig = {
        Restart = "always";
        RestartSec = "5s";
      };
    };

    users.users.${cfg.user} = {
      description = "Kiosk display session";

      # A system uid rather than a pinned one from the 1000-up range in
      # people.nix: this account owns nothing that outlives the machine.
      isSystemUser = true;
      group = cfg.user;

      home = cfg.stateDir;
      createHome = true;

      # No password, no keys, and the system-user default shell (nologin). The
      # session is started by systemd at boot and its PAM stack runs no auth
      # phase, so there is nothing here to log into and nothing that needs to:
      # locked is the correct state, not an oversight.
      hashedPassword = "!";

      # Seat access for the compositor: the GPU, the panel's backlight, and
      # whatever input devices are plugged in.
      extraGroups = [
        "video"
        "render"
        "input"
      ];
    };

    users.groups.${cfg.user} = { };
  };
}
