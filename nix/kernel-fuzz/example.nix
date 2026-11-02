# A hand-drivable fuzzing environment, for working on ./default.nix without a
# daemon in the way. `rig-buildd` calls the same function with values off the
# wire; this is the same thing with values on the command line.
#
#   ssh-keygen -t ed25519 -N "" -f /tmp/rig-fuzz
#   nix build -f nix/kernel-fuzz/example.nix -L
#   nix build -f nix/kernel-fuzz/example.nix -L --argstr commit <sha>
#
# That keypair is the one syz-manager authenticates with: its public half is
# baked into the guest here, and /tmp/rig-fuzz is what the manager config's
# `sshkey` points at. A rig generates its own once and keeps it in the daemon's
# state directory; nothing of the sort is committed.
#
# Then run it by hand. There is no syz-init here on purpose — writing the
# manager config is rig-buildd's job, and doing it twice is how the two
# versions come to disagree:
#
#   jq -n --arg env "$(readlink -f result)" --arg wd "$PWD/workdir" '
#     $env as $e | ($e + "/env.json" | @text) as $_ | {}' # see result/env.json
#
# In practice: read result/env.json, drop its values into a syzkaller.cfg
# alongside a workdir and an sshkey, and run `syz-manager -config`. The README
# section on fuzz jobs has a worked example.
#
# NOTE this builds a KASAN + KCOV kernel, which is roughly twice the build time
# and twice the vmlinux of the defconfig that nix/kernel-build/example.nix
# builds — and it shares nothing with it, because the configuration differs and
# therefore so does the base derivation. Budget an hour for the first one.
{
  pkgs ? import <nixpkgs> { },
  repo ? "/home/humaid/repos/ai4os/linux_source_code/linux.git",
  commit ? "7d0a66e4bb9081d75c82ec4957c50034cb0ea449", # v6.18
  configBase ? "defconfig",
  patchFile ? null,
  # The key you will hand syz-manager as `sshkey`. Its private half never
  # appears in a derivation: on a rig this is generated once by the daemon and
  # kept in its state directory.
  authorizedKeyFile ? "/tmp/rig-fuzz.pub",
}:
let
  kernelBuild = pkgs.callPackage ../kernel-build { };
  kernelFuzz = pkgs.callPackage ./. {
    syzkaller = pkgs.callPackage ../../pkgs/syzkaller/package.nix { };
  };

  kernel = kernelBuild.build {
    inherit commit configBase;

    src = builtins.fetchGit {
      url = "file://${repo}";
      rev = commit;
      allRefs = true;
    };

    # The caller's overrides go in here, and kernel-fuzz decides what survives
    # them: KCOV and the boot-critical symbols are forced on top, KASAN and
    # friends are defaults underneath. See ./config.nix.
    configSet = kernelFuzz.configSetFor {
      # E1000 is already forced on — it is how syz-manager reaches the VM at
      # all — so this is the interesting half of the e1000 focus profile:
      # nothing else is needed to make the driver present.
      CONFIG_E1000 = "y";
    };

    patch = if patchFile == null then null else builtins.readFile patchFile;
  };
in
kernelFuzz.env {
  inherit kernel;

  name = "linux-${builtins.substring 0 12 commit}";

  authorizedKey = builtins.readFile authorizedKeyFile;
}
