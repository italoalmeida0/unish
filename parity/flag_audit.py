"""Flag audit: for every flag unish declares, does it actually DO something?

For each (command, flag) it runs the command twice — with and without the
flag — against a GNU oracle where one exists, and classifies:

  WORKS    - unish output == oracle output (or, with no oracle, the flag
             changed the output in the documented way)
  INERT    - unish accepts the flag but the output is identical to
             running without it (declared but ignored -> fake coverage)
  BROKEN   - unish errors, panics, or exits non-zero where the oracle
             does not
  NEEDS-FIXTURE - the flag needs setup this harness cannot invent
             (a tty, a network, another user, an archive format...)
  DIFFERS  - both ran, outputs differ (real semantic gap)

The point is evidence, not opinion: a flag is only "tested" once we know
its expected output.

SAFETY (learned the hard way)
-----------------------------
An earlier version of this file swept `pkill .`. Process-matching tools
take a NAME PATTERN, not a path: `pkill .` matches every process on the
machine, and it terminated the developer's entire session. The audit now
refuses any argv that could touch processes or anything outside its temp
fixture directory (`unsafe()` + FORBIDDEN_COMMANDS), and sweeps only
non-recursive, non-force flags for destructive commands (SAFE_FLAGS).
Never add `pkill`, `pgrep`, `kill`, `killall` or a bare filesystem root
to the fixtures.
"""
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Per command: a fixture (files created before running) and a base
# invocation the flags attach to.
FIXTURES = {
    "default": {
        "files": {"a.txt": "alpha\nbravo\ncharlie\n", "b.txt": "delta\necho\nfoxtrot\n"},
    },
    "grep": {
        "files": {"a.txt": "alpha\nbravo\ncharlie\n", "b.txt": "delta\necho\nfoxtrot\n"},
        "base": "alpha",
    },
    "sort": {"files": {"a.txt": "3 c\n1 a\n2 b\n"}, "base": "a.txt"},
    "sed": {"files": {"a.txt": "alpha\nbravo\ncharlie\n"}, "base": "s/a/A/ a.txt"},
    "cut": {"files": {"a.txt": "a:b:c\nd:e:f\n"}, "base": "-d: -f1 a.txt"},
    "wc": {"files": {"a.txt": "alpha\nbravo\ncharlie\n"}, "base": "a.txt"},
    "uniq": {"files": {"a.txt": "x\nx\ny\nz\nz\n"}, "base": "a.txt"},
    "ls": {"files": {"a.txt": "a", "b.txt": "b"}, "base": "."},
    "head": {"files": {"a.txt": "1\n2\n3\n4\n5\n"}, "base": "a.txt"},
    "tail": {"files": {"a.txt": "1\n2\n3\n4\n5\n"}, "base": "a.txt"},
    "tr": {"base": "'abc' 'xyz'"},
    "tar": {"files": {"a.txt": "a", "b.txt": "b"}, "base": "cf out.tar a.txt b.txt"},
    "find": {"files": {"a.txt": "a"}, "base": "."},
    "date": {"base": "+%Y"},
    "seq": {"base": "3"},
    "df": {"base": ""},
    "du": {"files": {"a.txt": "a"}, "base": "."},
    "stat": {"files": {"a.txt": "a"}, "base": "a.txt"},
    "od": {"files": {"a.txt": "abc\n"}, "base": "a.txt"},
    "nl": {"files": {"a.txt": "x\ny\n"}, "base": "a.txt"},
    "paste": {"files": {"a.txt": "1\n2\n", "b.txt": "x\ny\n"}, "base": "a.txt b.txt"},
    "comm": {"files": {"a.txt": "1\n2\n", "b.txt": "2\n3\n"}, "base": "a.txt b.txt"},
    "join": {"files": {"a.txt": "1 x\n2 y\n", "b.txt": "1 p\n2 q\n"}, "base": "a.txt b.txt"},
    "split": {"files": {"a.txt": "1\n2\n3\n"}, "base": "a.txt"},
    "tee": {"files": {"a.txt": "data\n"}, "base": "a.txt", "stdin": True},
    "base64": {"files": {"a.txt": "hello\n"}, "base": "a.txt"},
    "md5sum": {"files": {"a.txt": "hello\n"}, "base": "a.txt"},
    "sha1sum": {"files": {"a.txt": "hello\n"}, "base": "a.txt"},
    "sha256sum": {"files": {"a.txt": "hello\n"}, "base": "a.txt"},
    "shasum": {"files": {"a.txt": "hello\n"}, "base": "a.txt"},
    "strings": {"files": {"a.txt": "hello\n"}, "base": "a.txt"},
    "cmp": {"files": {"a.txt": "same\n", "b.txt": "same\n"}, "base": "a.txt b.txt"},
    "diff": {"files": {"a.txt": "1\n2\n", "b.txt": "1\n3\n"}, "base": "a.txt b.txt"},
    "fold": {"files": {"a.txt": "abcdefghij\n"}, "base": "a.txt"},
    "expand": {"files": {"a.txt": "a\tb\n"}, "base": "a.txt"},
    "unexpand": {"files": {"a.txt": "a       b\n"}, "base": "a.txt"},
    "rev": {"files": {"a.txt": "abc\n"}, "base": "a.txt"},
    "tac": {"files": {"a.txt": "1\n2\n"}, "base": "a.txt"},
    "cat": {"files": {"a.txt": "hello\n"}, "base": "a.txt"},
    "gzip": {"files": {"a.txt": "hello\n"}, "base": "a.txt", "writes": "a.txt.gz"},
    "cp": {"files": {"a.txt": "a"}, "base": "a.txt copy.txt"},
    "mv": {"files": {"a.txt": "a"}, "base": "a.txt moved.txt"},
    "rm": {"files": {"a.txt": "a"}, "base": "a.txt"},
    "ln": {"files": {"a.txt": "a"}, "base": "a.txt link.txt"},
    "mkdir": {"base": "newdir"},
    "touch": {"base": "newfile"},
    "chmod": {"files": {"a.txt": "a"}, "base": "644 a.txt"},
    "mktemp": {"base": ""},
    "readlink": {"files": {"a.txt": "a"}, "base": "a.txt"},
    "realpath": {"files": {"a.txt": "a"}, "base": "a.txt"},
    "basename": {"base": "a/b/c.txt"},
    "dirname": {"base": "a/b/c.txt"},
    "id": {"base": ""},
    "whoami": {"base": ""},
    "hostname": {"base": ""},
    "uname": {"base": ""},
    "pwd": {"base": ""},
    "printenv": {"base": "PATH"},
    "env": {"base": ""},
    "nproc": {"base": ""},
    "uptime": {"base": ""},
    "ps": {"base": ""},
    # NOTE: pgrep/pkill/kill are deliberately absent. They match by process
    # NAME, so a flag sweep would run eg `pkill .` and terminate every
    # process on the machine (this actually happened). They require a
    # sandboxed test that spawns its own victims and never touches others.
    "sleep": {"base": "0"},
    "timeout": {"base": "1 true"},
    "nice": {"base": "true"},
    "nohup": {"base": "true"},
    "xargs": {"base": "echo"},
    "which": {"base": "sh"},
    "true": {"base": ""},
    "false": {"base": ""},
    "yes": {"base": ""},
    "seq2": {"base": ""},
}

