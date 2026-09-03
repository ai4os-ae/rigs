{ inputs, lib, ... }:
let
  # Guarded because the file does not exist until the first secret is created,
  # and a `sops.defaultSopsFile` pointing at a missing path fails evaluation —
  # which would make the repository unbuildable before it has any secrets at
  # all. Drop the guard once secrets/all.yaml is committed.
  allSecrets = ../../secrets/all.yaml;
in
{
  imports = [ inputs.sops-nix.nixosModules.sops ];

  sops = {
    defaultSopsFile = lib.mkIf (builtins.pathExists allSecrets) allSecrets;
    defaultSopsFormat = "yaml";

    # Derived from the rig's SSH host key rather than generated separately, so
    # a rig's identity is whatever it already proved when you first SSH'd in.
    # `ssh-to-age < /etc/ssh/ssh_host_ed25519_key.pub` gives the public half to
    # paste into .sops.yaml.
    age.sshKeyPaths = [ "/etc/ssh/ssh_host_ed25519_key" ];
  };
}
