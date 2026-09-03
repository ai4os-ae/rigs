{
  config,
  pkgs,
  lib,
  inputs,
  ...
}:
let
  cfg = config.rigs;
in
{
  imports = [
    inputs.srvos.nixosModules.mixins-nix-experimental
    ./options.nix
    ./secrets.nix
    ./users.nix
    ./people.nix
    ./autoupgrade.nix
    ./ssh.nix
    ./wifi.nix
    ./nebula.nix
  ];

  config = {
    # Where the nightly upgrade pulls from. Per-host overridable, but every rig
    # currently tracks the same branch of the same repository.
    rigs.projectFlake = lib.mkDefault "github:ai4os-ae/rigs";

    time.timeZone = cfg.timezone;
    i18n.defaultLocale = "en_GB.UTF-8";

    environment.systemPackages = with pkgs; [
      bc
      curl
      dig
      ethtool
      fd
      file
      git
      htop
      iotop
      jq
      killall
      lsof
      nix-output-monitor
      numactl
      parted
      pciutils
      pstree
      pv
      ripgrep
      rsync
      smartmontools
      sops
      sysstat
      tcpdump
      tmux
      tree
      unzip
      usbutils
      vim
      wget
      zip
    ];

    environment.variables = {
      EDITOR = "vim";
      DO_NOT_TRACK = "1";
    };

    security.sudo-rs.enable = true;
    security.sudo.extraConfig = ''
      Defaults lecture = never
    '';

    # A rig has no console anyone will walk up to, and its accounts are
    # SSH-key-only by default — so a sudo password prompt on a remote rebuild
    # is a prompt with nothing to answer it. wheel is already root here by way
    # of root's authorized_keys, so this widens the path rather than the
    # privilege. Both spellings, because which one is in use depends on whether
    # sudo-rs is the active implementation.
    security.sudo-rs.wheelNeedsPassword = false;
    security.sudo.wheelNeedsPassword = false;

    networking.firewall.enable = true;

    hardware.enableRedistributableFirmware = true;

    services.getty.greetingLine = lib.mkOverride 50 ''<<< ${config.networking.hostName} — AI4OS rig (\l) >>>'';

    nix = {
      settings = {
        experimental-features = [
          "pipe-operators"
          "auto-allocate-uids"
        ];
        trusted-users = [ "root" ] ++ lib.attrNames (lib.filterAttrs (_: u: u.admin) cfg.users);

        # Otherwise nix's answer to this flake's `nixConfig` block is cached
        # per-user, and the nightly upgrade — whose stderr is not a terminal —
        # can never ask, so it warns and ignores the settings instead.
        accept-flake-config = true;
        auto-optimise-store = true;
      };

      gc = {
        automatic = true;
        dates = "weekly";
        options = "--delete-older-than 30d";
      };
    };

    nixpkgs.config.allowUnfree = true;
  };
}
