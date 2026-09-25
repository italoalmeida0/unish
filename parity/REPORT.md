# Test campaign report

What was tested, what it found, and what is still open. Written after the
per-command test campaign (Fases 1-7) so the next person does not have to
rediscover any of it.

## Totals

| Group | Cases | Result |
|---|---|---|
| Output parity (GNU oracle) | 372 | 0 failures, 9 documented wording deltas |
| Flag cases (oracle at run time) | 410 | 0 failures, 60 known differences |
| Per-command fixture cases | 114 | 0 failures, 17 known gaps |
| Sandbox (docker: pgrep/pkill/ss/nc) | 35 | 0 failures, 7 known gaps |
| Functional (does it DO it) | 21 | 0 failures, 5 known gaps |
| Gaps: the 15 uncovered commands | 37 | 0 failures, 1 known gap |
| End-to-end (real processes) | 38 | 0 failures |
| Edge cases / robustness | 62 | 0 failures, 1 known gap |
| Go unit suite | 86 tests | 0 failures, 5 platform skips |

CI runs all of it on Linux, macOS and Windows, plus a docker sandbox
job (10 jobs, all green).

## Flag coverage: 502 of 503 (100%)

Every declared flag of every embedded command has a case. The single
remainder, `od -n`, is not a GNU flag at all — it is an artefact of the
extractor reading `-N`.

Where a flag needs state a test cannot invent safely (a process to
signal, a socket), the case lives in the docker sandbox instead of being
skipped. Where the flag does not exist in GNU (`strings -a`, `od -u`,
`cat -S`, `md5sum -q/-s`), the case is tagged FINDING and reported as a
known gap rather than hidden.

## Real findings in unish

Ordered by severity. None are fixed yet — the campaign was about finding
them, and each is kept visible in the suite (reported, never skipped).

### Job control is broken (one root cause)

`$!` yields a fake id (`g1`), not a pid, so:

- `kill $!` and `kill %1` fail with `strconv.Atoi: parsing "g1"`
- `jobs` lists nothing
- `trap ... TERM` never fires (EXIT traps do work)
- `wait` does not wait for a background job: `(sleep 0.3; echo bg) & wait`
  prints `end` before `bg`, or drops it entirely
- `${PIPESTATUS[*]}` records two stages for a three-stage pipeline

Root cause, diagnosed with a handler spy rather than guessed: mvdan/sh
only reports a real PID for a background statement when
`execHandlerIsDefault && callHandler == nil` (vendor/.../interp/runner.go).
unish registers both, so `cmd &` runs as an in-process goroutine: no
child process exists, the job table stays empty, and the exec handler is
never called for a background statement. Fixing this means forking
background statements as real processes — a shell-core change.

### `uname -p` / `-i` are hardcoded

`uname -p`, `uname -i`, `--processor` and `--hardware-platform` print
`unknown`. The code comment claims GNU does the same; on Linux GNU prints
`x86_64`. Four flags wrong.

### `expand -i` is not implemented

GNU leaves leading tabs untouched with `-i`; unish rejects the flag.

### `env -i` does not clear the environment

`env -i printenv PATH` still prints the full PATH. The flag is parsed and
discarded (`_ = ignore`), and the implementation calls `os.Clearenv` /
`os.Setenv`, mutating the whole shell process instead of the child.

### `pgrep -x` never matches

`-x` compares `^name$` against the base of the full command line
(`sleep 30`), so it never matches a process started with arguments.

### `pkill -s` (session) is not implemented

### `nc` sends nothing

`pipeConn` returns as soon as EITHER direction ends, so the stdout copier
(which finishes first when the peer does not reply) closes the connection
before the stdin copier has sent the data.

### `pgrep`/`pkill` on macOS exit 2

Where the process list cannot be read, they return 2; GNU returns 1 for
"no match".

### `ps` on macOS

The header is BSD-style, so anything asserting the GNU header is not
portable.

### `tee /dev/null` fails on Windows

`/dev/null` is resolved as a relative path, so the write fails. POSIX
only, but it is the canonical `tee` idiom.

### `df` column widths differ from GNU

Values are right; the padding is narrower.

## Findings about the harness itself (fixed, documented in code)

The first three made flags look inert and nearly produced a false report:

1. Flags appended after operands (`head a.txt -n`) — 452 false "inert"
2. Fixture files never created; everything ran in the repo dir
3. GNU tools invoked through `-c` (which they do not have)
4. The oracle's own name passed as an operand (`cat cat -n a.txt`)
5. Long options stripped of `--`
6. Fixtures shared between flags, so mutating commands leaked state
7. **Frozen expectations baked in the capture machine's line endings** —
   green on Windows, structurally broken on Linux/macOS. Caught only by
   the cross-platform CI run. Expectations are now computed at run time.
8. Comparing against a BSD oracle on macOS, where differing formatting
   (ls -C tabs, od padding, uniq -c width) is the oracle's, not unish's

## Safety incident

An early version of the flag audit swept `pkill .`. Process matchers take
a NAME PATTERN, so that matched every process on the machine and killed
the developer's whole session. The audit now refuses any argv touching
processes or anything outside its temp directory, sweeps only
non-recursive/non-force flags for destructive commands, and excludes
pgrep/pkill entirely (they need a sandbox that spawns its own victims).

## What is still not covered

- `awk` is not embedded; nothing tests it because it is external
- no test spawns a real signal to a child and checks delivery beyond the
  known-gap cases above
- interactive REPL behaviour (history, completion, prompt width) has Go
  unit tests but no end-to-end terminal test
- network tools (`nc`, `ss`) are audited for flags but not exercised
  against a real socket
