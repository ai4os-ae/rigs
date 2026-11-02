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

    ./hardware.nix
    ./disk.nix
  ];

  networking.hostName = "desktop-01";
  networking.useNetworkd = true;

  # DHCP on anything wired. Unlike laptop-01 this one has a port (eno1); it
  # was installed with nothing plugged into it, which is why wifi is set up
  # as well.
  systemd.network.networks."10-wired" = {
    matchConfig.Type = "ether";
    networkConfig.DHCP = "yes";
  };

  systemd.network.networks."25-wireless" = {
    matchConfig.Type = "wlan";
    networkConfig.DHCP = "yes";
  };

  rigs = {
    rig.enable = true;
    autoupgrade.enable = true;

    # 20 threads and 128GB. Eight builds at once oversubscribes the CPU, so
    # each one is slower than it would be alone, but RAM is what runs out
    # first on a kernel build and there is room here for eight.
    buildd = {
      enable = true;
      maxConcurrent = 8;
    };

    # The fleet's front door, moved here from laptop-01: this is the rig that
    # is plugged in and stays put.
    fleetd = lib.mkIf (!config.rigs.bootstrap) {
      enable = true;
      apiKeysFile = config.sops.secrets."fleetd/api-keys".path;

      # rigs.fleeti.ae is served by hisn, which proxies to this port over the
      # mesh. hisn is the lighthouse and a public VPS, so it holds neither
      # `trusted` nor `ai4os`; this admits it to the broker and nothing else.
      reverseProxy = "hisn";
      members = [
        {
          name = "desktop-01";
          url = "http://10.10.0.62:9000";
        }
        {
          name = "laptop-01";
          url = "http://10.10.0.60:9000";
        }
      ];
    };

    # The screen shows the rig's name, as on laptop-01. cage takes tty1.
    kiosk.enable = true;

    # Its host key was pre-seeded at install, so its age key is in .sops.yaml
    # from the first boot and there is no bootstrap phase to go through.
    bootstrap = false;
  };

  # Same network and credential as laptop-01; see the comment there.
  rigs.wifi = lib.mkIf (!config.rigs.bootstrap) {
    enable = true;
    secretsFile = config.sops.secrets."wifi/env".path;
    networks."MBZUAI-STUDENT" = {
      priority = 10;
      enterprise = {
        identity = "humaid.alqasimi@mbzuai.ac.ae";
        passwordSecret = "mbzuai_password";
      };
    };
  };

  # 10.10.0.62 on the mesh, in the `ai4os` group.
  rigs.nebula = lib.mkIf (!config.rigs.bootstrap) {
    enable = true;
    cert = config.sops.secrets."nebula/crt".path;
    key = config.sops.secrets."nebula/key".path;
  };

  sops.secrets = lib.mkIf (!config.rigs.bootstrap) {
    # The keys the fleet broker admits. Owned by its own user, which reads them
    # after systemd has dropped privileges, and restarting on change is what
    # makes revoking a key take effect without a person remembering to.
    "fleetd/api-keys" = {
      sopsFile = ../../secrets/desktop-01.yaml;
      owner = "rig-fleetd";
      restartUnits = [ "rig-fleetd.service" ];
    };

    # Owned by wpa_supplicant for the reason given on laptop-01.
    "wifi/env" = {
      sopsFile = ../../secrets/desktop-01.yaml;
      owner = "wpa_supplicant";
      restartUnits = [ "wpa_supplicant.service" ];
    };

    "nebula/crt" = {
      sopsFile = ../../secrets/desktop-01.yaml;
      owner = "nebula-sifr0";
      restartUnits = [ "nebula@sifr0.service" ];
    };

    "nebula/key" = {
      sopsFile = ../../secrets/desktop-01.yaml;
      owner = "nebula-sifr0";
      restartUnits = [ "nebula@sifr0.service" ];
    };
  };

  boot.loader.systemd-boot.enable = true;
  boot.loader.efi.canTouchEfiVariables = true;

  nixpkgs.hostPlatform = "x86_64-linux";
  system.stateVersion = "26.05";
}
