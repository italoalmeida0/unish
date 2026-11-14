# Changelog

## 1.1.0 — 2026-11-14

### Added

- **Embedded POSIX / GNU `awk` (`gawk`) engine.** Full pure-Go AWK lexer
  (context-sensitive `/re/` vs `/` division), recursive-descent AST parser,
  and execution VM: `BEGIN`/`END` blocks (`exit` status preservation and
  override), pattern/expression/range rules, `$0`/`$1..$NF` bidirectional
  resplitting with mid-record `FS` snapshotting (`FS = ""` UTF-8 rune split,
  single-char, default whitespace, ERE regex, and `RS = ""` paragraph mode),
  strnum comparisons, multi-dimensional associative arrays (`SUBSEP`, `in`,
  `delete`), user-defined recursive functions with local array/scalar
  parameters, string/math/time builtins (`printf`/`sprintf` with exact
  64-bit integer formatting and `+inf`/`-inf`/`nan`, `sub`/`gsub`/`gensub`,
  `match`/`RSTART`/`RLENGTH`, `split`, `substr`, `index`, `systime`),
  `getline`, output redirections, command pipes (`cmd | getline`,
  `print | cmd`, `close(cmd)`), and `system(cmd)`.
- **Embedded `jq` 1.7 JSON processor.** Pure-Go streaming JSON and filter
  engine with exact 64-bit `int64` arithmetic and comparisons (preserving
  Snowflake/GitHub IDs without 53-bit `float64` precision loss, promoting on
  overflow, and passing >64-bit integers raw), insertion-ordered objects
  (`-S` sorted), generator backtracking, full path and update assignments
  (`=`, `|=`, `+=`, `-=`, `*=`, `//=`, `del(...)`, slice assignment, nested
  auto-vivification), `try`/`catch`, `reduce`/`foreach`, recursive `def`
  closures, string interpolation and formatters (`@uri`, `@base64`,
  `@base64d`, `@csv`, `@tsv`, `@sh`, `@html`, `@json`, `@text`), regex
  builtins (`test`, `match`, `capture`, `sub`, `gsub`, `splits`), and CLI
  parity (`-r`, `-j`, `-0`, `-c`, `-s`, `-R`, `-n`, `-e`, `--arg`,
  `--argjson`, `--slurpfile`, `--rawfile`, `--args`, `--jsonargs`, `$ENV`,
  `$ARGS`).
- **Agent & automation power toolkit (`rg`, `patch`, `bc`, `sponge`,
  `flock`, `column`, `xxd`).**
  - `rg`: parallel recursive search honoring hierarchical
    `.gitignore`/`.ignore`/`.rgignore` rules (including negation `!`,
    directory-only `/`, and anchored patterns), binary NUL skipping,
    smart-case (`-S`), fixed-strings (`-F`), globs (`-g`), file types
    (`-t`/`-T`), context lines (`-A`/`-B`/`-C`), `--files`, `--vimgrep`,
    `$1` capture replacement (`-r`), and line-streaming `EPIPE` early exit.
  - `patch`: unified diff applier supporting `-pN` path stripping, file
    creation (`--- /dev/null`) and deletion (`+++ /dev/null`), fuzzy hunk
    offset matching, `--dry-run`, `-R` reverse patches, `-N` forward
    idempotence, `-b` backups, `-o` output redirection, and
    `\ No newline at end of file` fidelity.
  - `bc`: POSIX + GNU `bc` arbitrary-precision calculator (`math/big`) with
    `-l` standard math library (`scale=20`, `s(x)`, `c(x)`, `a(x)`, `l(x)`,
    `e(x)`, `j(n,x)`), `ibase`/`obase` (bases 2–16 with POSIX ordering),
    arrays, control flow, and recursive `define` functions with `auto`
    locals.
  - `sponge`, `flock`, `column`, `xxd`: atomic soak-before-write `sponge`
    (`-a` append) for safe in-place pipelines; cross-platform advisory
    `flock` (`unix.Flock` / Windows `LockFileEx`, `-s`/`-x`/`-u`/`-n`/
    `-w`/`-E`, lockfile + command and FD modes); UTF-8 and CJK
    display-width-aware `column` (`-t`/`-s`/`-o`/`-n`/`-c`); and
    canonical/plain/C-include/bits `xxd` hex dumper with binary `-r` and
    `-r -p` reverse patching.

### Performance

- **Zero-allocation shell execution engine & pooled in-memory pipelines.**
  Added single-pass `RegisterFastCallHandler` (`CallHandled`) and zero-alloc
  `runTest` (`[`/`test`), `cmdPrintf`, `echo`, and virtual `/dev/null`
  redirections; eliminated per-iteration heap allocations across arithmetic
  loops (`$((...))`), parameter expansions (`${#v}`, `${v#pat}`,
  `${v/sub/rep}` with bounded regexp memoization), and Windows
  case-insensitive environment lookups; and replaced OS pipe syscalls on
  pure in-process pipelines with a race-free 64 KB `sync.Pool` ring-buffered
  `memPipe` (~10 GB/s throughput, with automatic OS-pipe bridging for
  external subprocesses and wrapper commands).
