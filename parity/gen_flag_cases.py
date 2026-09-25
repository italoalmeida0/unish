"""Generate flag parity cases from measured oracle output.

Fase 2 of the per-command roadmap: the audit told us which 232 flags
WORK. This turns each of those into a real regression case whose
expected output is what the GNU oracle actually printed — so the case
can never drift into "whatever unish happens to output".

Cases are written to parity/cases/flag_cases.py as (script, expected,
xfail) triples with a distinct shape from the shell-script cases, and
the runner grows a branch to execute them.
"""
import json
import os
import shlex
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import flag_audit as fa  # noqa: E402


def oracle(cmd):
    return fa.oracle_path(cmd)


def main():
    unish = sys.argv[1] if len(sys.argv) > 1 else os.path.join(REPO, "unish.exe")
    flags = fa.read_flags()
    takes_value = fa.read_value_flags()
    cases = []
    skipped = []

    for cmd in sorted(flags):
        if cmd in fa.FORBIDDEN_COMMANDS:
            continue
        fx = fa.FIXTURES.get(cmd, fa.FIXTURES["default"])
        base = fx.get("base", "")
        ops = shlex.split(base) if base else []
        if not oracle(cmd):
            skipped.append((cmd, "no oracle on this machine"))
            continue

        work = tempfile.mkdtemp(prefix="genflag_")
        for name, content in fx.get("files", {}).items():
            with open(os.path.join(work, name), "w") as fh:
                fh.write(content)

        safe = fa.SAFE_FLAGS.get(cmd)
        for fl in flags[cmd]:
            if safe is not None and fl not in safe:
                continue
            if any(fl == nf or fl.startswith(nf) for nf in fa.NEEDS_FIXTURE):
                continue
            # A value is attached ONLY for flags the command table marks as
            # value-taking. Attaching one to a boolean flag (cat --number)
            # makes the operand a stray argument and the case meaningless.
            val = fa.FLAG_VALUES.get(fl, "") if fl in takes_value.get(cmd, set()) else ""
            if val:
                argv = [cmd, fl, val] + ops
            else:
                argv = [cmd, fl] + ops
            if fa.unsafe(argv):
                continue
            # Fresh fixtures per flag: mutating commands (cp/mv/rm/gzip)
            # otherwise leave the directory changed for the next flag and
            # the captured expectations become order-dependent nonsense.
            for name in os.listdir(work):
                p_ = os.path.join(work, name)
                if os.path.isdir(p_):
                    shutil.rmtree(p_, ignore_errors=True)
                else:
                    os.remove(p_)
            for name, content in fx.get("files", {}).items():
                with open(os.path.join(work, name), "w") as fh:
                    fh.write(content)
            # Capture the oracle's exact result: this IS the expectation.
            feed = b"abc xyz\n" if fx.get("stdin") else b""
            o_rc, o_out, _ = fa.run(oracle(cmd), argv, work, stdin=feed)  # GNU: direct argv
            # Git Bash's coreutils emit CRLF; normalise so the captured
            # expectation is about semantics, not the terminal.
            o_out = o_out.replace(b"\r\n", b"\n")
            if o_rc == 124:
                # The oracle hung (tail --follow, nc --listen): a test that
                # blocks forever is not a test.
                skipped.append((cmd + " " + fl, "oracle hangs (needs a live target)"))
                continue
            if o_rc != 0 and not o_out:
                # Non-zero with no output is meaningful for some flags
                # (sort --check reports disorder), but only when the exit
                # status itself is the observable. Keep those; drop the
                # ones where the oracle simply refused the flag.
                if b"unrecognized option" in _ or b"invalid option" in _ or b"unknown option" in _:
                    skipped.append((cmd + " " + fl, "oracle rejects this flag"))
                    continue
            u_rc, u_out, _ = fa.run(unish, argv, work, stdin=feed, shell=True)  # unish: builtin via -c
            u_out = u_out.replace(b"\r\n", b"\n")
            same = u_out == o_out and u_rc == o_rc
            if same:
                note = ""
            elif u_out == o_out:
                # stdout matches but the exit status does not: unish
                # rejected a flag GNU accepts (or vice versa).
                note = "EXIT: unish=%d oracle=%d" % (u_rc, o_rc)
            else:
                note = "DIFFERS: unish=%r" % u_out[:60]
            cases.append({
                "cmd": cmd, "flag": fl, "argv": argv, "rc": o_rc,
                "expect": o_out.decode("utf-8", "replace"),
                "matches": same, "note": note,
                # Fixtures travel with the case: the runner must recreate
                # the exact bytes the oracle saw, or the expectation is
                # meaningless (uniq on x/x/y/z/z is not uniq on text).
                "files": fx.get("files", {}),
            })
        shutil.rmtree(work, ignore_errors=True)

    out = os.path.join(REPO, "parity", "cases", "flag_cases.py")
    with open(out, "w", encoding="utf-8", newline="\n") as fh:
        fh.write('"""Flag cases generated from measured GNU oracle output.\n\n')
        fh.write('Each entry is (argv, expected_stdout, xfail). The expectation is\n')
        fh.write('what GNU actually printed for that exact invocation — regenerated by\n')
        fh.write('parity/gen_flag_cases.py, never hand-written.\n"""\n\n')
        fh.write("# (argv:list[str], xfail:str, files:dict, oracle_exit:int)\n")
        fh.write("# The expected stdout is produced by the oracle at run time, not\n")
        fh.write("# frozen here: a captured string carries the capture machine's\n")
        fh.write("# line endings and would fail on another platform.\n")
        fh.write("FLAG_CASES = [\n")
        for c in cases:
            xf = "" if c["matches"] else c["note"]
            fh.write("    (%r, %r, %r, %r),\n" % (c["argv"], xf, c["files"], c["rc"]))
        fh.write("]\n")

    ok = sum(1 for c in cases if c["matches"])
    print("generated %d flag cases (%d match GNU, %d differ)" % (len(cases), ok, len(cases) - ok))
    print("skipped %d:" % len(skipped))
    for name, why in skipped[:20]:
        print("   %-24s %s" % (name, why))
    print("-> %s" % out)


if __name__ == "__main__":
    main()
