# Disk layout for desktop-01, applied by disko at install time.
{
  disko.devices = {
    disk = {
      root = {
        type = "disk";
        # By id rather than /dev/nvme0n1: the install runs from a USB SSD, and
        # enumeration order is not something to bet a wipe on.
        device = "/dev/disk/by-id/nvme-PM9F1_SED_Samsung_2048GB__S7NRNE0Y405930";
        content = {
          type = "gpt";
          partitions = {
            ESP = {
              size = "1G";
              type = "EF00";
              content = {
                type = "filesystem";
                format = "vfat";
                mountpoint = "/boot";
                mountOptions = [ "umask=0077" ];
              };
            };
            root = {
              size = "100%";
              content = {
                type = "btrfs";
                extraArgs = [ "-f" ];
                subvolumes = {
                  "/root" = {
                    mountpoint = "/";
                    mountOptions = [
                      "compress=zstd"
                      "noatime"
                    ];
                  };
                  "/nix" = {
                    mountpoint = "/nix";
                    mountOptions = [
                      "compress=zstd"
                      "noatime"
                    ];
                  };
                  "/home" = {
                    mountpoint = "/home";
                    mountOptions = [
                      "compress=zstd"
                      "noatime"
                    ];
                  };
                  # Job outputs and datasets. Separate so a runaway job fills a
                  # subvolume that can be reclaimed on its own rather than
                  # taking the root filesystem down with it.
                  "/var/lib/rig" = {
                    mountpoint = "/var/lib/rig";
                    mountOptions = [
                      "compress=zstd"
                      "noatime"
                    ];
                  };
                };
              };
            };
          };
        };
      };
    };
  };
}
