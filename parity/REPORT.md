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

## Real findings in unish — ALL FIXED

Every finding below was fixed, and its `FINDING`/`GAP` tag removed so the
case is now a real passing test.

### Job control (one root cause, six symptoms) — FIXED

`$!` yielded a fake id (`g1`), so `kill $!`/`kill %1`, `jobs`, `wait`,
`trap TERM` and `PIPESTATUS` were all broken.

Root cause, diagnosed with a handler spy rather than guessed: mvdan/sh only
reports a real pid for a background statement when
`execHandlerIsDefault && callHandler == nil`. unish registers both, so
`cmd &` ran as an in-process goroutine: no child process, empty job table.

Fix: the vendored interp always wires the started channel for a plain call
and exposes `InBackground`/`ReportBgStart`; `extraHandler` runs background
statements as real children (re-executing the unish binary), so `$!` is a
real pid. `main.go` catches TERM/HUP/QUIT; `kill -TERM $$` enqueues the
signal for the shell's own trap; PIPESTATUS accumulates every stage
(verified for 2, 3 and 4-stage pipelines against GNU).

### `env -i` did not clear the environment — FIXED

It was parsed and discarded, and it called `os.Clearenv`/`os.Setenv`,
mutating the whole shell. It now builds the child environment explicitly
and never touches the process environment.

### `uname -p`/`-i` hardcoded "unknown" — FIXED

They now report the real machine architecture.

### `expand -i` not implemented — FIXED

GNU semantics: tabs are converted only up to the first non-blank.

### `tee /dev/null` failed on Windows — FIXED

Mapped to NUL, like the shell's own redirection handling.

### `pgrep -x` never matched — FIXED

It compared `^name$` against the whole command line; it now matches the
command name.

### `pkill -s/-l/-n/-a/-g` not implemented — FIXED

All five now work (verified in the docker sandbox).

### `nc` sent nothing — FIXED

`pipeConn` returned when either direction ended, closing the connection
before the payload was written; it now waits for the stdin copier while
still not hanging on a half-open socket (the zombie regression test).

### `shift` past the end exited 0 — FIXED

Now exits 1 with "shift count out of range", like bash.

### `compgen`/`complete`/`declare -i` not implemented — FIXED

`compgen` lists matching words; `complete` accepts registration; `declare
-i` evaluates its value as arithmetic.

### Phantom flags in the table — FIXED

`cat -S` and `od -u` are not GNU flags at all; the table was corrected
(`cat -s` squeeze-blank, `od -d`/`-t u2`). Real missing flags were
implemented: `strings -a`, `md5sum/sha1sum/sha256sum -q/-s`, and
`hexdump -e` now applies the format instead of dumping hex.

### Platform gaps (reported, not hidden)

- `pgrep` on Windows cannot enumerate other processes (OS limitation)
- `pgrep`/`pkill` on macOS (no procfs) exit 2 where GNU exits 1
- `nc -u` UDP delivery is connectionless timing (harness limitation)
- `ps` on macOS has a BSD header

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
