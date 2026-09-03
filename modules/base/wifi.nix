# Wireless for rigs that have no ethernet port — which, for a laptop rig, is
# most of them. Without this such a rig comes up after install with no network
# at all and no way to reach it.
#
# Passwords never enter the Nix store. wpa_supplicant reads them at runtime
# from `secretsFile`, which sops decrypts into /run; the repo only ever names
# the variable. That matters more than usual here because campus credentials
# are SSO credentials — the same password as mail and the student lab.
{
  config,
  lib,
  ...
}:
let
  cfg = config.rigs.wifi;
  inherit (lib)
    mkOption
    mkEnableOption
    mkIf
    types
    mapAttrs
    ;
in
{
  options.rigs.wifi = {
    enable = mkEnableOption "wireless networking via wpa_supplicant";

    secretsFile = mkOption {
      description = ''
        Path to a file of `name=value` lines, one per password referenced below.
        Normally a sops secret, so the plaintext exists only in /run on the rig
        itself.
      '';
      type = types.nullOr types.path;
      default = null;
    };

    networks = mkOption {
      description = "Wireless networks this rig should join, by SSID.";
      default = { };
      type = types.attrsOf (
        types.submodule {
          options = {
            enterprise = mkOption {
              description = ''
                WPA-EAP settings, for a campus or corporate network. Null means
                an ordinary WPA-PSK network.
              '';
              default = null;
              type = types.nullOr (
                types.submodule {
                  options = {
                    identity = mkOption {
                      description = "EAP identity, usually user@domain.";
                      type = types.str;
                    };
                    passwordSecret = mkOption {
                      description = "Variable name to look up in secretsFile.";
                      type = types.str;
                    };
                    eap = mkOption {
                      description = "Outer EAP method.";
                      type = types.str;
                      default = "PEAP";
                    };
                    phase2 = mkOption {
                      description = "Inner authentication method.";
                      type = types.str;
                      default = "auth=MSCHAPV2";
                    };
                  };
                }
              );
            };

            pskSecret = mkOption {
              description = ''
                Variable name in secretsFile holding the passphrase, for a
                plain WPA-PSK network. Ignored when `enterprise` is set.
              '';
              type = types.nullOr types.str;
              default = null;
            };

            priority = mkOption {
              description = "Higher wins when several known networks are in range.";
              type = types.int;
              default = 0;
            };
          };
        }
      );
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.networks == { } || cfg.secretsFile != null;
        message = "rigs.wifi.secretsFile must be set when rigs.wifi.networks is non-empty.";
      }
    ];

    networking.wireless = {
      enable = true;
      inherit (cfg) secretsFile;

      # Rigs are unattended; there is nobody to pick a network at a prompt.
      allowAuxiliaryImperativeNetworks = false;

      networks = mapAttrs (
        _ssid: n:
        {
          inherit (n) priority;
        }
        // (
          if n.enterprise != null then
            {
              # `ext:` is what keeps the password out of the store — it tells
              # wpa_supplicant to resolve the value from secretsFile at
              # connect time rather than from this generated config.
              auth = ''
                key_mgmt=WPA-EAP
                eap=${n.enterprise.eap}
                identity="${n.enterprise.identity}"
                password=ext:${n.enterprise.passwordSecret}
                phase2="${n.enterprise.phase2}"
              '';
            }
          else
            { pskRaw = "ext:${n.pskSecret}"; }
        )
      ) cfg.networks;
    };
  };
}
