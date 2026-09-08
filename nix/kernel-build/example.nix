# A hand-drivable kernel build, for working on ./default.nix without a daemon
# in the way. `rig-buildd` calls the same function with values off the wire;
# this is the same thing with values on the command line.
#
#   nix build -f nix/kernel-build/example.nix --argstr commit <sha> -L
#   nix build -f nix/kernel-build/example.nix --argstr commit <sha> \
#     --argstr patchFile ./fix.diff -L
#
# `repo` is a local bare mirror because a kernel build should not be waiting on
# git.kernel.org, and because this is exactly what a rig will have: fetching
# 3.5GB per job is the thing the mirror exists to avoid.
{
  pkgs ? import <nixpkgs> { },
  repo ? "/home/humaid/repos/ai4os/linux_source_code/linux.git",
  commit ? "7d0a66e4bb9081d75c82ec4957c50034cb0ea449", # v6.18
  configBase ? "defconfig",
  patchFile ? null,
}:
let
  kernelBuild = pkgs.callPackage ./. { };
in
kernelBuild.build {
  inherit commit configBase;

  # allRefs because a caller pins a commit, not a branch head, and without it
  # anything that is not currently a ref tip fails to resolve.
  src = builtins.fetchGit {
    url = "file://${repo}";
    rev = commit;
    allRefs = true;
  };

  # Only what the caller actually cares about. Naming a symbol Kconfig derives
  # for you is a good way to demand something impossible: ZRAM selects ZSMALLOC
  # as a module, so asking for ZSMALLOC=y alongside ZRAM=m is a contradiction
  # the verification step will (correctly) refuse to build.
  configSet = {
    ZRAM = "m";
  };

  patch = if patchFile == null then null else builtins.readFile patchFile;
}
