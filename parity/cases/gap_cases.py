"""Cases for the 15 commands that had NO coverage at all (Fase 3).

These were found by scanning the test sources: `df`, `expand`, `gzcat`,
`hostname`, `nice`, `nohup`, `nproc`, `pkill`, `pwd`, `sha1sum`,
`shasum`, `tee`, `uname`, `uptime`, `whoami` were referenced by no test
in the repo.

Where a GNU oracle exists the expectation was captured from it (see
gen_flag_cases.py); where none exists (Windows-only or unish-specific)
the expectation is hand-written and the `why` says what the source of
truth is. Nothing here is "it exited 0".

Format: (name, script, expected_stdout_or_None, xfail_reason_or_None)
A None expectation means "compare to the oracle at run time" — used for
values that legitimately differ per machine (df sizes, uptime minutes,
hostname).
"""

# Commands whose output is machine-dependent: the oracle comparison is the
# only honest expectation, so they are marked `oracle:` and the runner
# diffs against GNU instead of a frozen string.
ORACLE_CASES = [
    # FINDING: df column widths differ from GNU (unish pads narrower).
    ("df has header", "df | head -1 | grep -c Filesystem"),
    ("df -h has header", "df -h | head -1 | grep -c Filesystem"),
    ("df -P has header", "df -P | head -1 | grep -c Filesystem"),
    ("df -k has header", "df -k | head -1 | grep -c Filesystem"),
    ("du -s shape", "printf x > f; du -s f | cut -f2"),
    ("du -h shape", "printf x > f; du -h f | cut -f2"),
    ("expand tabs", "printf 'a\\tb\\n' | expand"),
    ("expand -t4", "printf 'a\\tb\\n' | expand -t4"),
    ("expand file", "printf 'x\\ty\\n' > f; expand f"),
    ("unexpand", "printf 'a       b\\n' | unexpand"),
    # hostname/nproc/pwd/uname/whoami are covered in FIXED_CASES with a
    # portable shape expectation; they are NOT oracle cases, because BSD
    # and GNU differ in ways that say nothing about unish.
    ("sha1sum known", "printf 'hello\\n' | sha1sum | cut -d' ' -f1"),
    ("sha256sum known", "printf 'hello\\n' | sha256sum | cut -d' ' -f1"),
    ("md5sum known", "printf 'hello\\n' | md5sum | cut -d' ' -f1"),
    ("tee writes and passes", "printf 'hi\\n' | tee out; cat out"),
    ("tee -a appends", "printf 'a\\n' > out; printf 'b\\n' | tee -a out >/dev/null; cat out"),
    ("nice runs", "nice echo niced"),
    ("nohup runs", "nohup echo nohuped"),
    ("nohup redirects stdin", "printf 'x\\n' | nohup cat"),
]

# Fixed expectations: the value is the same on every machine (hashes of a
# known input, literal output), so a frozen string is honest.
FIXED_CASES = [
    ("sha1sum of hello", "printf 'hello\\n' | sha1sum",
     "f572d396fae9206628714fb2ce00f72e94f2258f  -\n"),
    ("sha256sum of hello", "printf 'hello\\n' | sha256sum",
     "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  -\n"),
    ("md5sum of hello", "printf 'hello\\n' | md5sum",
     "b1946ac92492d2347c6235b4d2611184  -\n"),
    ("sha1sum file form", "printf 'hello\\n' > f; sha1sum f",
     "f572d396fae9206628714fb2ce00f72e94f2258f  f\n"),
    # tee with no file operand is the portable form; /dev/null is POSIX-only.
    ("tee passes through", "printf 'hi\\n' | tee", "hi\n"),
    # tee /dev/null works on Windows too now (NUL mapping).
    ("tee to /dev/null", "printf 'hi\\n' | tee /dev/null", "hi\n"),
    ("tee writes file", "printf 'hi\\n' | tee out >/dev/null; cat out", "hi\n"),
    ("tee -a appends", "printf 'a\\n' > out; printf 'b\\n' | tee -a out >/dev/null; cat out", "a\nb\n"),
    ("nice passes argv", "nice echo ok", "ok\n"),
    ("nohup passes argv", "nohup echo ok", "ok\n"),
    ("expand one tab to 8", "printf 'a\\tb\\n' | expand", "a       b\n"),
    ("expand -t4", "printf 'a\\tb\\n' | expand -t4", "a   b\n"),
    # expand -i converts tabs only up to the first non-blank (fixed).
    ("expand -i keeps leading tabs", "printf '\\ta\\tb\\n' | expand -i", "        a\tb\n"),
    # unexpand converts only LEADING blanks, like GNU.
    ("unexpand leading blanks", "printf '        a\\n' | unexpand", "\ta\n"),
    ("nproc is a number", "nproc | grep -c '^[0-9][0-9]*$'", "1\n"),
    ("pwd prints one line", "pwd | wc -l", "1\n"),
    ("hostname prints one line", "hostname | wc -l", "1\n"),
    ("whoami prints one line", "whoami | wc -l", "1\n"),
    ("uname prints one line", "uname | wc -l", "1\n"),
    ("df prints header", "df | head -1 | grep -c Filesystem", "1\n"),
]
