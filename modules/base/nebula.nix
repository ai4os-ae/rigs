# Membership of the sifr0 overlay network, which is how a rig is reached at
# all. Rigs sit on campus wireless behind NAT with no inbound port and no
# stable address; without the mesh there is no route to one that does not
# start with somebody walking to the machine.
#
# The overlay is not this repository's — it belongs to the personal dotfiles
# fleet, and rigs are guests on it. Two things follow. The CA below is that
# network's CA, so a rig can only join with a certificate signed by it, and
# rigs are issued certificates in the `ai4os` group and no other: the group is
# what the rest of the fleet writes its own firewall rules against, and a rig
# carrying `trusted` would be handed the access a personal laptop has.
{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.rigs.nebula;

  port = 4242;

  # hisn is the network's sole lighthouse and relay. Rigs are never either:
  # they are behind NAT and frequently off, which is the opposite of what a
  # lighthouse has to be.
  lighthouse = "10.10.0.20";

  # A static entry is not lighthouse status — it is "this address answers at
  # this endpoint", which saves a rig from needing a lighthouse in order to
  # find the lighthouse.
  staticHosts = {
    ${lighthouse} = [ "45.59.120.67:${toString port}" ];
  };

  # Overlay addresses, so a rig can say `laptop-01` rather than an address. The
  # 10.10.0.60+ block is the rigs' own: the addresses below it are personal
  # devices in the dotfiles repo, and allocating from the top of the range
  # keeps the two rosters from growing into each other. A new rig takes the
  # next free address here and gets a certificate for it — see README.
  hosts = {
    ${lighthouse} = [ "hisn" ];
    "10.10.0.60" = [ "laptop-01" ];
  };
in
{
  options.rigs.nebula = {
    enable = lib.mkEnableOption "membership of the sifr0 overlay network";

    cert = lib.mkOption {
      description = ''
        Path to this rig's signed node certificate. Normally a sops secret, so
        it arrives with the key it belongs to and nothing has to be copied onto
        the rig by hand.
      '';
      type = lib.types.path;
    };

    key = lib.mkOption {
      description = ''
        Path to this rig's node private key. Must be a sops secret owned by
        `nebula-sifr0`: the daemon drops privileges to that user and reads the
        key afterwards, so a root-only path fails at startup.
      '';
      type = lib.types.path;
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ pkgs.nebula ];

    networking.hosts = hosts;

    networking.firewall = {
      allowedUDPPorts = [ port ];

      # The mesh's access policy is the nebula firewall below, which is
      # evaluated on group membership rather than on address and is the only
      # layer that can tell a personal laptop from a stranger who reached the
      # port. Filtering sifr0 a second time here would express the same policy
      # in terms that cannot see groups.
      trustedInterfaces = [ "sifr0" ];
    };

    services.nebula.networks.sifr0 = {
      enable = true;
      tun.device = "sifr0";

      isLighthouse = false;
      isRelay = false;
      lighthouses = [ lighthouse ];
      relays = [ lighthouse ];
      staticHostMap = staticHosts;

      ca = ./sifr0-ca.crt;
      inherit (cfg) cert key;

      listen = {
        host = "0.0.0.0";
        inherit port;
      };

      firewall = {
        outbound = [
          {
            host = "any";
            port = "any";
            proto = "any";
          }
        ];

        inbound = [
          # Reachability is worth more than hiding on a machine nobody can walk
          # up to: a rig that answers a ping is a rig you can tell apart from
          # one whose wifi dropped.
          {
            host = "any";
            port = "any";
            proto = "icmp";
          }

          # Administration. `trusted` is the dotfiles fleet's group for
          # humaid's own machines, and it is the path a remote rebuild and a
          # rescue SSH both take.
          {
            groups = [ "trusted" ];
            port = "any";
            proto = "any";
          }

          # Rig to rig, for distributed jobs and for one rig building for
          # another. Rigs hold no personal data and run the same configuration,
          # so the group is open to itself rather than enumerated per port.
          {
            groups = [ "ai4os" ];
            port = "any";
            proto = "any";
          }
        ];
      };

      settings = {
        punchy = {
          punch = true;
          punch_back = true;
          respond = true;
        };

        # Rigs sharing a lab switch or an access point should talk across it
        # rather than round-tripping through the lighthouse in another country.
        preferred_ranges = [
          "10.0.0.0/8"
          "192.168.0.0/16"
          "172.16.0.0/12"
        ];
      };
    };

    # A rig whose only route in is the mesh should not be waiting on a DHCP
    # lease that already came back before it starts trying to reach the
    # lighthouse, nor should a wifi drop leave the tunnel wedged.
    systemd.services."nebula@sifr0" = {
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      serviceConfig = {
        Restart = lib.mkDefault "always";
        RestartSec = lib.mkDefault "10s";
      };
    };
  };
}
