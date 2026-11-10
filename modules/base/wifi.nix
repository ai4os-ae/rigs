# Wireless for rigs that have no ethernet port — which, for a laptop rig, is
# most of them. Without this such a rig comes up after install with no network
# at all and no way to reach it.
#
# Every rig joins the same networks with the same credentials, so all of it
# lives once, in secrets/all.yaml under `wifi/`. None of it — not even the
# SSIDs or the identity — enters the repo or the Nix store: sops renders the
# network blocks into /run and wpa_supplicant reads them as an extra config
# file. That matters more than usual here because campus credentials are SSO
# credentials — the same password as mail and the student lab.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.rigs.wifi;
  inherit (lib)
    mkOption
    mkEnableOption
    mkIf
    optionalString
    types
    ;
  secret = name: config.sops.placeholder."wifi/${name}";

  # Succeeds when a physical wired link has a default route — a lease and a
  # gateway, not just carrier. A cable into a dead port or a Thunderbolt link
  # to another machine has carrier too, and dropping wifi for one of those is
  # how a rig falls off the network.
  wiredUp = pkgs.writeShellScript "rig-wired-up" ''
    for dir in /sys/class/net/*; do
      [ -e "$dir/device" ] && [ ! -e "$dir/wireless" ] || continue
      if [ -n "$(${pkgs.iproute2}/bin/ip route show default dev "''${dir##*/}")" ]; then
        exit 0
      fi
    done
    exit 1
  '';
in
{
  options.rigs.wifi = {
    enable = mkEnableOption "wireless networking via wpa_supplicant";

    fallbackHotspot = mkOption {
      description = ''
        Also join humaid's phone hotspot, from `wifi/hotspot_ssid` and
        `wifi/hotspot_psk`. This is the way back in when the campus password
        changes: with the old one wpa_supplicant fails to authenticate, falls
        through to the hotspot, and the rig is on the mesh again so the new
        password can be pushed to it over SSH.
      '';
      type = types.bool;
      default = true;
    };
  };

  config = mkIf cfg.enable {
    networking.wireless = {
      enable = true;
      secretsFile = config.sops.templates."wifi/secrets.env".path;
      extraConfigFiles = [ config.sops.templates."wifi/networks.conf".path ];

      # Rigs are unattended; there is nobody to pick a network at a prompt.
      allowAuxiliaryImperativeNetworks = false;

      # The default, spelled out because it is load-bearing. With no
      # `networks` and an empty extraConfig the NixOS module decides the
      # config is imperative, makes a writable imperative.conf the main file
      # and drops nixos.conf — and with it the secretsFile backend.
      extraConfig = "ap_scan=1";
    };

    # Ethernet, when it works, is used exclusively: wpa_supplicant is stopped
    # while a wired link has a default route and started again when it loses
    # it. Re-evaluated on every link state change, not just the wired one's,
    # since the wired link going routable and its DHCP lease landing are
    # separate events.
    services.networkd-dispatcher = {
      enable = true;
      rules."wifi-yields-to-ethernet" = {
        onState = [
          "routable"
          "degraded"
          "carrier"
          "no-carrier"
          "off"
        ];
        script = ''
          if ${wiredUp}; then
            ${config.systemd.package}/bin/systemctl stop wpa_supplicant.service
          else
            ${config.systemd.package}/bin/systemctl start wpa_supplicant.service
          fi
        '';
      };
    };

    # The same check on wpa_supplicant's own start, so a sops secret change
    # restarting it does not bring wifi back up beside a working cable. `+`
    # runs it outside the daemon's sandbox, which cannot see the routes.
    systemd.services.wpa_supplicant.serviceConfig.ExecCondition =
      "+${pkgs.writeShellScript "rig-wired-down" "! ${wiredUp}"}";

    sops = {
      secrets = {
        "wifi/mbzuai_identity" = { };
        "wifi/mbzuai_password" = { };
      }
      // lib.optionalAttrs cfg.fallbackHotspot {
        "wifi/hotspot_ssid" = { };
        "wifi/hotspot_psk" = { };
      };

      # Both templates are owned by wpa_supplicant, not root. The NixOS module
      # runs the daemon as its own unprivileged user, so a root-only file fails
      # at exactly the wrong moment: association and certificate validation
      # both succeed, and it dies at the password lookup with
      #   EXT PW FILE: could not open file '...': Permission denied
      # which reads like a credentials problem and is not one.

      # The campus password goes through `ext:` rather than straight into the
      # network block: the file backend takes the value verbatim to the end of
      # the line, where a quoted string would break on an SSO password with a
      # `"` in it.
      templates."wifi/secrets.env" = {
        owner = "wpa_supplicant";
        restartUnits = [ "wpa_supplicant.service" ];
        content = ''
          mbzuai_password=${secret "mbzuai_password"}
        '';
      };

      # The hotspot sits below campus, so it is only used when campus fails or
      # is out of range. WPA2 and WPA3, since phones default to either; its
      # passphrase is inline because SAE does not take one through `ext:`.
      templates."wifi/networks.conf" = {
        owner = "wpa_supplicant";
        restartUnits = [ "wpa_supplicant.service" ];
        content = ''
          network={
            ssid="MBZUAI-STUDENT"
            key_mgmt=WPA-EAP
            eap=PEAP
            identity="${secret "mbzuai_identity"}"
            password=ext:mbzuai_password
            phase2="auth=MSCHAPV2"
            priority=10
          }
        ''
        + optionalString cfg.fallbackHotspot ''
          network={
            ssid="${secret "hotspot_ssid"}"
            psk="${secret "hotspot_psk"}"
            key_mgmt=WPA-PSK SAE
            ieee80211w=1
            priority=-10
          }
        '';
      };
    };
  };
}
