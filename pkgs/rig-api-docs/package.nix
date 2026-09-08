# The browsable API documentation, as a directory an nginx can serve as its
# root. It is static: the spec is written by hand and checked in, so it is
# reviewed like the code it describes rather than generated from it and
# believed.
{
  lib,
  runCommand,
  fetchzip,
  yq-go,
}:
let
  version = "5.32.15";

  # Vendored rather than loaded from a CDN. The rigs are reachable from the
  # mesh and from one public name, and a docs page that quietly needs the
  # wider internet is a docs page that fails in the lab — which is where
  # somebody is most likely to be reading it.
  swaggerUI = fetchzip {
    url = "https://registry.npmjs.org/swagger-ui-dist/-/swagger-ui-dist-${version}.tgz";
    hash = "sha256-yGQBG7ChkiUXMuhG4OtTDAjTMZJsgsABgihqeoZ1e10=";
  };
in
runCommand "rig-api-docs"
  {
    nativeBuildInputs = [ yq-go ];

    meta = {
      description = "Browsable OpenAPI documentation for the rig build API";
      platforms = lib.platforms.all;
    };
  }
  ''
    mkdir -p $out

    # A malformed spec renders as an empty page with the reason only in the
    # browser console, so it is parsed here instead: a typo fails the build
    # rather than the deployment.
    yq -p yaml -o json '.' ${./openapi.yaml} > /dev/null

    cp ${./openapi.yaml} $out/openapi.yaml
    cp ${./index.html} $out/index.html

    # Only what the page loads. The dist tarball is 5.4MB, most of it source
    # maps and the bundles for a standalone app this does not use.
    for asset in \
      swagger-ui.css \
      swagger-ui-bundle.js \
      swagger-ui-standalone-preset.js \
      favicon-32x32.png \
      favicon-16x16.png
    do
      cp ${swaggerUI}/$asset $out/$asset
    done
  ''
