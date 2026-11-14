# Vendored mvdan.cc/sh patches

`vendor/mvdan.cc/sh/v3` is a snapshot of [mvdan/sh](https://github.com/mvdan/sh)
(v3.14.1-0.20260909211640) with local patches. Everything below is a
behavior change on top of upstream; when re-vendoring (`go mod vendor`)
or bumping the module, re-apply and re-run `parity/runner.py` (the
differential suite catches any regression) plus `go test ./...`.

## syntax

| File | Patch | Why |
|---|---|---|
| `syntax/lexer.go` | `paramToken` accepts `~`; `arithmToken` treats `--`/`++` before a digit as two unary operators | `${var~}` (bash case toggle) parses; `$(())` `--5` is -(-5) like bash, not a decrement |
| `syntax/parser.go` | `~` accepted as a case-conversion operator (`caret,dblCaret,comma,dblComma,tilde`) | `${var~}` support |
| `syntax/tokens.go` | `ToggleFirst = ParExpOperator(tilde)` | operator constant for the above |

## expand

| File | Patch | Why |
|---|---|---|
| `expand/param.go` | `replaceElems` understands unquoted `#`/`%` pattern anchors and bash `&` / `\&` / quoted `&` in replacements (with nil replacement words handled for `${var/pat}`); `AssocKey` expands arithmetic-tokenized associative subscripts (`m[foo-bar]`, `m[$k-port]`); case conversions support `ToggleFirst`; empty case-conversion pattern matches every char without compiling a regexp | `${var/#pat/rep}`, `${var/%pat/rep}`, `${var/pat}`, `${var/pat/[&]}`, `${m[foo-bar]}`, `${var~}` parity |
| `expand/arith.go` | `atoi` reports "value too great for base" for bad octal (`08`, `0o10`) instead of silently returning 0; `--5`/`++5` semantics via the lexer patch; indexed/associative array lvalues (`(( a[0] = 5 ))`, `(( a[i++] += 10 ))`, `(( m[k]++ ))`) and overwriting non-numeric scalars (`(( x = 5 ))`); fast path for single-literal words (`sum`, `1`, `0x10`) that skips the word-expansion machinery | bash arithmetic lvalues, errors, and hot loops |
| `expand/expand.go` | glob paths always join with `/` and split on `/` (no `filepath.Separator`) | glob results must look the same on every OS |
| `expand/patcache.go` | **new file**: bounded memo cache for `pattern.Regexp` + compiled regexps | `${var#pat}` in loops recompiled the regexp every call |

## interp

| File | Patch | Why |
|---|---|---|
| `interp/runner.go` | `expandErr` treats expansion errors as fatal; `callHandler` errors carry non-fatal statuses and track active runners for self-signals (`QueueActiveSignalByName`); redirections with fd > 2 (`exec 3>file`, `echo >&3`) scoped per statement unless `exec` and isolated/cleaned up across subshells; `ProcSubst` notifies `StmtDone` on statement completion; `declare` parses combined flag clusters (`-ir`, `-ix`) and `declare -i` / `local -i` preserves local/export/readonly attributes; `setPipeStatus` maintains `PIPESTATUS`; signal traps (`SIG`-prefixed names, `trap ''` ignore, reset, `exit` inside `trapCallback`) delivered between and after statements (`deliverSignals`); `PendingBgStarts` / `NoteBgTrackedPID` coordinate `&` job registration | bash parity and leak/race-free fds, process substitutions, `declare`, `PIPESTATUS`, traps, and background jobs |
| `interp/vars.go` | `setVarWithIndex` and `assignVal` evaluate associative subscripts via `expand.AssocKey` (and support `+=` on associative arrays) | `m[foo-bar]=1` and `declare -A m=([one+two]=3)` without panics or dropped assignments |
| `interp/builtin.go` | `trap` supports signal names (`INT`, `SIGTERM`, `0`), `-p`, `-l`, `trap ''` ignore vs `trap -` reset; `wait` honors context cancellation and `--`; `read` supports `-n N`, `-N N`, `-d DELIM` (attached option values too); `BuiltinExit(code)` helper exported | bash trap, wait, and read parity |
| `interp/api.go`, `interp/handler.go`, `interp/os_unix.go`, `interp/os_notunix.go` | extra fds (`extraFds`/`extraInFds`) cloned as non-closing views in `subshell()` and closed on `Reset()`/subshell exit; subshell `finishSubshell` runs subshell `EXIT` traps (`callbackExit`) and `resetSignals()` (`signal.Stop`), and `subshell()` preserves ignored signal names (`sigIgnored` + `sigNames`); signal trap/ignore state (`sigCallbacks`, `sigIgnored`, `sigCh`), `QueueSignal`/`TrapRegistered`; `ProcSubstFile.StmtDone` (including non-blocking FIFO drain for unconsumed POSIX `<(...)`/`>(...)`) & refcounted `procSubstRegistry`; `HandlerContext` helpers (`HasBgProc`, `HasFunc`, `LookupAlias`, `NoteBgTracked`) | subshell trap/fd isolation, process substitution cleanup, and builtin/job integration |
| `interp/signals_unix.go`, `interp/signals_windows.go` | **new files**: per-platform signal constants (`sigUSR1`, …) | Windows lacks most `syscall.SIG*` names |

The `stdin_*` split (os.Pipe vs io.Pipe) and everything else is stock
upstream.
