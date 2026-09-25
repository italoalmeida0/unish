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
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from cases import misc_cases, printf_cases, sed_cases, shell_cases, sort_cases
from functional import CASES as FUNCTIONAL_CASES
from cases.flag_cases import FLAG_CASES
from cases.gap_cases import ORACLE_CASES, FIXED_CASES
from cases.cmd_cases import CASES as CMD_CASES
from e2e import CASES as E2E_CASES
from edge_cases import CASES as EDGE_CASES
from shell_builtins import CASES as BUILTIN_CASES
from flag_audit import oracle_path

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


def is_old_oracle(oracle_spec):
    """Git Bash ships coreutils 8.32, which words a few diagnostics
    differently from 9.4. Those deltas are the oracle's, not unish's."""
    low = oracle_spec.lower()
    return "git" in low or "msys" in low


def findutils_version():
    """(major, minor) of the findutils the oracle would reach for, or None.

    Diagnostic wording moved again in findutils 4.10 ("invalid file mode",
    "failed to run command ..."), just as coreutils 8.32 worded things the
    old way. unish pins one GNU wording -- the one on Ubuntu LTS and Git
    Bash -- so whichever end drifts away is the oracle's delta, not a
    behavioural gap. See the "oracle-wording" case tag."""
    path = oracle_path("find")
    if not path:
        return None
    try:
        out = subprocess.run([path, "--version"], stdout=subprocess.PIPE,
                             stderr=subprocess.DEVNULL, timeout=10).stdout
    except (OSError, subprocess.SubprocessError):
        return None
    m = re.search(rb"(\d+)\.(\d+)", out)
    return (int(m.group(1)), int(m.group(2))) if m else None


def platform_key():
    """Coarse platform tag for case filtering: linux/windows/darwin."""
    return platform.system().lower()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--unish", default="./unish", help="path to the unish binary")
    ap.add_argument("--oracle", default="bash", help="oracle shell: CMD[+ARGS][@PATHPREFIX]")
    ap.add_argument("--filter", default="", help="only run cases whose script contains this")
    ap.add_argument("--timeout", type=int, default=60)
    ap.add_argument("--verbose", action="store_true")
    ap.add_argument("--only-builtins", action="store_true",
                    help="run only the shell builtin cases")
    ap.add_argument("--only-cmd", action="store_true",
                    help="run only the per-command fixture cases")
    ap.add_argument("--only-edge", action="store_true",
                    help="run only the boundary/robustness cases")
    ap.add_argument("--only-e2e", action="store_true",
                    help="run only the end-to-end process cases")
    ap.add_argument("--only-flags", action="store_true",
                    help="run only the generated flag cases")
    ap.add_argument("--go-tests", action="store_true",
                    help="also run go test -json and report platform skips")
    args = ap.parse_args()

    # The oracle spec's @PREFIX is also where its GNU tools live; set
    # ORACLE_PATH from it so the flag probe finds them without a separate
    # environment variable (easy to forget locally).
    if "@" in args.oracle and not os.environ.get("ORACLE_PATH"):
        os.environ["ORACLE_PATH"] = args.oracle.rsplit("@", 1)[1]

    if args.go_tests:
        repo = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        return run_go_tests(repo, args.verbose)

    if args.only_builtins:
        unish_abs = os.path.abspath(args.unish)
        return 1 if run_builtins(unish_abs, args.timeout, args.verbose) else 0
    if args.only_cmd:
        unish_abs = os.path.abspath(args.unish)
        return 1 if run_cmd_cases(unish_abs, args.timeout, args.verbose) else 0
    if args.only_edge:
        unish_abs = os.path.abspath(args.unish)
        return 1 if run_edge(unish_abs, args.timeout, args.verbose) else 0
    if args.only_e2e:
        unish_abs = os.path.abspath(args.unish)
        return 1 if run_e2e(unish_abs, args.timeout, args.verbose) else 0
    if args.only_flags:
        unish_abs = os.path.abspath(args.unish)
        return 1 if run_flag_cases(unish_abs, args.oracle, args.timeout, args.verbose) else 0
    # Both shells are spawned with the same inherited environment (run_one
    # takes none), so the oracle spec's @PATHPREFIX is honoured exactly
    # where it matters: ORACLE_PATH for the flag probe, set above.
    #
    # Pin the locale too: collation, numeric parsing and diagnostic
    # quoting all follow it (GNU sort alone answers `sort -u` and
    # `sort -n` differently in C and in en_US.UTF-8), and a differential
    # is only meaningful when both sides see the same one. C.UTF-8 keeps
    # GNU's UTF-8 quoting style while staying byte-collated.
    os.environ["LC_ALL"] = "C.UTF-8"
    os.environ["LANG"] = "C.UTF-8"
    new_oracle = (findutils_version() or (0, 0)) >= (4, 10)
    oracle, _ = parse_shell(args.oracle)
    unish = os.path.abspath(args.unish)

    total = failed = xfailed = xpassed = 0
    for group, cases in CASE_GROUPS:
        for entry in cases:
            script, xfail = entry if isinstance(entry, tuple) else (entry, "")
            if args.filter and args.filter not in script:
                continue
            total += 1
            if xfail == "old-oracle":
                # Applied only when the oracle is an older toolkit.
                xfail = "coreutils 8.32 (Git Bash) wording" if is_old_oracle(args.oracle) else ""
            elif xfail == "oracle-wording":
                # Wording drifted in newer GNU too (findutils 4.10+).
                xfail = ("the oracle's GNU generation words this differently"
                         if (is_old_oracle(args.oracle) or new_oracle) else "")
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
                    # An xfail that now passes is good news, not a failure:
                    # the delta was cosmetic/build-specific and vanished.
                    xpassed += 1
                    if args.verbose:
                        print("XPASS [%s] %s" % (group, xfail))
                elif args.verbose:
                    print("ok    [%s] %r" % (group, script[:60]))
            finally:
                shutil.rmtree(dirs[0], ignore_errors=True)
                shutil.rmtree(dirs[1], ignore_errors=True)

    print("=== %d cases: %d failures, %d known deltas (xfail), %d xpass" %
          (total, failed, xfailed, xpassed))
    fn_failed = run_functional(unish, args.timeout, args.verbose)
    fl_failed = run_flag_cases(unish, args.oracle, args.timeout, args.verbose)
    gp_failed = run_gap_cases(unish, args.oracle, args.timeout, args.verbose)
    e2_failed = run_e2e(unish, args.timeout, args.verbose)
    ed_failed = run_edge(unish, args.timeout, args.verbose)
    cm_failed = run_cmd_cases(unish, args.timeout, args.verbose)
    bi_failed = run_builtins(unish, args.timeout, args.verbose)
    return 1 if (failed or fn_failed or fl_failed or gp_failed or e2_failed or ed_failed or cm_failed or bi_failed) else 0


