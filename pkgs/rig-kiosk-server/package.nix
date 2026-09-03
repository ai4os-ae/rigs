{
  lib,
  buildGoModule,
  qrencode,
}:
let
  # The QR the binary falls back to when nothing passes it one, so that running
  # this on a laptop shows the same page a rig does. A rig's own QR is built by
  # the NixOS module from `rigs.kiosk.link`, which is what makes that option
  # mean anything.
  defaultLink = "https://github.com/ai4os-ae";
in
buildGoModule {
  pname = "rig-kiosk-server";
  version = "0.1.0";

  src = ./.;

  # Standard library only, deliberately: this runs unattended on every rig, so
  # a dependency here is a dependency to keep patched. Keep it that way — the
  # QR code is drawn by qrencode at build time for the same reason.
  vendorHash = null;

  nativeBuildInputs = [ qrencode ];

  ldflags = [
    "-s"
    "-w"
    "-X"
    "main.defaultQR=${placeholder "out"}/share/rig-kiosk-server/qr.svg"
  ];

  postInstall = ''
    mkdir -p $out/share/rig-kiosk-server
    qrencode --type=SVG --level=M --margin=0 \
      --output=$out/share/rig-kiosk-server/qr.svg \
      ${lib.escapeShellArg defaultLink}
  '';

  meta = {
    description = "Serves the status page shown on a rig's own screen";
    mainProgram = "rig-kiosk-server";
    platforms = lib.platforms.linux;
  };
}
