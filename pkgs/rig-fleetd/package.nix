{
  lib,
  buildGoModule,
}:
buildGoModule {
  pname = "rig-fleetd";
  version = "0.1.0";

  src = ./.;

  # Standard library only, deliberately, as with the rest of the services here.
  # This one is published, so a dependency is not only something to keep
  # patched but something reachable from the internet.
  vendorHash = null;

  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "One endpoint in front of the rigs: places build jobs and proxies them";
    mainProgram = "rig-fleetd";
    platforms = lib.platforms.linux;
  };
}
