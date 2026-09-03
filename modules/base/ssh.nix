{ lib, ... }:
{
  services.openssh = {
    enable = true;

    settings = {
      # Every account here is key-only by construction (see users.nix), so a
      # password prompt on a public-facing rig can only ever be answered by
      # somebody guessing.
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;

      # prohibit-password rather than "no": remote rebuilds and nixos-anywhere
      # both need root over SSH, and admins' keys are already in root's
      # authorized_keys.
      PermitRootLogin = lib.mkDefault "prohibit-password";
    };
  };

  # Every rig gets an ed25519 host key, which is also the key sops derives its
  # age identity from. The RSA key srvos would otherwise leave enabled is not
  # used for anything here.
  services.openssh.hostKeys = [
    {
      path = "/etc/ssh/ssh_host_ed25519_key";
      type = "ed25519";
    }
  ];
}
