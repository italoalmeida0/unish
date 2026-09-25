# Changelog

## 1.0.0 — 2026-09-25

First stable release. unish is one self-contained, bash-compatible shell
for Windows, macOS and Linux: a REPL plus a GNU coreutils-style command
set embedded in a single static binary that runs from anywhere, with no
DLLs and no installation.

### What this release stands on

- 1120 behavioural cases — differential parity against a GNU oracle,
  generated flag coverage for every declared flag (502 of 503, the one
  remainder being a non-flag), per-command fixtures, shell builtins,
  functional, edge/robustness, end-to-end process cases and a docker
  sandbox for the process/network tools — plus 86 Go tests. Zero
  failures.
- Enforced in CI on every push on all six shipped targets
  (linux/windows/macos × amd64/arm64), plus a musl/Alpine job that also
  proves the Linux artifacts are glibc-free.
- The release workflow rebuilds and re-runs the full differential suite
  against a GNU reference on Linux, Windows and macOS before anything is
  published, and smoke-tests every binary (`--version` must match the
  tag). Each release ships 6 binaries and SHA256SUMS.
- Known deltas from GNU are documented, not hidden: diagnostic wording on
  old coreutils (8.32), and real platform gaps (pgrep on Windows, BSD
  `ps` header and pgrep exit codes on macOS). See `parity/REPORT.md`.

### CI/test changes that make "green" mean what it says

- The behavioural suite in CI no longer runs with `--filter "__none__"`
  (which silently skipped the 372 differential cases on push) and the
  musl run no longer ends in `|| true` (it was hiding 17 failures — all
  of them busybox-vs-GNU oracle differences).
- Every differential oracle is now GNU: Homebrew's GNU tools + bash 5 on
  macOS (the system bash is 3.2 over a BSD userland, so the oracle was
  the outlier) and GNU coreutils/findutils/sed/grep/tar on Alpine
  (busybox output differs from GNU by design). Both setups assert the
  tools really report GNU before comparing anything.
- `parity/runner.py` lost its no-op `--skip-platform` flag and some dead
  env/locale plumbing. Neither ever affected a run.

## 0.1.0 – 0.9.3

The build-up: embedding the command set, the differential test campaign
(the findings and their fixes are recorded in `parity/REPORT.md`), real
job control (pids, kill/jobs/wait/trap/PIPESTATUS), and the native CI
matrix. Details live in git history.