# Flags that need something this harness cannot fake (documented, not
# silently skipped).
NEEDS_FIXTURE = (
    "-t", "--tty", "-i", "--interactive", "-f", "--follow", "--net", "-l",
    "--listen", "--devices", "--all", "--context", "--dereference",
)


def read_flags():
    """Every declared flag per command, plus which ones take a value."""
    src = ""
    for f in os.listdir(REPO):
        if f.endswith(".go") and not f.endswith("_test.go"):
            src += open(os.path.join(REPO, f), encoding="utf-8", errors="replace").read()
    out = {}
    for m in re.finditer(
        r'"([a-zA-Z0-9_]+)":\s*\{bools:\s*"([^"]*)",\s*values:\s*"([^"]*)",\s*long:\s*map\[string\]string\{([^}]*)\}',
        src,
    ):
        name, bools, values, longs = m.groups()
        fl = ["-" + c for c in bools] + ["-" + c for c in values]
        # Long options keep their `--` prefix: handing GNU `cat number`
        # instead of `cat --number` produces empty output and looks like a
        # unish bug.
        for lm in re.finditer(r'"([^"]+)":\s*"[^"]+"', longs):
            fl.append("--" + lm.group(1))
        out[name] = sorted(set(fl))
    return out


# Commands that can affect the machine (processes, devices, the filesystem
# outside our temp dir). The audit must never invoke these.
FORBIDDEN_COMMANDS = {
    "pkill", "kill", "killall", "pgrep", "ps", "reboot", "shutdown",
    "dd", "mkfs", "fdisk", "mount", "umount", "shred",
}

