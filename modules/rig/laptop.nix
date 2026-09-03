# Some of the rigs are laptops, and a laptop is the one rig somebody can walk
# up to and change the state of without touching a keyboard: pull the charger
# out of the socket it was borrowing, or fold the lid shut on a machine that is
# eight hours into a run. Neither shows up anywhere useful — the rig module
# already tells logind to ignore the lid, so the job keeps running and the only
# sign is that the screen saying "do not unplug this device" is now face-down.
#
# So the machine says it out loud instead, to the person who is still standing
# next to it. Nothing here suspends, throttles or stops anything.
{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.rigs.rig;

  # Drawn at build time for the same reason the kiosk's QR code is: aplay comes
  # with alsa-utils, which the rig has anyway, while synthesising the tone at
  # runtime would put sox on every laptop rig to produce the same 0.6 seconds
  # of audio every time. Two pulses rather than one, so it reads as an alarm
  # and not as a notification, and 16-bit so the file stays small. The fades
  # are what keep the start and end of each pulse from clicking.
  tone = pkgs.runCommand "rig-alarm-tone.wav" { nativeBuildInputs = [ pkgs.sox ]; } ''
    sox -n -r 48000 -c 1 pulse.wav synth 0.25 sine 1000 gain -3 fade q 0.01 0.25 0.01
    sox -n -r 48000 -c 1 gap.wav trim 0 0.12
    sox pulse.wav gap.wav pulse.wav -b 16 -t wav "$out"
  '';

  alarm = pkgs.writeShellApplication {
    name = "rig-alarm";
    runtimeInputs = with pkgs; [
      alsa-utils # aplay, amixer
      coreutils
      systemd # busctl, for logind's view of the lid
    ];
    text = ''
      # Overridable, so the tone can be swapped without rebuilding the script.
      RIG_ALARM_TONE="''${RIG_ALARM_TONE:-${tone}}"
      export RIG_ALARM_TONE
    ''
    + builtins.readFile ./rig-alarm.sh;
  };
in
{
  options.rigs.rig.isLaptop = lib.mkOption {
    description = ''
      This rig is a laptop: it has a battery, a lid, and a speaker, and the
      power it runs on is a socket somebody else can want back.

      Turns on an audible alarm that beeps when the rig goes off mains or has
      its lid closed, for five minutes per episode and then quietly. It does
      not change what happens on either event — a rig ignores both by design —
      it only makes them hard to miss at the moment they happen.
    '';
    type = lib.types.bool;
    default = false;
  };

  config = lib.mkIf (cfg.enable && cfg.isLaptop) {
    # Here for the same reason as the alarm itself: this is the thing somebody
    # runs by hand at the machine, `RIG_ALARM_DRY_RUN=1 rig-alarm`, to find out
    # whether the sensors read the way the alarm thinks they do.
    environment.systemPackages = [ alarm ];

    systemd.services.rig-alarm = {
      description = "Beep while this laptop rig is unplugged or its lid is shut";

      wantedBy = [ "multi-user.target" ];
      after = [
        "sound.target"
        "systemd-logind.service"
      ];

      serviceConfig = {
        ExecStart = lib.getExe alarm;

        # The alarm is only useful if it is running at the moment somebody
        # unplugs the rig, which is not a moment anybody is watching for.
        Restart = "always";
        RestartSec = "10s";

        # It reads sysfs, asks logind about the lid, and opens the sound card:
        # no state of its own and no account of its own. The audio group is
        # what /dev/snd is owned by, and is the whole reason for the device
        # rules below — PrivateDevices would take the sound card away and leave
        # a service that runs forever and never makes a sound.
        DynamicUser = true;
        SupplementaryGroups = [ "audio" ];
        DevicePolicy = "closed";
        DeviceAllow = [ "char-alsa rw" ];

        CapabilityBoundingSet = [ "" ];
        NoNewPrivileges = true;
        ProtectHome = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [ "@system-service" ];

        # AF_UNIX for the system bus, AF_NETLINK because that is how libasound
        # enumerates cards.
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_NETLINK"
        ];
      };
    };
  };
}
