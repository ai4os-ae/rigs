{ config, lib, ... }:
let
  cfg = config.rigs.autoupgrade;
in
{
  options.rigs.autoupgrade = {
    enable = lib.mkEnableOption "the nightly rebuild from the project flake";

    allowReboot = lib.mkOption {
      description = ''
        Whether the nightly upgrade may reboot when the kernel or initrd
        changed.

        On by default, and the tradeoff is worth stating: a rig running a long
        job will lose it to a reboot inside the window below. There is no A/B
        partitioning here — the upgrade switches the running system in place
        and reboots when it must — so a rig with jobs that cannot be
        interrupted should set this to false and be rebooted by hand instead.
        With it off, kernel updates stage but do not take effect until someone
        reboots.
      '';
      type = lib.types.bool;
      default = true;
    };

    dates = lib.mkOption {
      description = "systemd calendar expression for when the upgrade starts.";
      type = lib.types.str;
      default = "01:30";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = config.rigs.projectFlake != null;
        message = "rigs.projectFlake must be set when rigs.autoupgrade.enable is true";
      }
    ];

    system.autoUpgrade = {
      enable = true;
      inherit (cfg) allowReboot dates;

      # Rigs are expected to come up together after a power event, and an
      # upgrade is the one moment they all pull from the same forge and the
      # same cache at once. The spread keeps that from being simultaneous.
      randomizedDelaySec = "45min";

      rebootWindow = {
        lower = "01:00";
        upper = "05:00";
      };

      flake = "${config.rigs.projectFlake}#${config.networking.hostName}";
      flags = [
        # Without this the flake input is served from the local eval cache and
        # the rig quietly rebuilds the same revision every night.
        "--refresh"
        "-L"
      ];
    };
  };
}
