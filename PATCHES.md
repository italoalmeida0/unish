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
| `expand/environ.go` | `FormatIntFast` zero-alloc integer string table (`0..16383`) and zero-alloc `compareFoldUpper` in `listEnviron.compare` | avoids per-iteration heap allocations in `${#v}`, `$?`, `$((...))`, `PIPESTATUS`, and Windows case-insensitive env binary search |
| `expand/param.go` | `replaceElems` understands unquoted `#`/`%` pattern anchors and bash `&` / `\&` / quoted `&` in replacements (with nil replacement words handled for `${var/pat}`); `AssocKey` expands arithmetic-tokenized associative subscripts (`m[foo-bar]`, `m[$k-port]`); case conversions support `ToggleFirst`; empty case-conversion pattern matches every char without compiling a regexp; scalar stack fast path (`[1]string`), literal/`*lit`/`lit*` pure-string `removePattern` (`#`/`##`/`%`/`%%`), and literal `replaceScalar` (`${v/sub/rep}`) | `${var/#pat/rep}`, `${var/%pat/rep}`, `${var/pat}`, `${var/pat/[&]}`, `${m[foo-bar]}`, `${var~}` parity and zero-regex-overhead parameter expansions in loops |
| `expand/arith.go` | `atoi` reports "value too great for base" for bad octal (`08`, `0o10`) instead of silently returning 0; `--5`/`++5` semantics via the lexer patch; indexed/associative array lvalues (`(( a[0] = 5 ))`, `(( a[i++] += 10 ))`, `(( m[k]++ ))`) and overwriting non-numeric scalars (`(( x = 5 ))`); fast paths for positive decimals, single-literal words (`sum`, `1`, `0x10`), and `formatArithInt64` | bash arithmetic lvalues, errors, and zero-alloc hot loops |
| `expand/expand.go` | glob paths always join with `/` and split on `/` (no `filepath.Separator`); `RunnerCallbacks` interface, `FieldsInto`, lazy `PWD` lookup only when globbing, `ConcurrentBufferWriter`-guarded pooled `cmdSubstBuffer` for `cmdSubst` (detaching when background goroutines inherit stdout), zero-alloc `prepareConfig` (`emptyEnviron`), unmatched `[` literal fast path, and single-part fast paths in `Literal` / `Pattern` / `FieldsInto` | cross-OS glob parity and race-free zero-closure/zero-slice-alloc command & word expansion |
| `expand/patcache.go` | **new file**: bounded `RWMutex` memo caches for `pattern.Regexp` + compiled remove/find regexps (`cachedRemoveRegexp`, `cachedFindRegexp`) | `${var#pat}` and `${var/pat/rep}` in loops avoid recompiling regexps |

## interp

| File | Patch | Why |
|---|---|---|
| `interp/runner.go` | `expandErr` treats expansion errors as fatal; `callHandler` errors carry non-fatal statuses and track active runners for self-signals (`QueueActiveSignalByName`); `FastCallHandlerFunc` & `fastDevNull` bypass per-call `context.WithValue`/mutex/syscall overhead; `Runner.callArgsBuf` + `expand.FieldsInto` avoid per-call `[]string` slice allocs; inline `ecfgStorage` + `RunnerCallbacks` (`ExpandCmdSubst` pooling subshell `Runner`/`overlayEnviron`); lazy `setPipeStatusSingle`/`setPipeStatus` (`PIPESTATUS`); atomic `hasActiveSigCh`/`hasIgnoredSigs` fast checks in `deliverSignals`/`resetSignals`; synchronous `<64 KB` heredoc/`<<<` writes onto in-memory pipes; redirections with fd > 2 (`exec 3>file`, `echo >&3`) scoped per statement unless `exec` and isolated/cleaned up across subshells; `ProcSubst` notifies `StmtDone` on statement completion; `declare` parses combined flag clusters (`-ir`, `-ix`) and `declare -i` / `local -i` preserves local/export/readonly attributes; signal traps (`SIG`-prefixed names, `trap ''` ignore, reset, `exit` inside `trapCallback`) delivered between and after statements; `PendingBgStarts` / `NoteBgTrackedPID` coordinate `&` job registration | bash parity, zero-alloc hot-loop dispatch/redirects/expansions, and leak/race-free fds, process substitutions, `declare`, `PIPESTATUS`, traps, and background jobs |
| `interp/vars.go` | `setVarWithIndex` and `assignVal` evaluate associative subscripts via `expand.AssocKey` (and support `+=` on associative arrays); zero-alloc Windows case-insensitive `overlayEnviron` lookup (`scanWinVarName` + cached `normKey`), `overlayEnvPool`, and lazy `materializePipeStatus` | associative array parity and zero-alloc variable lookup/assignment on Windows & Unix |
| `interp/builtin.go` | `trap` supports signal names (`INT`, `SIGTERM`, `0`), `-p`, `-l`, `trap ''` ignore vs `trap -` reset; `wait` honors context cancellation and `--`; `read` supports `-n N`, `-N N`, `-d DELIM` (attached option values too); pre-boxed `[256]error` `BuiltinExit(code)` and `CallHandled(code)` exported | bash trap, wait, and read parity plus single-pass zero-alloc `CallHandler` completion |
| `interp/api.go`, `interp/handler.go`, `interp/os_unix.go`, `interp/os_notunix.go` | extra fds (`extraFds`/`extraInFds`) cloned as non-closing views in `subshell()` and closed on `Reset()`/subshell exit; subshell `finishSubshell` runs subshell `EXIT` traps (`callbackExit`) and `resetSignals()` (`signal.Stop`), and `subshell()` preserves ignored signal names (`sigIgnored` + `sigNames`); signal trap/ignore state (`sigCallbacks`, `sigIgnored`, `sigCh`), `QueueSignal`/`TrapRegistered`; `ProcSubstFile.StmtDone` (including non-blocking FIFO drain for unconsumed POSIX `<(...)`/`>(...)`) & refcounted `procSubstRegistry`; `HandlerContext` helpers (`HasBgProc`, `HasFunc`, `LookupAlias`, `NoteBgTracked`, `PushActive`, `PopActive`); `RegisterFastCallHandler`, `RegisterFastOpenDevNull`, `WithHandlerContext`; `DefaultExecHandler` bridges virtual `/dev/null` and non-`*os.File` `StdinPipe` via `PrepareExecStdio` | subshell trap/fd isolation, process substitution cleanup, fast handler registration, and external subprocess compatibility |
| `interp/pipe_hook.go`, `interp/stdin_os.go`, `interp/stdin_js.go` | **new file** (`pipe_hook.go`) + `stdin_*` hooks: `StdinPipe`, `SetPipeFactory`, `SetMemPipeDenyFilter`, `BridgeStdinPipeToOS`, `VirtualDevNull`, `PrepareExecStdio`, `newOSPipe`, and finite-reader `readerStdin` / streaming-reader `os.Pipe` handling in `newStdinFile` | lets pipelines and redirects use pooled 64 KB in-memory pipes and zero-syscall `/dev/null` across Unix and Windows while keeping external `exec.Cmd` and child-spawning wrapper commands race/hang-free |
| `interp/signals_unix.go`, `interp/signals_windows.go` | **new files**: per-platform signal constants (`sigUSR1`, …) | Windows lacks most `syscall.SIG*` names |

Everything else is stock upstream.
