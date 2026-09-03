{
  config,
  lib,
  ...
}:
let
  cfg = config.rigs;
  inherit (lib)
    mkOption
    mkEnableOption
    types
    mapAttrs
    filterAttrs
    attrNames
    concatStringsSep
    ;

  admins = filterAttrs (_: u: u.admin) cfg.users;

  # Placeholder used only in bootstrap mode. Same hash as the one in the
  # dotfiles repo; it is a known value and is not a secret, which is the whole
  # point — it exists so a freshly installed rig can be logged into at the
  # console before it has an age key.
  bootstrapPassword = "$6$67sQfb8Pm3Jyvdvo$OPXnLbgHCdoRfhlhhz/pygvJ32ZA.L0HifV.fBSVW47SsfKK6xiroi/Xx.hcB6YJ94XXaiUH5zqDvnAmKq6gE1";
in
{
  options.rigs = {
    users = mkOption {
      description = ''
        People with accounts on the rigs. Declared once here and applied to
        every host, so adding a researcher is a one-line change rather than a
        per-host edit.
      '';
      default = { };
      type = types.attrsOf (
        types.submodule (
          { name, ... }:
          {
            options = {
              fullname = mkOption {
                description = "Human name, used as the account's GECOS field.";
                type = types.str;
              };

              uid = mkOption {
                description = ''
                  Fixed uid. Pinned rather than allocated so that a home
                  directory copied between rigs, or restored from a backup,
                  keeps its ownership.
                '';
                type = types.int;
              };

              admin = mkEnableOption "wheel membership, i.e. root on every rig";

              sshKeys = mkOption {
                description = ''
                  Public keys accepted for this account. SSH is the only way in
                  — password authentication is off — so an account with an
                  empty list here cannot log in at all.
                '';
                type = types.listOf types.str;
                default = [ ];
              };

              passwordSecret = mkOption {
                description = ''
                  Name of the sops secret holding this account's hashed
                  password, relative to the rig's secrets file. Null leaves the
                  account with no password at all: SSH-key login works, console
                  login does not, and `su` to it is impossible.
                '';
                type = types.nullOr types.str;
                default = null;
              };

              extraGroups = mkOption {
                description = "Additional groups beyond the standard set.";
                type = types.listOf types.str;
                default = [ ];
              };

              enable = mkOption {
                description = ''
                  Whether this account exists. Set to false rather than
                  deleting the block when someone leaves, so the uid is not
                  handed to the next person.
                '';
                type = types.bool;
                default = true;
              };
            };

            config = {
              # Reasonable default so a one-word entry is enough to add someone.
              fullname = lib.mkDefault name;
            };
          }
        )
      );
    };
  };

  config = {
    assertions = [
      {
        assertion = admins != { };
        message = "rigs.users must contain at least one admin account.";
      }
      {
        # A rig is a remote machine with no console anybody will walk up to.
        # An admin with no key and no password is a machine nobody can reach,
        # and it is far cheaper to catch that here than after the install.
        assertion =
          cfg.bootstrap
          || lib.all (u: u.sshKeys != [ ]) (lib.attrValues (filterAttrs (_: u: u.enable) cfg.users));
        message =
          "These accounts have no SSH keys and so cannot log in: "
          + concatStringsSep ", " (attrNames (filterAttrs (_: u: u.enable && u.sshKeys == [ ]) cfg.users))
          + ". Add keys, set enable = false, or turn on rigs.bootstrap.";
      }
    ];

    users = {
      # Accounts come from this file and nowhere else; useradd on a rig is a
      # change that the next rebuild silently undoes.
      mutableUsers = false;

      users = {
        # Root is reachable with any admin's key, which is what makes
        # `nixos-rebuild --target-host` and nixos-anywhere work without a
        # password prompt on a machine nobody is sitting in front of.
        root.openssh.authorizedKeys.keys = lib.concatMap (u: u.sshKeys) (lib.attrValues admins);
      }
      // mapAttrs (
        _name: u:
        {
          isNormalUser = true;
          inherit (u) uid;
          description = u.fullname;
          group = "users";
          extraGroups = [
            "dialout"
            "video"
            "kvm"
          ]
          ++ lib.optional u.admin "wheel"
          ++ u.extraGroups;
          openssh.authorizedKeys.keys = u.sshKeys;
        }
        // (
          if cfg.bootstrap && u.admin then
            { hashedPassword = bootstrapPassword; }
          else if u.passwordSecret != null then
            { hashedPasswordFile = config.sops.secrets.${u.passwordSecret}.path; }
          else
            { hashedPassword = "!"; }
        )
      ) (filterAttrs (_: u: u.enable) cfg.users);
    };

    warnings = lib.optional cfg.bootstrap ''
      rigs.bootstrap is enabled on ${config.networking.hostName}: admin accounts
      have a known placeholder password. Turn it off once the host's age key is
      in .sops.yaml.
    '';
  };
}
