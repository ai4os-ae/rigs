{
  config,
  self,
  inputs,
  lib,
  ...
}:
{
  imports = [
    self.nixosModules.rigs.base
    self.nixosModules.rigs.rig
    inputs.disko.nixosModules.disko

    # Dell Latitude 7440. nixos-hardware has no profile for this model; the
    # generic Intel laptop handling in hardware.nix is what it needs. Revisit
    # if one appears upstream.

    ./hardware.nix
    ./disk.nix
  ];

  networking.hostName = "laptop-01";
  networking.useNetworkd = true;

  # DHCP on anything wired. The 7440 has no ethernet port, so in practice this
  # only ever matches a USB-C dock or a Thunderbolt host-to-host link.
  systemd.network.networks."10-wired" = {
    matchConfig.Type = "ether";
    networkConfig.DHCP = "yes";
  };

  # Wireless is this rig's only real uplink, so it is not optional here: with
  # no ethernet port, a wifi misconfiguration is a machine that has to be
  # rescued at the keyboard.
  systemd.network.networks."25-wireless" = {
    matchConfig.Type = "wlan";
    networkConfig.DHCP = "yes";
  };

  rigs = {
    rig.enable = true;
    autoupgrade.enable = true;

    # Kernel build jobs, over the mesh. 12 threads and 32GB, which is one
    # kernel build at a time. The fleet's broker is on desktop-01.
    buildd = {
      enable = true;
      maxConcurrent = 1;
    };

    # A Latitude on a desk in a shared room: the charger is the nearest one to
    # borrow and the lid is the obvious thing to close on a machine that looks
    # idle. Both now beep until they are put back.
    rig.isLaptop = true;

    # The laptop's own panel, which is otherwise a lid nobody opens: cage takes
    # tty1 at boot and shows the rig's name. No getty, no login prompt.
    kiosk.enable = true;

    # Off since 2026-09-03: the host is installed, its age key is in
    # .sops.yaml, and it can read the wireless credentials in secrets/all.yaml.
    # Turning this off is what enables wifi — see rigs.wifi below.
    bootstrap = false;
  };

  # Wireless is gated on the same flag: the secret it reads is only declared
  # when bootstrap is off, and pointing wpa_supplicant at a path that will
  # never appear fails silently at 3am rather than loudly at build time.
  # The networks and credentials are fleet-wide; see modules/base/wifi.nix.
  rigs.wifi.enable = !config.rigs.bootstrap;

  # 10.10.0.60 on the mesh, in the `ai4os` group. Same gate as wifi, and for
  # the same reason: both halves of the node identity come out of sops, which a
  # rig cannot read until it has an age key.
  rigs.nebula = lib.mkIf (!config.rigs.bootstrap) {
    enable = true;
    cert = config.sops.secrets."nebula/crt".path;
    key = config.sops.secrets."nebula/key".path;
  };

  sops.secrets = lib.mkIf (!config.rigs.bootstrap) {
    # The certificate is public and the key is not, but both are owned by the
    # daemon's user for the same reason the wifi secrets are owned by
    # wpa_supplicant (see modules/base/wifi.nix): nebula reads them after
    # dropping privileges.
    "nebula/crt" = {
      sopsFile = ../../secrets/laptop-01.yaml;
      owner = "nebula-sifr0";
      restartUnits = [ "nebula@sifr0.service" ];
    };

    "nebula/key" = {
      sopsFile = ../../secrets/laptop-01.yaml;
      owner = "nebula-sifr0";
      restartUnits = [ "nebula@sifr0.service" ];
    };
  };

  boot.loader.systemd-boot.enable = true;
  boot.loader.efi.canTouchEfiVariables = true;

  nixpkgs.hostPlatform = "x86_64-linux";
  system.stateVersion = "26.05";
}