# Flags whose operand is not a path but a pattern/host/etc; running them
# blind is meaningless or dangerous.
FORBIDDEN_FLAG_PREFIXES = ("--net", "--listen", "-o", "--dev")

# Destructive commands: only these flags may be swept. Anything recursive
# or forced (rm -r/-f, chmod -R, chown -R, cp -r outside a fixture...) is
# excluded, because a sweep has no idea what the operand really is.
SAFE_FLAGS = {
    "rm": {"-i", "-v"},
    "mv": {"-n", "-v"},
    "cp": {"-n", "-v", "-i"},
    "ln": {"-s", "-v"},
    "chmod": {"-v"},
    "gzip": {"-k"},
}


def unsafe(argv):
    """True when an argv must not be executed by this harness."""
    if not argv:
        return True
    if argv[0] in FORBIDDEN_COMMANDS:
        return True
    for a in argv:
        if a == "/" or a == "/*" or a.startswith("/dev/") or a == "..":
            return True
        if a.startswith("../"):
            return True
        if any(a.startswith(p) for p in FORBIDDEN_FLAG_PREFIXES):
            return True
    return False


def oracle_path(cmd):
    """Find the GNU tool for `cmd`, honouring ORACLE_PATH when set.

    ORACLE_PATH exists because the shell under test (unish) shadows these
    names with builtins: a bare PATH lookup inside a unish session finds
    the builtin, not GNU. CI exports ORACLE_PATH=/usr/bin; on Windows it
    points at Git's usr/bin.
    """
    dirs = []
    env = os.environ.get("ORACLE_PATH", "")
    if env:
        dirs += env.split(os.pathsep)
    dirs += os.environ.get("PATH", "").split(os.pathsep)
    for d in dirs:
        if not d:
            continue
        for name in (cmd, cmd + ".exe"):
            p = os.path.join(d, name)
            if os.path.isfile(p):
                return p
    return None


