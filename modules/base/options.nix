{ lib, ... }:
let
  inherit (lib) mkOption types;
in
{
  options.rigs = {
    timezone = mkOption {
      description = "System timezone.";
      type = types.str;
      default = "Asia/Dubai";
    };

    projectFlake = mkOption {
      description = ''
        Flake reference this rig rebuilds itself from. Used by the nightly
        upgrade and by the `rig-rebuild` helper.
      '';
      type = types.nullOr types.str;
      default = null;
    };

    bootstrap = mkOption {
      description = ''
        Bootstrap mode, for a rig that does not have a sops age key yet.

        Passwords come from sops, and a rig has no age key until its SSH host
        key exists — which is only after the first install. With this on, admin
        accounts get a known placeholder password so the first console login
        works; with it off, accounts are SSH-key-only unless a password secret
        is named. Turn it off once the host is in .sops.yaml.
      '';
      type = types.bool;
      default = false;
    };
  };
}