- **Coreutils SIMD literal fast paths, zero-alloc streaming & Schwartzian
  `sort`.** Added `bytes.Index`/`bytes.Count` literal fast paths to `grep`
  and `sed` (`s/lit/rep/`), chunked 64 KB line/word/UTF-8 rune counting in
  `wc` (`O(1)` `Stat` on regular `wc -c`), zero-alloc byte-slice streaming
  across `cut`, `tr` (`[256]byte` LUT), `uniq`, `head`, `seq`
  (`strconv.AppendInt`), and `cat`, pre-extracted `int64`/`float64`/byte
  Schwartzian sort keys in `sort` (`-n`, `-k`, `-u`), `fs.DirEntry`-based
  `find` traversal (eliminating redundant `Lstat` calls), and pooled buffers
  in `tar`.

## 1.0.2 — 2026-11-14

### Fixed

- **Shell engine, subshells, descriptors, process substitution & traps.**
  Patched vendored `mvdan/sh` parameter and arithmetic expansion to fix
  `${var/pat}` nil-replacement panics, quoted/escaped `#`/`%` non-anchors,
  `&`/`\&` replacement semantics, `(( arr[i] = ... ))` array assignments,
  arithmetic-tokenized associative subscripts (`expand.AssocKey`), and
  `declare -i` / `local -i` attribute preservation (`-ir`, `-ix`).
  Isolated FDs > 2 (`extraFds`/`extraInFds`) per statement unless `exec`
  and across subshells without closing parent descriptors; synchronized
  `>(cmd)` write-side drain at statement boundaries and unblocked
  unconsumed `<(cmd)`/`>(cmd)` POSIX FIFOs with refcounted cleanup.
  Normalized `SIG`-prefixed `trap` names and `trap -p`, separated
  `trap ''` ignore from `trap -` reset, inherited ignored traps and ran
  `EXIT` traps inside subshells (`(...)`, `$(...)`, `<(...)`, pipelines,
  `&`) while stopping `os/signal` channels on teardown, delivered
  `kill -<sig> $$` self-signals in-process (exiting `128+sig` on
  untrapped fatal signals), stabilized job IDs and `wait`/`timeout`/
  `kill -0` races, and fixed dash-prefixed script/`-c` positional args
  plus `type`/`which`/`hash --`/`umask`/`ulimit`.
- **Interactive REPL, raw-mode lifecycle, stdin isolation, UTF-8
  completion & history.** Replaced `bufio.Scanner` read-ahead in non-TTY
  `plainLoop` with a 1-byte reader so child processes never lose stdin;
  restored terminal mode cleanly on `exit 0`; preserved `$?` across
  `PROMPT_COMMAND` and ran `EXIT` traps on `EOF`/`Ctrl+D`; synchronized
  `rawOn`, `promptSigCh`, and history slices across raw-mode suspend/
  resume; and aborted unsubmitted partial input (`PS1`/`PS2`) on PTY
  hangup without spinning on read errors. Fixed `Alt+Backspace` and
  `Alt+Y` (`yank-pop`) empty-state panics, CSI private-mode (`\x1b[?2004h`)
  sequence leaks, bracketed-paste tab/CRLF handling, multi-byte UTF-8
  cursor slicing and escaped spaces in tab completion, trailing-backslash
  line continuations, `HISTCONTROL` (`erasedups`/`ignorespace`),
  `HISTFILESIZE=0` / `HISTFILE=/dev/null`, `history -a/-n/-r/-w/-d`,
  `Ctrl+R` reverse-search cycling, and `\!` history-expansion escaping.
- **Filesystem, `tar`/`gzip`, `find` & concurrent `xargs -P`.** Guarded
  `cp -r` against directory/symlink self-recursion and destination-symlink
  source truncation; checked source `Lstat`/`os.SameFile` before `-f`
  unlinks targets in `mv` and `ln`; restricted `mv` cross-device
  copy-delete fallback to `EXDEV` with synced atomic temp rename,
  case-only NTFS/APFS renames, and Windows read-only `-f` handling; and
  fixed `chmod` `-r`/`-w`/`-x` symbolic modes, `X`/`s`/`t`/`ugo` bits,
  and `-R` symlink skipping. Hardened `tar` against archive
  self-inclusion, symlink/hardlink Zip-Slip escapes, out-of-order
  hardlinks, symlink body writes, and `-cvf -` stdout pollution; wrapped
  corrupt `gzip`/`gunzip` streams as `exitError{1}` with atomic writes
  and prompt Windows handle closes; fixed `find` `-mindepth` gating,
  post-order `-delete`, `-L`/`-H` cycle detection, and `-exec ... {} +`
  batching; bounded `xargs -P` worker concurrency with serialized output,
  POSIX quote/`-d` parsing, and child stdin isolation; and tightened
  `readlink -f/-m`, `mktemp -u`/`TMPDIR`, `base64`,
  `sha256sum -c --strict`, `ls -R`, and `scanlines` partial-EOF reads.
