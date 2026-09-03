# The roster. Every rig gets every person listed here, so onboarding is one
# block in this file plus a rebuild of each rig.
#
# uids are handed out from 1000 upwards and are never reused: set `enable =
# false` when somebody leaves rather than deleting their block, so a stale file
# owned by 1002 can never come back as somebody else's.
#
# Keys are public halves only. Nothing secret belongs in this file.
{
  rigs.users = {
    humaid = {
      fullname = "Humaid Alqasimi";
      uid = 1000;
      admin = true;
      sshKeys = [
        # FIDO tokens, carried over from the personal dotfiles repo. Confirm
        # these are the pair you want the rigs to accept before the first
        # install — they are what root's authorized_keys is built from.
        "sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIC+JivWVZLN5Q+gQp+Y+YOHr0tglTPujT5uqz0Vk//YnAAAABHNzaDo= HK05"
        "sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIBDT3fTXfORHii5qehplQUj0JQztBhELP9D+22/8cg+9AAAAD3NzaDpodW1haWQtYW5vYQ== humaid-nano-anoa-ssh-git"
      ];
    };

    # Template for the next person. Fill in the key and drop `enable = false`.
    #
    # jiawen = {
    #   fullname = "Jiawen ...";
    #   uid = 1001;
    #   admin = false;
    #   enable = false;
    #   sshKeys = [ ];
    # };
  };
}
