#!/usr/bin/env python3
"""Differential parity runner for unish.

Every case is a shell script that must behave identically under unish
and an oracle shell (GNU bash + coreutils): same stdout, same exit code
and same files left behind in a fresh working directory. Stderr is
compared loosely (presence only) because shell diagnostics carry
per-shell prefixes.

Cases may be (script, xfail_reason) pairs: those record known cosmetic
deltas that are reported but don't fail the run.

Typical uses:

    # in CI (Linux runner ships GNU bash + coreutils):
    python3 parity/runner.py --unish ./unish --oracle /bin/bash

    # on Windows against Git Bash:
    python parity/runner.py --unish .\\unish.exe ^
        --oracle "C:\\Program Files\\Git\\bin\\bash.exe@C:\\Program Files\\Git\\usr\\bin"
"""

import argparse
import os
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from cases import misc_cases, printf_cases, sed_cases, shell_cases, sort_cases

CASE_GROUPS = [
    ("sort", sort_cases.CASES),
    ("sed", sed_cases.CASES),
    ("printf", printf_cases.CASES),
    ("tools", misc_cases.CASES),
    ("shell", shell_cases.CASES),
]


def norm(b: bytes) -> bytes:
    return b.replace(b"\r\n", b"\n")


def listing(root: str):
    out = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames.sort()
        for f in sorted(filenames):
            rel = os.path.relpath(os.path.join(dirpath, f), root)
            out.append(rel.replace(os.sep, "/"))
    return out


def run_one(argv, script, cwd, timeout):
    try:
        p = subprocess.run(argv + ["-c", script], cwd=cwd,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                           timeout=timeout)
        return norm(p.stdout), p.returncode, bool(p.stderr)
    except subprocess.TimeoutExpired:
        return b"<TIMEOUT>", 124, True


def parse_shell(spec):
    """name=CMD[+ARGS][@PATHPREFIX] like the bench harness."""
    if "@" in spec:
        cmd, path_prefix = spec.rsplit("@", 1)
    else:
        cmd, path_prefix = spec, ""
    env = None
    if path_prefix:
        env = dict(os.environ)
        env["PATH"] = path_prefix + os.pathsep + env.get("PATH", "")
    return cmd.split("+"), env


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--unish", default="./unish", help="path to the unish binary")
    ap.add_argument("--oracle", default="bash", help="oracle shell: CMD[+ARGS][@PATHPREFIX]")
    ap.add_argument("--filter", default="", help="only run cases whose script contains this")
    ap.add_argument("--timeout", type=int, default=60)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    oracle, env = parse_shell(args.oracle)
    unish = os.path.abspath(args.unish)
    base_env = dict(os.environ)
    base_env["LC_ALL"] = "C.UTF-8"
    base_env["LANG"] = "C.UTF-8"
    if env:
        env = dict(env)
        env["LC_ALL"] = "C.UTF-8"
        env["LANG"] = "C.UTF-8"

    total = failed = xfailed = xpassed = 0
    for group, cases in CASE_GROUPS:
        for entry in cases:
            script, xfail = entry if isinstance(entry, tuple) else (entry, "")
            if args.filter and args.filter not in script:
                continue
            total += 1
            dirs = [tempfile.mkdtemp(prefix="parity_u_"), tempfile.mkdtemp(prefix="parity_o_")]
            try:
                uo, uc, ue = run_one([unish], script, dirs[0], args.timeout)
                oo, oc, oe = run_one(oracle, script, dirs[1], args.timeout)
                ul, ol = listing(dirs[0]), listing(dirs[1])
                diffs = []
                if uo != oo:
                    diffs.append("stdout")
                if uc != oc:
                    diffs.append("exit(%d vs %d)" % (uc, oc))
                if ul != ol:
                    diffs.append("files(%s vs %s)" % (ul, ol))
                if ue != oe and not diffs:
                    diffs.append("stderr-presence")  # informational only

                if diffs and xfail:
                    xfailed += 1
                    if args.verbose:
                        print("XFAIL [%s] %s: %s" % (group, ",".join(diffs), xfail))
                elif diffs:
                    failed += 1
                    print("FAIL  [%s] %s" % (group, ",".join(diffs)))
                    print("      script: %r" % script)
                    if uo != oo:
                        print("      unish: %r" % uo[:300])
                        print("      oracle: %r" % oo[:300])
                    if ul != ol:
                        print("      files unish: %s oracle: %s" % (ul, ol))
                elif xfail:
                    xpassed += 1
                    print("XPASS [%s] %s (fixed? remove the xfail)" % (group, xfail))
                elif args.verbose:
                    print("ok    [%s] %r" % (group, script[:60]))
            finally:
                shutil.rmtree(dirs[0], ignore_errors=True)
                shutil.rmtree(dirs[1], ignore_errors=True)

    print("=== %d cases: %d failures, %d known deltas (xfail), %d xpass" %
          (total, failed, xfailed, xpassed))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
