# Differential parity suite

unish promises GNU behavior on every OS. This suite keeps that honest:
every case is a shell script that runs **identically under unish and an
oracle shell** (GNU bash + coreutils) and must produce the same stdout,
the same exit code and the same files.

The oracles were captured from GNU coreutils 9.4 and bash 5.2; the same
cases run in CI against the Linux runner's own `/bin/bash`, so a drift
in either unish *or* GNU shows up.

## Running

    # build the shell first
    go build -o unish .

    # Linux (the CI shape; /bin/bash is GNU):
    python3 parity/runner.py --unish ./unish --oracle /bin/bash

    # Windows against Git Bash:
    python parity/runner.py --unish .\unish.exe ^
        --oracle "C:\Program Files\Git\bin\bash.exe@C:\Program Files\Git\usr\bin"

Add `--filter sed` to run one group and `--verbose` to list passes.

## What is compared

- stdout byte-for-byte (CRLF normalized)
- exit codes
- the files left in a fresh working directory (both sides run in their
  own temp dir)
- stderr presence (informational only: shells prefix diagnostics
  differently)

## xfails

Eight cases are `(script, reason)` pairs: known cosmetic deltas where
the *behavior* matches but wording differs (e.g. sed reports error
positions as `char N` slightly differently, and the bash builtin
prefixes `printf` diagnostics with `bash: line N:`). They are reported
as `XFAIL` and don't fail the run; if one starts passing it is reported
as `XPASS` so the tag can be removed.

## Oracle versions

The oracles were captured from **GNU coreutils 9.4 / bash 5.2** (what
the CI runner ships). Older toolchains differ in known ways — e.g. the
coreutils 8.32 in Git Bash says `Binary file … matches` where 9.4 says
`grep: …: binary file matches`, and MSYS keeps `//` in `basename` /
`dirname` because it is a network root there. Those are properties of
the old oracle, not unish bugs; the Linux CI run is authoritative.

## Adding cases

Add the script to `cases/*_cases.py` (one script per entry). If the
delta is knowingly cosmetic, use a `(script, reason)` tuple — but only
after checking the difference against a real GNU shell.
