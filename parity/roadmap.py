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
    ("sort", 6, 34, 1, "partial", "114 parity cases; key specs/-n/-u/-V good"),
    ("sed", 6, 9, 1, "partial", "86 parity cases; scripts/ranges/hold space"),
    ("printf", 6, 0, 0, "partial", "70 parity cases; every conversion"),
    ("grep", 5, 21, 1, "partial", "literal fast path; -P/-o/-A/-B/-l thin"),
    ("cut", 3, 13, 1, "partial", "-d/-f/-c/-s partly"),
    ("wc", 4, 5, 1, "partial", "-l/-w/-c/-m/-L; multi-file totals"),
    ("uniq", 3, 16, 1, "partial", "-c/-d/-u/-f/-s/-w"),
    ("tar", 4, 16, 1, "partial", "create/extract; -t/-C/-z thin"),
    ("find", 4, 0, 1, "partial", "-name/-type/-maxdepth; -exec/-printf missing"),
    ("ls", 4, 27, 0, "partial", "27 flags declared, ~6 exercised"),
    ("head", 2, 3, 1, "partial", "-n good; -c missing"),
    ("tail", 2, 3, 1, "partial", "-n good; -c/-f missing"),
    ("tr", 3, 4, 1, "partial", "-d/-s/-c partly"),
    ("date", 3, 2, 0, "partial", "format specifiers thin"),
    ("seq", 3, 3, 0, "partial", "integer fast path; -w/-s/-f partly"),

    # no coverage at all today (verified by scanning the test sources)
    ("df",       0, 2,  0, "missing", "never referenced in any test"),
    ("expand",   0, 1,  0, "missing", "never referenced in any test"),
    ("gzcat",    0, 0,  0, "missing", "never referenced in any test"),
    ("hostname", 0, 1,  0, "missing", "never referenced in any test"),
    ("nice",     0, 4,  0, "missing", "never referenced in any test"),
    ("nohup",    0, 0,  0, "missing", "never referenced in any test"),
    ("nproc",    0, 1,  0, "missing", "never referenced in any test"),
    ("pkill",    0, 14, 0, "missing", "14 flags declared, zero cases"),
    ("pwd",      0, 2,  0, "missing", "never referenced in any test"),
    ("sha1sum",  0, 12, 0, "missing", "never referenced in any test"),
    ("shasum",   0, 12, 0, "missing", "never referenced in any test"),
    ("tee",      0, 1,  0, "missing", "never referenced in any test"),
    ("uname",    0, 16, 0, "missing", "16 flags declared, zero cases"),
    ("uptime",   0, 0,  0, "missing", "never referenced in any test"),
    ("whoami",   0, 0,  0, "missing", "never referenced in any test"),
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
