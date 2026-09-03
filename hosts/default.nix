{
  inputs,
  self,
  lib,
  ...
}:
{
  flake =
    let
      specialArgs = { inherit self inputs; };
    in
    {
      nixosModules = {
        host-laptop-01 = import ./laptop-01;
        host-installer = import ./installer.nix;
      };

      nixosConfigurations = {
        laptop-01 = lib.nixosSystem {
          inherit specialArgs;
          modules = [
            self.nixosModules.host-laptop-01
            inputs.srvos.nixosModules.server
          ];
        };

        # Not a rig. The throwaway environment a rig is installed from.
        installer = lib.nixosSystem {
          system = "x86_64-linux";
          inherit specialArgs;
          modules = [ self.nixosModules.host-installer ];
        };
      };

      packages.x86_64-linux =
        let
          installer = self.nixosConfigurations.installer;
          inherit (installer.config.system) build;
        in
        {
          # kernel + initrd + an iPXE script, for serving over PXE or UEFI HTTP
          # boot. The store lives inside the initrd, so these two files are the
          # whole system — nothing is fetched after the kernel starts.
          netboot = installer.pkgs.symlinkJoin {
            name = "netboot";
            paths = [
              build.kernel
              build.netbootRamdisk
              build.netbootIpxeScript
            ];
            preferLocalBuild = true;
          };

          # The same kernel and initrd plus a kexec-boot script, for a target
          # that already runs Linux: copy, run, and the machine replaces its
          # running kernel with this installer without touching the disk or the
          # firmware boot order.
          #
          # Note the script calls a bare `kexec`, so the target needs
          # kexec-tools installed — this tree is not self-contained.
          kexec = build.kexecTree;

          # The same thing packed for `nixos-anywhere --kexec`. Worth passing
          # explicitly: left to itself nixos-anywhere fetches the generic
          # nixos-images installer, which knows nothing about this rig's
          # Thunderbolt addressing, so the link would come back on a different
          # address after the kexec and the install would lose its own session.
          inherit (build) kexecTarball;
        };

      hydraJobs.x86_64-linux = {
        laptop-01 = self.nixosConfigurations.laptop-01.config.system.build.toplevel;
        inherit (self.packages.x86_64-linux) netboot kexec;
      };
    };
}
