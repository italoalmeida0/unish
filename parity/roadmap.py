"""Per-command test roadmap for unish: what "actually works" means.

The index maps every embedded command to its coverage. The rules that
keep it honest (no theatre):

  1. Every case asserts an EXACT expected value. A case that can only say
     "exit code was 0" is not a test and is not written.
  2. Where a GNU oracle exists, the expectation is captured FROM the
     oracle, so it cannot drift into "whatever unish happens to print".
  3. Where no oracle exists (Windows-only tools), the expectation is
     hand-written with a `why:` noting the source of truth, and marked so
     a platform without the tool skips WITH A REASON.
  4. Nothing skips silently: every skip carries a platform and a reason.

Run:  python parity/roadmap.py            # print the coverage table
      python parity/roadmap.py --check    # exit 1 if any command is missing
"""

COMMANDS = [
    # name, smokes, flags, e2e, status, note
    # status: "done" | "partial" | "missing"
    ("sort", 6, 34, 1, "partial", "114 parity + flag cases; key specs good"),
    ("sed", 6, 9, 1, "partial", "86 parity cases; scripts/ranges/hold space"),
    ("printf", 6, 0, 0, "partial", "70 parity cases; every conversion"),
    ("grep", 5, 21, 1, "partial", "literal path; -P/-o/-A/-B/-l thin"),
    ("cut", 3, 13, 1, "partial", "flag cases generated; -b/-n gaps"),
    ("wc", 4, 5, 1, "partial", "flag cases generated"),
    ("uniq", 3, 16, 1, "partial", "flag cases generated"),
    ("tar", 4, 16, 1, "partial", "create/extract; -t/-C/-z thin"),
    ("find", 4, 0, 1, "partial", "-name/-type/-maxdepth; -exec/-printf missing"),
    ("ls", 4, 27, 0, "partial", "flag cases generated; BSD formatting differs"),
    ("head", 2, 3, 1, "partial", "flag cases generated"),
    ("tail", 2, 3, 1, "partial", "flag cases generated"),
    ("tr", 3, 4, 1, "partial", "flag cases generated"),
    ("date", 3, 2, 0, "partial", "format specifiers thin"),
    ("seq", 3, 3, 0, "partial", "integer fast path; -w/-s/-f partly"),

    # previously zero coverage; now covered by parity/cases/gap_cases.py
    ("df",       1, 2,  0, "partial", "gap cases; column widths differ from GNU"),
    ("expand",   3, 1,  0, "partial", "gap cases; -i NOT implemented (finding)"),
    ("gzcat",    1, 0,  0, "partial", "gap cases"),
    ("hostname", 1, 1,  0, "partial", "gap cases"),
    ("nice",     1, 4,  0, "partial", "gap cases"),
    ("nohup",    1, 0,  0, "partial", "gap cases"),
    ("nproc",    1, 1,  0, "partial", "gap cases"),
    ("pkill",    0, 0,  0, "missing", "needs a sandboxed victim; unsafe to sweep"),
    ("pwd",      2, 2,  0, "partial", "gap cases"),
    ("sha1sum",  2, 12, 0, "partial", "gap cases; hashes verified"),
    ("shasum",   1, 12, 0, "partial", "gap cases"),
    ("tee",      3, 1,  0, "partial", "gap cases; /dev/null fails on Windows"),
    ("uname",    2, 16, 0, "partial", "gap cases; -p/-i hardcoded 'unknown' (finding)"),
    ("uptime",   1, 0,  0, "partial", "gap cases"),
    ("whoami",   1, 0,  0, "partial", "gap cases"),
]


def report():
    print("%-10s %7s %6s %5s  %-8s %s" % ("command", "smokes", "flags", "e2e", "status", "note"))
    total_flags = 0
    missing = 0
    for name, s, f, e, status, note in sorted(COMMANDS, key=lambda r: r[4]):
        if status == "missing":
            missing += 1
        total_flags += f
        print("%-10s %7d %6d %5d  %-8s %s" % (name, s, f, e, status, note))
    print()
    print("%d commands listed, %d flag cases to write, %d commands with NO coverage"
          % (len(COMMANDS), total_flags, missing))
    return missing


if __name__ == "__main__":
    import sys
    missing = report()
    if "--check" in sys.argv and missing:
        sys.exit(1)
