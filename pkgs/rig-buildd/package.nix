{
  lib,
  buildGoModule,
}:
buildGoModule {
  pname = "rig-buildd";
  version = "0.1.0";

  src = ./.;

  # Standard library only, deliberately, for the same reason as
  # rig-kiosk-server: this runs unattended on every rig, so a dependency here
  # is a dependency to keep patched. Keep it that way.
  vendorHash = null;

  ldflags = [
    "-s"
    "-w"
  ];

  # `git` and `nix` are shelled out to rather than baked in: the mirror is
  # ordinary git, and the nix that runs a build has to be the machine's own, not
  # one pinned in this derivation.
  meta = {
    description = "Runs build jobs on a rig and publishes them over HTTP";
    mainProgram = "rig-buildd";
    platforms = lib.platforms.linux;
  };
}