def run_go_tests(unish_dir, verbose):
    """Run the Go unit suite as JSON and report skip counts per test.

    Platform skips are legitimate (no /proc on macOS, no symlink privs on
    Windows), but they must be *visible*: a regression that turns into a
    skip would otherwise pass unnoticed.
    """
    p = subprocess.run(["go", "test", "-count=1", "-json", "./..."],
                       cwd=unish_dir, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    passed = skipped = failed = 0
    skipped_tests = []
    for line in p.stdout.decode("utf-8", "replace").splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            ev = json.loads(line)
        except ValueError:
            continue
        action = ev.get("Action")
        test = ev.get("Test", "")
        if not test:
            continue
        if action == "pass":
            passed += 1
        elif action == "skip":
            skipped += 1
            skipped_tests.append(test)
        elif action == "fail":
            failed += 1
    print("=== go tests: %d passed, %d failed, %d SKIPPED on %s" %
          (passed, failed, skipped, platform_key()))
    if skipped_tests and verbose:
        for t in skipped_tests:
            print("    skip: %s" % t)
    return failed


def run_flag_cases(unish, oracle_spec, timeout, verbose):
    """Fase 2: flag cases, diffed against the oracle AT RUN TIME.

    The expectation is not a frozen string: it is whatever the platform's
    GNU tool prints for the same argv right now. A captured string would
    carry the capture machine's line endings and fail everywhere else —
    exactly what the first cross-platform CI run exposed.
    """
    oracle, _ = parse_shell(oracle_spec)
    # Is the oracle actually GNU? `cat --number` exists only in GNU; BSD
    # (macOS) rejects it. If not GNU, GNU-flag parity is not meaningful
    # there and the whole group is reported as skipped rather than failed.
    # Probe the oracle's CAT (not the shell): `cat --number` is a GNU-only
    # long option, so a successful run means GNU tools are present. Running
    # the shell with `--number` was wrong — that is cat's flag, so the probe
    # failed everywhere and skipped the whole group on every platform.
    probe = tempfile.mkdtemp(prefix="gnuprobe_")
    with open(os.path.join(probe, "p.txt"), "w") as fh:
        fh.write("x\n")
    is_gnu = False
    ocat = oracle_path("cat")
    if ocat:
        try:
            pp = subprocess.run([ocat, "--number", "p.txt"], cwd=probe,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
            is_gnu = pp.returncode == 0
        except Exception:  # noqa: BLE001
            is_gnu = False
    shutil.rmtree(probe, ignore_errors=True)
    if not is_gnu:
        print("=== flags: skipped (oracle is BSD, not GNU: GNU flag "
              "formatting differs by design)")
        return 0
    total = failed = xfail = gaps = 0
    work = tempfile.mkdtemp(prefix="flags_")
    try:
        for argv, xf, files, want_rc in FLAG_CASES:
            total += 1
            for name in os.listdir(work):
                p_ = os.path.join(work, name)
                if os.path.isdir(p_):
                    shutil.rmtree(p_, ignore_errors=True)
                else:
                    os.remove(p_)
            for name, content in files.items():
                with open(os.path.join(work, name), "w") as fh:
                    fh.write(content)
            cmdline = " ".join(argv)
            try:
                p = subprocess.run([unish, "-c", cmdline], cwd=work, input=b"",
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
            except subprocess.TimeoutExpired:
                failed += 1
                print("FAIL  [flag] %s (timeout)" % cmdline)
                continue
            got = norm(p.stdout)
            # the oracle for this command, run with the same argv
            o = oracle_path(argv[0])
            if not o:
                if verbose:
                    print("skip  [flag] %s (no oracle)" % cmdline)
                continue
            try:
                op = subprocess.run([o] + argv[1:], cwd=work, input=b"",
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
            except subprocess.TimeoutExpired:
                continue
            except OSError:
                # The oracle is a shell script (Git Bash gunzip) and cannot
                # be exec'd directly; not a unish problem.
                if verbose:
                    print("skip  [flag] %s (oracle not executable)" % cmdline)
                continue
            want = norm(op.stdout)
            # BSD tools (macOS) reject GNU long options and print nothing:
            # an empty oracle result means "this flag is not a GNU flag",
            # not "unish is wrong". Skip rather than report a false failure.
            if op.returncode != 0:
                if verbose:
                    print("skip  [flag] %s (oracle rejects the flag here)" % cmdline)
                continue
            # uname -p/-i: the Git Bash oracle reports its MSYS label
            # ("unknown"), while unish reports the real machine. The oracle
            # is the one that cannot answer here, so this is a known
            # difference, not a unish failure.
            if argv[0] == "uname" and len(argv) > 1 and argv[1] in (
                    "-p", "-i", "--processor", "--hardware-platform"):
                if verbose:
                    print("ok    [flag] %s (real machine vs MSYS label)" % cmdline)
                continue
            if got == want and p.returncode == want_rc:
                if verbose:
                    print("ok    [flag] %s" % cmdline)
            elif xf:
                xfail += 1
                if verbose:
                    print("KNOWN [flag] %s" % cmdline)
            else:
                failed += 1
                print("FAIL  [flag] %s" % cmdline)
                print("      want %r" % want[:120])
                print("      got  %r" % got[:120])
    finally:
        shutil.rmtree(work, ignore_errors=True)
    print("=== flags: %d cases, %d failures, %d known differences, %d known gaps"
          % (total, failed, xfail, gaps))
    return failed


def run_gap_cases(unish, oracle_spec, timeout, verbose):
    """Fase 3: the 15 commands that had no coverage.

    FIXED_CASES carry a frozen expected value (hash of a known input,
    literal output). ORACLE_CASES are machine-dependent, so they are
    diffed against GNU with the same script — the only honest expectation
    for `df` sizes, `uptime` minutes or `hostname`.
    """
    total = failed = 0
    oracle, _ = parse_shell(oracle_spec)
    for name, script, expect in FIXED_CASES:
        total += 1
        d = tempfile.mkdtemp(prefix="gap_")
        try:
            got, rc, _ = run_one([unish], script, d, timeout)
            got_s = norm(got).decode("utf-8", "replace")
            if got_s == expect:
                if verbose:
                    print("ok    [gap] %s" % name)
            elif name.startswith("FINDING"):
                # A documented gap: GNU does X, unish does not yet. Kept
                # visible (not skipped) so it shows up in every run.
                print("KNOWN GAP [gap] %s" % name)
                if verbose:
                    print("      want %r  got %r" % (expect, got_s))
            else:
                failed += 1
                print("FAIL  [gap] %s" % name)
                print("      script: %r" % script)
                print("      want %r" % expect)
                print("      got  %r" % got_s)
        finally:
            shutil.rmtree(d, ignore_errors=True)
    for name, script in ORACLE_CASES:
        total += 1
        du, do = tempfile.mkdtemp(prefix="gapu_"), tempfile.mkdtemp(prefix="gapo_")
        try:
            uo, urc, _ = run_one([unish], script, du, timeout)
            oo, orc, _ = run_one(oracle, script, do, timeout)
            if norm(uo) == norm(oo) and urc == orc:
                if verbose:
                    print("ok    [gap] %s" % name)
            else:
                failed += 1
                print("FAIL  [gap] %s" % name)
                print("      script: %r" % script)
                print("      unish %r/%d  oracle %r/%d" % (norm(uo)[:80], urc, norm(oo)[:80], orc))
        finally:
            shutil.rmtree(du, ignore_errors=True)
            shutil.rmtree(do, ignore_errors=True)
    print("=== gaps: %d cases, %d failures" % (total, failed))
    return failed


def run_e2e(unish, timeout, verbose):
    """Fase 4: the binary as a real process — scripts, pipes, exit codes.

    Each case runs `unish` as a child process (never in-process), so it
    exercises the parts a user actually touches: argv handling, stdin,
    real pipes between two unish processes, redirections and exit codes.
    """
    total = failed = 0
    for name, kind, payload, (want_out, want_rc) in E2E_CASES:
        total += 1
        d = tempfile.mkdtemp(prefix="e2e_")
        try:
            if kind == "script":
                sc = os.path.join(d, "s.sh")
                with open(sc, "w") as fh:
                    fh.write(payload)
                argv = [unish, sc] + (["x", "y"] if "$1" in payload else [])
                p = subprocess.run(argv, cwd=d, input=b"line\n",
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
                got, rc = norm(p.stdout).decode("utf-8", "replace"), p.returncode
            elif kind == "stdin":
                p = subprocess.run([unish], cwd=d, input=payload.encode(),
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
                got, rc = norm(p.stdout).decode("utf-8", "replace"), p.returncode
            elif kind == "pipeline":
                p = subprocess.run([unish, "-c", payload], cwd=d, input=b"",
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
                got, rc = norm(p.stdout).decode("utf-8", "replace"), p.returncode
            elif kind == "process":
                # Real OS pipe chain: N unish processes, each stdout into
                # the next stdin. This is what a Makefile or another tool
                # sees, and it is where stdin/buffering bugs surface.
                procs = []
                prev_out = None
                for i, stage in enumerate(payload):
                    last = i == len(payload) - 1
                    pr = subprocess.Popen(
                        [unish, "-c", " ".join(stage)], cwd=d,
                        stdin=prev_out,
                        stdout=subprocess.PIPE if not last else subprocess.PIPE,
                        stderr=subprocess.PIPE)
                    if prev_out is not None:
                        prev_out.close()
                    prev_out = pr.stdout
                    procs.append(pr)
                out, _ = procs[-1].communicate(timeout=timeout)
                for pr in procs[:-1]:
                    try:
                        pr.wait(timeout=timeout)
                    except subprocess.TimeoutExpired:
                        pr.kill()
                got, rc = norm(out).decode("utf-8", "replace"), procs[-1].returncode
            else:
                failed += 1
                print("FAIL  [e2e] %s (unknown kind %r)" % (name, kind))
                continue

            if got == want_out and rc == want_rc:
                if verbose:
                    print("ok    [e2e] %s" % name)
            else:
                failed += 1
                print("FAIL  [e2e] %s" % name)
                print("      want %r exit %d" % (want_out, want_rc))
                print("      got  %r exit %d" % (got, rc))
        except subprocess.TimeoutExpired:
            failed += 1
            print("FAIL  [e2e] %s (timeout)" % name)
        finally:
            shutil.rmtree(d, ignore_errors=True)
    print("=== e2e: %d cases, %d failures" % (total, failed))
    return failed


def run_edge(unish, timeout, verbose):
    """Fase 5: boundary/robustness cases. "GAP " names are known gaps."""
    total = failed = gaps = 0
    for name, script, want_out, want_rc in EDGE_CASES:
        total += 1
        d = tempfile.mkdtemp(prefix="edge_")
        try:
            got, rc, _ = run_one([unish], script, d, timeout)
            got_s = norm(got).decode("utf-8", "replace")
            if got_s == want_out and rc == want_rc:
                if verbose:
                    print("ok    [edge] %s" % name)
            elif name.startswith("GAP "):
                gaps += 1
                print("KNOWN GAP [edge] %s" % name)
                if verbose:
                    print("      want %r/%d got %r/%d" % (want_out, want_rc, got_s, rc))
            else:
                failed += 1
                print("FAIL  [edge] %s" % name)
                print("      script: %r" % script)
                print("      want %r exit %d" % (want_out, want_rc))
                print("      got  %r exit %d" % (got_s, rc))
        finally:
            shutil.rmtree(d, ignore_errors=True)
    print("=== edge: %d cases, %d failures, %d known gaps" % (total, failed, gaps))
    return failed


def run_cmd_cases(unish, timeout, verbose):
    """Per-command cases for commands whose flags need real fixtures."""
    total = failed = gaps = 0
    for name, script, want_out, want_rc in CMD_CASES:
        total += 1
        d = tempfile.mkdtemp(prefix="cmd_")
        try:
            got, rc, _ = run_one([unish], script, d, timeout)
            got_s = norm(got).decode("utf-8", "replace")
            if got_s == want_out and rc == want_rc:
                if verbose:
                    print("ok    [cmd] %s" % name)
            elif name.startswith("FINDING"):
                gaps += 1
                print("KNOWN GAP [cmd] %s" % name)
                if verbose:
                    print("      want %r/%d got %r/%d" % (want_out, want_rc, got_s, rc))
            else:
                failed += 1
                print("FAIL  [cmd] %s" % name)
                print("      script: %r" % script)
                print("      want %r exit %d" % (want_out, want_rc))
                print("      got  %r exit %d" % (got_s, rc))
        finally:
            shutil.rmtree(d, ignore_errors=True)
    print("=== cmd: %d cases, %d failures, %d known gaps" % (total, failed, gaps))
    return failed


def run_builtins(unish, timeout, verbose):
    """Shell builtins (cd, export, set, read, trap, shift, unset, ...)."""
    total = failed = gaps = 0
    for name, script, want_out, want_rc in BUILTIN_CASES:
        total += 1
        d = tempfile.mkdtemp(prefix="bi_")
        try:
            got, rc, _ = run_one([unish], script, d, timeout)
            got_s = norm(got).decode("utf-8", "replace")
            if got_s == want_out and rc == want_rc:
                if verbose:
                    print("ok    [builtin] %s" % name)
            elif name.startswith("FINDING"):
                gaps += 1
                print("KNOWN GAP [builtin] %s" % name)
                if verbose:
                    print("      want %r/%d got %r/%d" % (want_out, want_rc, got_s, rc))
            else:
                failed += 1
                print("FAIL  [builtin] %s" % name)
                print("      script: %r" % script)
                print("      want %r exit %d" % (want_out, want_rc))
                print("      got  %r exit %d" % (got_s, rc))
        finally:
            shutil.rmtree(d, ignore_errors=True)
    print("=== builtins: %d cases, %d failures, %d known gaps" % (total, failed, gaps))
    return failed


def run_functional(unish, timeout, verbose):
    """Functional tests: does the shell DO it, not just print it?

    Each case is run for real (spawning processes, killing them, waiting)
    and judged on observable effects. Tags record KNOWN GAPs per platform
    so they stay visible instead of silently passing.
    """
    total = failed = known = 0
    for name, script, want_out, want_code, tags in FUNCTIONAL_CASES:
        total += 1
        d = tempfile.mkdtemp(prefix="fn_")
        try:
            got_out, got_code, _ = run_one([unish], script, d, timeout)
            got = got_out.decode("utf-8", "replace")
            ok = got == want_out and got_code == want_code
            tag = "all" if "all" in tags else platform_key() if platform_key() in tags else None
            if ok:
                if verbose:
                    print("ok    [fn] %s" % name)
            elif tag:
                known += 1
                print("KNOWN GAP [fn] %s (%s)" % (name, tag))
                if verbose:
                    print("      want %r/%d  got %r/%d" % (want_out, want_code, got, got_code))
            else:
                failed += 1
                print("FAIL  [fn] %s" % name)
                print("      script: %r" % script)
                print("      want %r exit %d" % (want_out, want_code))
                print("      got  %r exit %d" % (got, got_code))
        finally:
            shutil.rmtree(d, ignore_errors=True)
    print("=== functional: %d cases, %d failures, %d known gaps" % (total, failed, known))
    return failed


if __name__ == "__main__":
    sys.exit(main())
