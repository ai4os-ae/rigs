{ inputs, ... }:
{
  imports = [ inputs.sops-nix.nixosModules.sops ];

  sops = {
    # Secrets shared by every rig — currently the account password hashes,
    # which follow the roster in people.nix and so are fleet-wide by the same
    # logic. Per-rig material names its own file instead; see the nebula and
    # wifi secrets on laptop-01.
    defaultSopsFile = ../../secrets/all.yaml;
    defaultSopsFormat = "yaml";

    # Derived from the rig's SSH host key rather than generated separately, so
    # a rig's identity is whatever it already proved when you first SSH'd in.
    # `ssh-to-age < /etc/ssh/ssh_host_ed25519_key.pub` gives the public half to
    # paste into .sops.yaml.
    age.sshKeyPaths = [ "/etc/ssh/ssh_host_ed25519_key" ];
  };
}
