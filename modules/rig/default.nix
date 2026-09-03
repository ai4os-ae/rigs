{ config, lib, ... }:
let
  cfg = config.rigs.rig;
in
{
  imports = [
    ./kiosk.nix
    ./laptop.nix
  ];

  options.rigs.rig = {
    enable = lib.mkEnableOption "rig behaviour: unattended, always-on, runs jobs";
  };

  config = lib.mkIf cfg.enable {
    # Several of the rigs are laptops. A closed lid on a machine running a job
    # is not a request to suspend it, and neither is an idle SSH session.
    services.logind.settings.Login = {
      HandleLidSwitch = lib.mkForce "ignore";
      HandleLidSwitchDocked = lib.mkForce "ignore";
      HandleLidSwitchExternalPower = lib.mkForce "ignore";
      HandlePowerKey = lib.mkForce "poweroff";
    };

    systemd = {
      # Emergency mode is a root shell on a console nobody is at; a rig that
      # cannot mount its filesystems should reboot and try again instead of
      # waiting silently forever.
      enableEmergencyMode = false;

      settings.Manager = {
        RuntimeWatchdogSec = lib.mkDefault "15s";
        RebootWatchdogSec = lib.mkDefault "30s";
        KExecWatchdogSec = lib.mkDefault "1m";
      };

      sleep.settings.Sleep = {
        AllowSuspend = "no";
        AllowHibernation = "no";
        AllowHybridSleep = "no";
        AllowSuspendThenHibernate = "no";
      };
    };

    # No display, so no reason to build a font cache.
    fonts.fontconfig.enable = lib.mkDefault false;

    # Jobs are the point of these machines; the scheduler should not be
    # throttling them to save power on a mains-powered box.
    powerManagement.cpuFreqGovernor = lib.mkDefault "performance";

    # A job that allocates past physical memory should be killed promptly
    # rather than taking the rig into a swap storm that makes SSH unusable.
    systemd.oomd = {
      enable = true;
      enableRootSlice = true;
      enableUserSlices = true;
    };
  };
}