- **Streaming text/`sed`/`printf`/`test` builtins & pipeline
  cancellation.** Replaced whole-input slurping with bounded streaming,
  context cancellation, and `EPIPE` handling across `sed` (1-line `$`
  lookahead across files, flattened `{...}` block compiler without global
  range-state races, `D` line-number preservation, UTF-8 `s///`/`y///`,
  and clobber-safe `-i`/`-Ei`/`-ni`), `grep`, `seq`, `tr`, `cut`, `uniq`,
  `paste`, `tail`, `hexdump`, `strings`, `od`, `fold`, and `cat`,
  alongside bounded Myers/1D-DP `diff`, UTF-8 `unexpand`, `join` unpaired
  column alignment, `tac -r`, and `sort -h`. Fixed `printf` dynamic `*`
  width/precision (`MinInt64`-safe negative width, omitted negative
  precision, and `%0.s` bare-dot parity), `test`/`[` POSIX `!`/`-a`/`-o`/
  parentheses precedence, unified `invokeExtra` builtin dispatch and
  mutex-guarded `lsColorMode` across `xargs`/`find`/`env`/`nice`/
  `timeout`, and `--`-aware flag/`--version` parsing.
- **Networking (`nc`), process tools (`ps`/`pgrep`/`pkill`) &
  cross-platform syscalls.** Fixed `nc` nil-stdin panics, TCP half-close
  response truncation (`CloseWrite` + drain), remote-EOF unblock, `-l`
  listener leaks on cancellation, and UDP (`nc -u`) client/listen modes.
  Fixed `pgrep`/`pkill` self-matching, basename vs `-f` matching, split
  signal flags (`-term`, `-stop`, `-segv`, `-sys`, `-usr1`, etc.), and OS
  process-group `pkill -g` targeting; hardened Linux `/proc/[pid]/stat`
  comm-with-parens parsing, dynamic page-size RSS scaling, `/proc/stat`
  `btime` bounds, `/proc/net/{tcp6,udp6}` IPv6 word endianness with
  bracketed `ss` `[ipv6]:port` output, and Darwin `KERN_PROCARGS2` argv
  boundaries. Fixed `env -u PATH` resurrection and case-insensitive
  Windows `Path=` deduplication, isolated `nice -n` priority changes to
  child processes, and corrected `ps -p`, `df` missing-path exit status,
  `stat` symlink `Lstat`, and `uname -i/-p` / Windows ARM64 detection.
- **CI & release infrastructure.** Ensured GNU `coreutils-from-gnu` on
  Ubuntu 25.10+ runners (where `coreutils` defaults to `uutils`), accepted
  all 12 calendar months in `TestParityLsTimeStyle`, and migrated the
  release build matrix from retired `macos-14` runners to
  `macos-15-intel` / `macos-latest` with explicit `-mod=vendor`.

## 1.0.1 — 2026-09-26

### Fixed

- **macOS `ps`/`pgrep`/`pkill` reported ppid 0 for every process.** The
  darwin process-table reader parsed `e_ppid` at a hand-rolled offset
  (296) that is actually the START of `eproc`; the real field sits much
  deeper (after `e_paddr`/`e_sess`/`e_pcred`/`e_ucred`/`e_vm`). Nothing
  surfaced it because `ps(1)`'s default columns do not show ppid and
  `pgrep`/`pkill` only need pid+comm — and the Go suite skipped `pgrep`
  on darwin entirely. The reader now uses `x/sys/unix`'s typed
  `KinfoProc` (`SysctlKinfoProcSlice`), so the layout is maintained by
  the Go team and cannot drift again.
- Added `TestListProcsParentLinkage`, which asserts the ppid column on
  every platform (the coverage that would have caught the bug).

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
- The comparison environment is now pinned: both shells run under
  `LC_ALL=C.UTF-8`, so a run no longer follows the machine's language
  (GNU sort alone answers `sort -u` and `sort -n` differently in C and
  in en_US.UTF-8 — behind every macOS differential failure). The runner
  also lost its no-op `--skip-platform` flag and env plumbing that never
  reached a child process.
- New `oracle-wording` case tag, symmetric with the existing
  `old-oracle`: where GNU generations word a diagnostic differently
  (findutils 4.10 changed two), the delta is recorded as the oracle's.
  unish pins the wording of Ubuntu LTS + Git Bash.

## 0.1.0 – 0.9.3

The build-up: embedding the command set, the differential test campaign
(the findings and their fixes are recorded in `parity/REPORT.md`), real
job control (pids, kill/jobs/wait/trap/PIPESTATUS), and the native CI
matrix. Details live in git history.
