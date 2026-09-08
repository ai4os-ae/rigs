# A hand-drivable test run, for working on ./default.nix without a daemon in
# the way. `rig-buildd` calls the same function with values off the wire.
#
#   nix build -f nix/kernel-test/example.nix -L
#   nix build -f nix/kernel-test/example.nix -L --arg kselftest false
#   nix build -f nix/kernel-test/example.nix -L --argstr scriptFile ./mytest.py
#
# The kernel is built by nix/kernel-build from the same local mirror its own
# example.nix uses, so the first run here pays for a kernel build and every run
# after it does not.
{
  pkgs ? import <nixpkgs> { },
  repo ? "/home/humaid/repos/ai4os/linux_source_code/linux.git",
  commit ? "7d0a66e4bb9081d75c82ec4957c50034cb0ea449", # v6.18
  configBase ? "defconfig",
  patchFile ? null,
  scriptFile ? null,
  kselftest ? true,
}:
let
  kernelBuild = pkgs.callPackage ../kernel-build { };
  kernelTest = pkgs.callPackage ./. { };

  kernel = kernelBuild.build {
    inherit commit configBase;

    src = builtins.fetchGit {
      url = "file://${repo}";
      rev = commit;
      allRefs = true;
    };

    configSet = {
      ZRAM = "m";
    };

    patch = if patchFile == null then null else builtins.readFile patchFile;
  };
in
kernelTest.run {
  inherit kernel kselftest;

  name = "linux-${builtins.substring 0 12 commit}";

  modules = [ "zram" ];

  script = if scriptFile == null then null else builtins.readFile scriptFile;

  timeoutSeconds = 900;
}
