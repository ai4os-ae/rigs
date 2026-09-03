# The throwaway environment a rig is installed from: kernel + initrd, with the
# whole store embedded in the initrd, so it needs no root filesystem and no
# boot media of its own.
#
# Two ways in, neither needing a USB stick:
#
#   - kexec, from whatever Linux is already on the target's disk. The store is
#     in RAM, so the disk is free to be repartitioned the moment the new kernel
#     takes over. This is what nixos-anywhere drives, and it is the usual route.
#   - netboot (PXE / UEFI HTTP), for a target with a wired link and nothing
#     usable on its disk.
#
# Deliberately standalone rather than built on modules/base — the upstream
# `installation-device` profile brings its own root account and mutableUsers
# handling, and reconciling that with the roster buys nothing for something
# that exists only to be booted once and thrown away.
{
  modulesPath,
  lib,
  ...
}:
let
  # The roster is a plain attrset, so it can be read as data here without
  # dragging in the rest of the module system.
  people = (import ../modules/base/people.nix).rigs.users;
  adminKeys = lib.concatMap (u: u.sshKeys or [ ]) (
    lib.attrValues (lib.filterAttrs (_: u: (u.admin or false) && (u.enable or true)) people)
  );

  # Optional, gitignored: { ssid = "..."; psk = "..."; }. Present when the
  # installer has to rejoin a wireless network by itself after the kexec —
  # see the comment on networking.wireless below.
  wifiFile = ../secrets/installer-wifi.nix;
  wifi = if builtins.pathExists wifiFile then import wifiFile else null;

  # Optional, gitignored: a list of extra public keys for this image only.
  #
  # The roster's keys are FIDO tokens that require a physical touch on every
  # signature. That is right for a rig and wrong for an installer, which has to
  # be reconnected to unattended the moment it comes back from the kexec — a
  # touch-required key there means the install stalls waiting for a fingertip
  # that the tooling cannot ask for. Put a throwaway keypair here for the
  # duration of an install and delete it afterwards.
  extraKeysFile = ../secrets/installer-keys.nix;
  extraKeys = if builtins.pathExists extraKeysFile then import extraKeysFile else [ ];
in
{
  imports = [ (modulesPath + "/installer/netboot/netboot-minimal.nix") ];

  # netboot-minimal turns this OFF (mkOverride 70) to keep the image small.
  # That is wrong for us and quietly so: without redistributable firmware there
  # is no iwlwifi firmware, so a target with no ethernet port has no network at
  # all once it kexecs — and no way to tell you why. Thunderbolt and most NICs
  # need it too. mkForce because 70 already beats a plain mkDefault.
  hardware.enableRedistributableFirmware = lib.mkForce true;

  # The only account that matters here. Without these keys the installer boots
  # to a login prompt and nothing can drive the install remotely.
  users.users.root.openssh.authorizedKeys.keys = adminKeys ++ extraKeys;

  services.openssh = {
    enable = true;
    settings.PermitRootLogin = lib.mkForce "prohibit-password";
  };

  networking.useDHCP = lib.mkDefault true;

  # Thunderbolt/USB4 host-to-host networking. Two laptops joined by a USB4
  # cable get a real point-to-point link this way, which is the only wired
  # option on machines that have no ethernet port — and it sidesteps client
  # isolation on a shared wireless network entirely.
  #
  # The address is fixed rather than DHCP because the install survives a kexec:
  # the SSH session driving it has to come back at the same address afterwards,
  # and there is no DHCP server on a two-machine cable. Set the other end to
  # 10.99.0.1/24.
  boot.kernelModules = [ "thunderbolt-net" ];

  # Declared through `networking.interfaces`, NOT `systemd.network`. The
  # upstream installation-device profile uses scripted networking with dhcpcd
  # and leaves networkd off, so a systemd.network block here is silently inert
  # — the interface comes up with no address, the post-kexec session never
  # reconnects, and the install strands with the disk already repartitioned.
  #
  # Keyed on the interface name because scripted networking has no equivalent
  # of networkd's driver match. `thunderbolt0` is what the thunderbolt-net
  # driver names the first host-to-host link, confirmed on this hardware.
  networking.interfaces.thunderbolt0.ipv4.addresses = [
    {
      address = "10.99.0.2";
      prefixLength = 24;
    }
  ];

  # Auto-authorise Thunderbolt peers.
  #
  # This is load-bearing, not a convenience. Thunderbolt controllers ship at
  # security level `user`, where a newly attached peer stays dead until
  # something explicitly authorises it — normally the `bolt` daemon, prompting
  # a human at a desktop. The kexec re-enumerates the bus, so the link that
  # carried the install *drops and has to be re-authorised* by an installer
  # that has no bolt and no human. Without this rule the install strands with
  # the target unreachable, which is the worst possible moment for it.
  #
  # The tradeoff is the usual Thunderbolt one — an authorised peer can DMA —
  # and it is accepted narrowly: this image exists for a few minutes, on a
  # cable somebody just plugged in themselves, on hardware where the IOMMU is
  # doing the real work. It is not a setting any rig carries afterwards.
  services.udev.extraRules = ''
    ACTION=="add", SUBSYSTEM=="thunderbolt", ATTR{authorized}=="0", ATTR{authorized}="1"
  '';

  # Wireless is the fallback when there is no usable cable. Credentials have to
  # be baked in rather than typed at a console: the kexec tears the link down,
  # and an installer that cannot rejoin the network on its own strands the
  # install half-done with no way back in.
  #
  # This puts a PSK in the initrd, which is why the file is gitignored and why
  # the intended use is a throwaway hotspot torn down after the install, not a
  # long-lived network's key.
  networking.wireless = lib.mkIf (wifi != null) {
    enable = true;
    networks.${wifi.ssid}.psk = wifi.psk;
  };

  # netboot-base turns on enableAllHardware, which brings ZFS with it. Nothing
  # here imports a pool, and the default becomes false upstream in 26.11 —
  # setting it now silences the warning without changing behaviour.
  boot.zfs.forceImportRoot = false;

  system.stateVersion = "26.05";
}
