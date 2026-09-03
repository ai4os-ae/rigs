# Agent Guidance

This repository holds the NixOS configurations for the AI4OS rigs — unattended
remote machines that run research jobs.

## Contributing

- Nix flakes only. Never `nix-channel`, never `nix-env`.
- All options this repository defines live under the `rigs.*` namespace.
- Format every change with `nix fmt` before committing.
- `nix flake check` evaluates every rig and is slow; it is a human pre-push
  step. Agents should build only the hosts they touched:
  `nix build .#nixosConfigurations.<host>.config.system.build.toplevel`.
- Keep commit messages concise and descriptive.

## Things worth knowing

- Rigs have no console anybody will walk up to. A change that can lock out SSH
  or leave the machine waiting at a prompt is a change that needs a site visit.
  `modules/base/users.nix` asserts on accounts with no keys for this reason.
- The roster in `modules/base/people.nix` applies to every rig. uids are pinned
  and never reused.
- A rig's sops identity is derived from its SSH host key, so it does not exist
  until after the first install. `rigs.bootstrap` covers that gap.
- No home-manager, no lanzaboote, and no per-user development tooling here —
  those belong in the personal dotfiles repo, not on shared research machines.
