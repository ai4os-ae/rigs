{
  description = "ai4os-rigs: NixOS configurations for the AI4OS research rigs";

  nixConfig = {
    show-trace = true;
    lazy-trees = true;
    warn-dirty = false;

    experimental-features = [
      "flakes"
      "nix-command"
      "pipe-operators"
      "auto-allocate-uids"
    ];
    allow-import-from-derivation = false;
  };

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-26.05";
    nixos-hardware.url = "github:nixos/nixos-hardware";

    flake-parts.url = "github:hercules-ci/flake-parts";
    flake-root.url = "github:srid/flake-root";

    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    flake-compat = {
      url = "github:inclyc/flake-compat";
      flake = false;
    };

    srvos = {
      url = "github:nix-community/srvos";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    sops-nix = {
      url = "github:Mic92/sops-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    disko = {
      url = "github:nix-community/disko";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    inputs@{ nixpkgs, flake-parts, ... }:
    let
      flakeOutputs =
        flake-parts.lib.mkFlake
          {
            inherit inputs;
            specialArgs = {
              inherit (nixpkgs) lib;
            };
          }
          {
            imports = [
              inputs.flake-root.flakeModule
              inputs.treefmt-nix.flakeModule
              ./hosts
            ];
            systems = [
              "x86_64-linux"
              "aarch64-linux"
            ];
            perSystem =
              {
                config,
                system,
                pkgs,
                ...
              }:
              {
                _module.args = {
                  pkgs = import inputs.nixpkgs {
                    inherit system inputs;
                    config = {
                      allowUnfree = true;
                    };
                  };
                };

                # The status page the kiosk session displays. Exposed so it can
                # be built and run on a laptop — `nix run .#rig-kiosk-server`
                # then open http://127.0.0.1:8000 — without deploying a rig to
                # look at a layout change.
                packages.rig-kiosk-server = pkgs.callPackage ./pkgs/rig-kiosk-server/package.nix { };

                # Everything needed to bring a rig up and to rotate its
                # secrets. `ssh-to-age` is the one non-obvious entry: host age
                # keys here are derived from each rig's SSH host key rather
                # than generated separately, so adding a rig to .sops.yaml runs
                # through it.
                devShells.default = pkgs.mkShell {
                  inputsFrom = [ config.flake-root.devShell ];
                  packages = with pkgs; [
                    age
                    sops
                    ssh-to-age
                    mkpasswd
                    nixos-rebuild
                    nix-output-monitor
                    nixos-anywhere
                    deadnix
                    statix
                  ];
                };

                treefmt.config = {
                  package = pkgs.treefmt;
                  inherit (config.flake-root) projectRootFile;
                  programs = {
                    nixfmt.enable = true;
                    nixfmt.package = pkgs.nixfmt;
                    deadnix.enable = true;
                    statix.enable = true;
                    shellcheck.enable = true;
                  };
                };
                formatter = config.treefmt.build.wrapper;
              };
          };
    in
    flakeOutputs
    // {
      nixosModules = flakeOutputs.nixosModules // {
        rigs = {
          base = import ./modules/base;
          rig = import ./modules/rig;
        };
      };
    };
}
