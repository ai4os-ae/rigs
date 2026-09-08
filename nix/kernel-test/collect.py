#!/usr/bin/env python3
"""The host half of a rig test run.

The guest writes framed, base64'd output to ttyS1 (see ./guest-init.sh); QEMU
writes that to a file; this turns it back into artifacts a caller can fetch,
and into one verdict.

Two subcommands, because they answer different questions and must not be the
same call:

    (default)   parse the raw channel into $out and write result.json
    --verdict   read result.json and exit non-zero if the test failed

They are separate so that extraction happens even when the run went badly. A
kernel that panicked halfway through a selftest has produced the most useful
output of the whole job, and losing it because the same command also decides
pass or fail would be exactly the wrong trade. The derivation therefore runs
extraction unconditionally and only then records the verdict.
"""

import argparse
import base64
import binascii
import json
import pathlib
import re
import sys

BEGIN = re.compile(r"^--RIG-BEGIN (\S+) (-?\d+)$")
END = re.compile(r"^--RIG-END (\S+)$")
DONE = re.compile(r"^--RIG-DONE (-?\d+)$")

# TAP 13, as kselftest emits it. The trailing directive is what says a test was
# skipped rather than passed, and skipped is not a failure: a selftest that
# needs a configuration this kernel was not built with is telling us about the
# configuration, not about the patch.
TAP = re.compile(r"^(not ok|ok)\s+(\d+)\s*(?:-\s*)?(.*)$")
SKIP = re.compile(r"#\s*(SKIP|skip)")

# What in a kernel log means the kernel itself went wrong, as opposed to a test
# deciding it failed. Deliberately conservative: every one of these is a splat
# nobody would defend, and none of them fires on an ordinary boot. WARNING is
# not here, because a clean kernel warns about something on QEMU's hardware
# often enough that failing on it would train people to ignore the result.
FATAL_PATTERNS = [
    "Kernel panic",
    "kernel BUG at",
    "BUG: kernel",
    "Oops:",
    "general protection fault",
    "unable to handle kernel",
    "Unable to handle kernel",
    "KASAN:",
    "UBSAN:",
]

WARNING_PATTERNS = [
    "WARNING: CPU",
    "------------[ cut here ]------------",
]

# Named so a caller reading an artifact listing can tell what each one is
# without consulting the docs. Anything the guest sends under a name not in
# here still lands on disk, as <name>.log — a new phase should show up rather
# than vanish.
ARTIFACT_NAMES = {
    "uname": "uname.log",
    "modules": "modules.log",
    "kselftest": "kselftest.log",
    "script": "script.log",
    "lsmod": "lsmod.log",
    "dmesg": "dmesg.log",
    "tainted": "tainted.log",
}


