#!/usr/bin/env python3
"""Bookkeeping for the kernel build derivations.

The build itself is a shell script, but everything it has to *report* is JSON:
command records with their arguments and exit status, per-symbol configuration
verification, and a hash for every artifact. Assembling that in bash means
hand-rolling JSON escaping around argv that contains spaces, quotes and
backslashes, so it lives here instead.

Three subcommands, called in order by the derivation:

    add-command   append one command record as it finishes
    check-config  verify the resulting .config against what was asked for
    finalise      walk $out, hash everything, write report.json

`finalise` is deliberately the only thing that writes report.json, so a report
either describes a complete build or does not exist.
"""

import argparse
import hashlib
import json
import os
import pathlib
import sys

# Read in chunks rather than whole: vmlinux with debug info is comfortably
# large enough that slurping every artifact would matter on a 32GB rig.
_CHUNK = 1 << 20


def sha256_file(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            block = handle.read(_CHUNK)
            if not block:
                break
            digest.update(block)
    return digest.hexdigest()


def add_command(args):
    """Append one command record to the JSONL records file.

    JSONL rather than a single array so that an interrupted build still leaves
    a readable prefix, and so appending needs no read-modify-write.
    """
    record = {
        "name": args.name,
        "argv": args.argv,
        "returncode": args.returncode,
        "passed": args.returncode == 0,
        # Relative so the report stays valid wherever $out is mounted or
        # copied to; the daemon serves these paths directly.
        "log": args.log,
    }
    with open(args.records, "a", encoding="utf-8") as handle:
        handle.write(json.dumps(record) + "\n")


def _parse_config(path):
    """Read a .config into {SYMBOL: "y"|"m"|"n"}, without the CONFIG_ prefix.

    Kconfig writes disabled symbols as a comment rather than as `=n`, and omits
    symbols whose dependencies are unmet entirely. Both mean "not enabled", so
    both are reported as "n" and the caller cannot tell them apart — which is
    correct, because for our purposes they fail identically.
    """
    values = {}
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if line.startswith("CONFIG_") and "=" in line:
                name, _, value = line.partition("=")
                values[name[len("CONFIG_") :]] = value
            elif line.startswith("# CONFIG_") and line.endswith(" is not set"):
                name = line[len("# CONFIG_") : -len(" is not set")]
                values[name] = "n"
    return values


def check_config(args):
    """Verify every requested symbol survived olddefconfig.

    This is the check that matters most in practice. A symbol whose
    dependencies are not met is silently dropped by olddefconfig, and the build
    then succeeds and produces a kernel that quietly lacks the driver under
    test — which is far worse than failing, because the result looks valid.
    """
    requested = json.loads(pathlib.Path(args.requested).read_text(encoding="utf-8"))
    actual = _parse_config(args.config)

    results = {}
    matched = True
    for symbol, want in sorted(requested.items()):
        got = actual.get(symbol, "n")
        ok = got == want
        matched = matched and ok
        results[symbol] = {"requested": want, "actual": got, "matched": ok}

    pathlib.Path(args.out).write_text(
        json.dumps({"matched": matched, "symbols": results}, indent=2) + "\n",
        encoding="utf-8",
    )

    for symbol, result in results.items():
        if not result["matched"]:
            print(
                f"required config mismatch: CONFIG_{symbol} "
                f"requested {result['requested']}, got {result['actual']}",
                file=sys.stderr,
            )

    return 0 if matched else 1


def _walk_artifacts(root, skip):
    """Every regular file under $out, hashed, as {relative path: {...}}.

    Symlinks are recorded but not followed or hashed: `modules_install` leaves
    `build` and `source` links in lib/modules/<release>/ that point at the
    build tree, which no longer exists once the derivation finishes.
    """
    artifacts = {}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if os.path.join(dirpath, d) not in skip]
        for filename in filenames:
            full = pathlib.Path(dirpath) / filename
            rel = str(full.relative_to(root))
            if full.is_symlink():
                artifacts[rel] = {"symlink": os.readlink(full)}
                continue
            artifacts[rel] = {
                "size": full.stat().st_size,
                "sha256": sha256_file(full),
            }
    return artifacts


def finalise(args):
    out = pathlib.Path(args.out)
    meta = json.loads(pathlib.Path(args.meta).read_text(encoding="utf-8"))

    commands = []
    records = pathlib.Path(args.records)
    if records.exists():
        for line in records.read_text(encoding="utf-8").splitlines():
            if line.strip():
                commands.append(json.loads(line))

    config_check = None
    if args.config_check and pathlib.Path(args.config_check).exists():
        config_check = json.loads(
            pathlib.Path(args.config_check).read_text(encoding="utf-8")
        )

    # A build "passed" only if every command it ran passed *and* the resulting
    # configuration is the one that was asked for. Note this is a property of
    # the kernel build, not of the derivation: the derivation exits 0 either
    # way, so that a failed build caches with its logs instead of being retried
    # from scratch every time somebody asks for it again.
    passed = all(command["passed"] for command in commands)
    if config_check is not None:
        passed = passed and config_check["matched"]

    kernel_release = None
    release_file = out / "kernel-release"
    if release_file.exists():
        kernel_release = release_file.read_text(encoding="utf-8").strip()

    report = {
        "schema": "rig_kernel_build_report_v1",
        "passed": passed,
        "kernel_release": kernel_release,
        **meta,
        "config": config_check,
        "commands": commands,
        # Opt-in: the base stage's $out is ~18GB of object files that nothing
        # downstream reads individually, and hashing it would cost minutes for
        # a manifest no caller wants. Only the patched stage, whose output is
        # the thing actually handed back, pays for one.
        "artifacts": (
            _walk_artifacts(out, skip={str(out / "logs")}) if args.artifacts else None
        ),
    }

    (out / "report.json").write_text(
        json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(f"build passed: {passed}")
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    add = sub.add_parser("add-command")
    add.add_argument("records")
    add.add_argument("name")
    add.add_argument("returncode", type=int)
    add.add_argument("log")
    add.add_argument("argv", nargs=argparse.REMAINDER)
    add.set_defaults(func=add_command)

    check = sub.add_parser("check-config")
    check.add_argument("config")
    check.add_argument("requested")
    check.add_argument("out")
    check.set_defaults(func=check_config)

    fin = sub.add_parser("finalise")
    fin.add_argument("out")
    fin.add_argument("records")
    fin.add_argument("meta")
    fin.add_argument("--config-check", default=None)
    fin.add_argument(
        "--artifacts",
        action="store_true",
        help="hash every file in $out into the report (see finalise)",
    )
    fin.set_defaults(func=finalise)

    args = parser.parse_args()
    return args.func(args) or 0


if __name__ == "__main__":
    sys.exit(main())