def run(binary, argv, cwd, stdin=b"", shell=False):
    """Run a command and return (rc, stdout, stderr).

    shell=False -> the binary takes the argv directly (GNU tools).
    shell=True  -> the binary is a SHELL and the command is a builtin, so
                   it goes through `-c` the way a user invokes it
                   (`unish -c "head -n 2 a.txt"`).

    Getting this wrong is why an earlier run reported 363 "no output":
    GNU cat was being handed a `-c` flag it does not have.
    """
    if unsafe(argv):
        return -2, b"<REFUSED: unsafe argv>", b""
    if not shell:
        # argv already names the command (["cat","-n","a.txt"]); `binary`
        # is the resolved path to it, so drop the leading name or GNU sees
        # it as an extra operand (the `cat cat -n a.txt` bug).
        if argv and os.path.basename(binary).split(".")[0] == argv[0]:
            argv = argv[1:]
    full = [binary] + (["-c", " ".join(shlex.quote(a) for a in argv)] if shell else argv)
    try:
        p = subprocess.run(full, cwd=cwd, input=stdin,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
        return p.returncode, p.stdout, p.stderr
    except subprocess.TimeoutExpired:
        return 124, b"<TIMEOUT>", b""
    except FileNotFoundError:
        return -3, b"<NO-BINARY: %s>" % binary.encode(), b""
    except Exception as e:  # noqa: BLE001
        return -1, b"<ERROR %s>" % str(e).encode(), b""


def main():
    unish = sys.argv[1] if len(sys.argv) > 1 else os.path.join(REPO, "unish.exe")
    only = sys.argv[2] if len(sys.argv) > 2 else None
    flags = read_flags()
    results = []

    for cmd in sorted(flags):
        if only and cmd != only:
            continue
        fx = FIXTURES.get(cmd, FIXTURES["default"])
        base = fx.get("base", "")
        base_argv = shlex.split(base) if base else []
        # A fresh directory with the command's fixture files: without this
        # the operands do not exist and every flag looks inert.
        work = tempfile.mkdtemp(prefix="flagaudit_")
        for name, content in fx.get("files", {}).items():
            with open(os.path.join(work, name), "w") as fh:
                fh.write(content)
        safe = SAFE_FLAGS.get(cmd)
        for fl in flags[cmd]:
            if safe is not None and fl not in safe:
                results.append((cmd, fl, "SKIPPED-DESTRUCTIVE",
                                "recursive/force flag not swept (safety)"))
                continue
            if any(fl == nf or fl.startswith(nf) for nf in NEEDS_FIXTURE):
                results.append((cmd, fl, "NEEDS-FIXTURE", "needs tty/net/other"))
                continue
            # FIXTURES["base"] holds only the OPERANDS; the command name
            # is always `cmd`. (`head`'s base is "a.txt", not
            # "head a.txt".) Flags precede operands, as a user writes them.
            argv = [cmd, fl] + base_argv
            base_run = [cmd] + base_argv if base_argv else [cmd]
            u_rc, u_out, u_err = run(unish, argv, work, shell=True)
            b_rc, b_out, b_err = run(unish, base_run, work, shell=True)

            if u_rc != 0 and b_rc == 0 and b_err:
                results.append((cmd, fl, "BROKEN", u_err.decode("utf-8", "replace").strip()[:80]))
                continue
            if u_out == b_out:
                # A flag that changes nothing may still be legitimate (e.g.
                # `-c` on a file with no newline). Ask the oracle whether
                # the flag is supposed to change anything at all.
                o0 = oracle_path(cmd)
                if o0 and run(o0, argv, work)[1] != run(o0, base_run, work)[1]:
                    results.append((cmd, fl, "INERT", "GNU changes output; unish does not"))
                else:
                    results.append((cmd, fl, "INERT", "no output change in either shell"))
                continue
            o = oracle_path(cmd)
            if o:
                o_rc, o_out, _ = run(o, argv, work)
                if u_out == o_out:
                    results.append((cmd, fl, "WORKS", "matches GNU"))
                else:
                    results.append((cmd, fl, "DIFFERS", "unish!=oracle"))
            else:
                results.append((cmd, fl, "WORKS", "no oracle; output differs from no-flag"))

        shutil.rmtree(work, ignore_errors=True)

    # report
    from collections import Counter
    c = Counter(s for _, _, s, _ in results)
    print("=== flag audit: %d flags checked ===" % len(results))
    for k, v in c.most_common():
        print("  %-14s %d" % (k, v))
    print()
    for cmd, fl, status, why in results:
        if status in ("BROKEN", "DIFFERS"):
            print("%-14s %-10s %-10s %s" % (cmd, fl, status, why))
    out = os.path.join(tempfile.gettempdir(), "flag_audit.json")
    json.dump([{"cmd": c_, "flag": f_, "status": s_, "why": w_} for c_, f_, s_, w_ in results],
              open(out, "w"), indent=1)
    print("\nfull audit -> %s" % out)


if __name__ == "__main__":
    main()