def parse_channel(raw):
    """Turn the ttyS1 capture into {name: (rc, bytes)} plus the final status.

    Tolerant on purpose. The channel is a serial port attached to a machine
    that may have died mid-sentence, so a frame that never ended, or whose
    payload does not decode, is recorded as such and does not stop the rest
    from being read.
    """
    frames = {}
    done = None
    truncated = []

    name = None
    rc = 0
    payload = []

    # latin-1 never raises: the channel is base64 between markers, but a guest
    # that crashed can leave arbitrary bytes in it and those must not become an
    # exception here.
    for line in raw.decode("latin-1").splitlines():
        if name is None:
            begun = BEGIN.match(line)
            if begun:
                name, rc = begun.group(1), int(begun.group(2))
                payload = []
                continue
            finished = DONE.match(line)
            if finished:
                done = int(finished.group(1))
            continue

        ended = END.match(line)
        if ended:
            joined = "".join(payload)
            try:
                data = base64.b64decode(joined, validate=True)
            except (binascii.Error, ValueError):
                data = b"[the payload for this phase did not decode]\n"
                truncated.append(name)
            frames[name] = (rc, data)
            name = None
            continue

        payload.append(line.strip())

    if name is not None:
        # A frame that began and never ended: the guest stopped talking. Keep
        # whatever decodes, since a partial dmesg is often the whole answer.
        truncated.append(name)
        joined = "".join(payload)
        usable = joined[: len(joined) // 4 * 4]
        try:
            data = base64.b64decode(usable, validate=True)
        except (binascii.Error, ValueError):
            data = b""
        frames[name] = (rc, data + b"\n[truncated: the guest stopped here]\n")

    return frames, done, truncated


def parse_tap(text):
    """Every TAP result line, as {name, status}."""
    results = []
    for line in text.splitlines():
        matched = TAP.match(line.strip())
        if not matched:
            continue
        status = "passed" if matched.group(1) == "ok" else "failed"
        description = matched.group(3).strip()
        if SKIP.search(description):
            status = "skipped"
        results.append(
            {
                "name": re.sub(r"\s*#.*$", "", description).strip() or matched.group(2),
                "status": status,
            }
        )
    return results


def scan_log(text, fail_on_warning):
    """Kernel splats in a log, as {pattern, line}.

    Both dmesg and the console are scanned, because they do not always both
    exist. `oops=panic` on the guest command line means an oops takes the
    machine down before the guest can send dmesg, and in that case the console
    is the only place the splat was ever written.
    """
    patterns = list(FATAL_PATTERNS)
    if fail_on_warning:
        patterns += WARNING_PATTERNS

    found = []
    for line in text.splitlines():
        for pattern in patterns:
            if pattern in line:
                found.append({"pattern": pattern, "line": line.strip()[:400]})
                break
    return found


def collect(args):
    out = pathlib.Path(args.out)
    raw_path = pathlib.Path(args.raw)
    raw = raw_path.read_bytes() if raw_path.exists() else b""

    frames, done, truncated = parse_channel(raw)

    phases = []
    for name, (rc, data) in sorted(frames.items()):
        filename = ARTIFACT_NAMES.get(name, f"{name}.log")
        (out / filename).write_bytes(data)
        phases.append({"name": name, "returncode": rc, "log": filename})

    # Which targets were asked for and which the build managed to produce. A
    # selftest target that does not compile on this kernel is not fatal — the
    # other five are still worth running — but it must not be silent either,
    # or a caller reads "all tests passed" for a target that was never there.
    requested = sorted((args.kselftest_requested or "").split())
    built = []
    if args.kselftest_built and pathlib.Path(args.kselftest_built).exists():
        built = sorted(pathlib.Path(args.kselftest_built).read_text().split())
    missing = [t for t in requested if t not in built]

    kselftest = None
    if "kselftest" in frames:
        text = frames["kselftest"][1].decode("utf-8", "replace")
        results = parse_tap(text)
        kselftest = {
            "returncode": frames["kselftest"][0],
            "targets_requested": requested,
            "targets_built": built,
            "targets_missing": missing,
            "total": len(results),
            "passed": sum(1 for r in results if r["status"] == "passed"),
            "failed": sum(1 for r in results if r["status"] == "failed"),
            "skipped": sum(1 for r in results if r["status"] == "skipped"),
            "results": results,
        }

    tainted = None
    if "tainted" in frames:
        try:
            tainted = int(frames["tainted"][1].decode("ascii", "ignore").strip())
        except ValueError:
            tainted = None

    console = pathlib.Path(args.console)
    log_text = console.read_text(encoding="utf-8", errors="replace") if console.exists() else ""
    if "dmesg" in frames:
        log_text += "\n" + frames["dmesg"][1].decode("utf-8", "replace")
    splats = scan_log(log_text, args.fail_on_warning)

    # `booted` is the question underneath every other one. The guest sends its
    # first frame only after it has mounted /proc and opened the channel, so a
    # run with no frames at all never reached userspace — which is a finding
    # about the kernel and not a broken harness.
    booted = bool(frames)

    reasons = []
    if not booted:
        reasons.append("the kernel never reached userspace")
    if done is None:
        reasons.append("the guest stopped before finishing the run")
    elif done != 0:
        # Named, not merely counted. "a test phase failed" sends the reader to
        # the logs to find out which one, and the whole point of a verdict is
        # to say what happened without that.
        failed = [f"{p['name']} (exit {p['returncode']})" for p in phases if p["returncode"] != 0]
        reasons.append("failed: " + ", ".join(failed) if failed else "the guest reported a failure")
    if splats:
        reasons.append(f"the kernel log contains {len(splats)} splat(s)")
    if truncated:
        reasons.append("truncated output for: " + ", ".join(sorted(set(truncated))))
    if missing:
        reasons.append("selftest targets that did not build: " + ", ".join(missing))

    # The TAP decides, not the exit status.
    #
    # run_kselftest.sh exits 0 whether every test passed or every test failed —
    # it is a harness reporting results, and it reported them successfully. A
    # verdict taken from its exit status is therefore always "passed", which is
    # exactly the failure mode that makes a test suite worse than none: it goes
    # green and nobody looks again.
    if kselftest:
        if kselftest["failed"]:
            reasons.append(
                f"{kselftest['failed']} of {kselftest['total']} selftests failed"
            )
        elif kselftest["total"] == 0:
            reasons.append("the selftest run reported no results at all")

    result = {
        "schema": "rig_kernel_test_result_v1",
        "passed": not reasons,
        "booted": booted,
        "guest_status": done,
        "reasons": reasons,
        "phases": phases,
        "kselftest": kselftest,
        "tainted": tainted,
        "kernel_splats": splats,
    }

    (out / "result.json").write_text(
        json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    print(f"test passed: {result['passed']}")
    for reason in reasons:
        print(f"  - {reason}")
    return 0


def verdict(args):
    """Exit non-zero if the test failed, so the caller can record it.

    Nothing is computed here: the judgement was made and written down by
    `collect`, and this only reads it back. Two commands so that a run whose
    extraction crashed is still reported as a failure rather than silently
    passing on a missing file.
    """
    path = pathlib.Path(args.verdict)
    if not path.exists():
        print(f"{path} was never written; the run produced no result", file=sys.stderr)
        return 1

    result = json.loads(path.read_text(encoding="utf-8"))
    if result.get("passed"):
        print("test passed")
        return 0

    print("test failed:", "; ".join(result.get("reasons", ["no reason recorded"])), file=sys.stderr)
    return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--verdict", help="read this result.json and exit on its verdict")
    parser.add_argument("--raw", help="the ttyS1 capture QEMU wrote")
    parser.add_argument("--console", help="the ttyS0 capture, for splats dmesg never reached")
    parser.add_argument("--out", help="directory to write artifacts and result.json into")
    parser.add_argument(
        "--kselftest-requested", default="", help="space-separated targets the job asked for"
    )
    parser.add_argument(
        "--kselftest-built", help="the selftest build's `targets` file, listing what compiled"
    )
    parser.add_argument(
        "--fail-on-warning",
        action="store_true",
        help="treat a WARNING splat as a failure, not only an oops or a panic",
    )

    args = parser.parse_args()
    if args.verdict:
        return verdict(args)
    if not (args.raw and args.console and args.out):
        parser.error("--raw, --console and --out are required unless --verdict is given")
    return collect(args)


if __name__ == "__main__":
    sys.exit(main())
