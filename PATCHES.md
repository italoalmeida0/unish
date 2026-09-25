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
| `expand/param.go` | `replaceElems` understands `#`/`%` pattern anchors and bash `&` / `\&` in replacements; case conversions support `ToggleFirst`; empty case-conversion pattern matches every char without compiling a regexp | `${var/#pat/rep}`, `${var/%pat/rep}`, `${var/pat/[&]}`, `${var~}` parity |
| `expand/arith.go` | `atoi` reports "value too great for base" for bad octal (`08`, `0o10`) instead of silently returning 0; `--5`/`++5` semantics via the lexer patch; fast path for single-literal words (`sum`, `1`, `0x10`) that skips the word-expansion machinery | bash arithmetic errors and hot loops |
| `expand/expand.go` | glob paths always join with `/` and split on `/` (no `filepath.Separator`) | glob results must look the same on every OS |
| `expand/patcache.go` | **new file**: bounded memo cache for `pattern.Regexp` + compiled regexps | `${var#pat}` in loops recompiled the regexp every call |

## interp

| File | Patch | Why |
|---|---|---|
| `interp/runner.go` | `expandErr` treats expansion errors as fatal (bash aborts a script on arithmetic/substitution errors); `callHandler` errors carry non-fatal statuses; redirections with fd > 2 (`exec 3>file`, `echo >&3`); `setPipeStatus` maintains `PIPESTATUS`; signal traps delivered between statements (`deliverSignals`); `readDelim` powers `read -n/-N/-d` | bash parity for arithmetic errors, fds, PIPESTATUS, traps and read flags |
| `interp/builtin.go` | `trap` accepts signal *names* (INT, TERM, …) via `signalByName`; `read` supports `-n N`, `-N N`, `-d DELIM` (attached option values too); `BuiltinExit(code)` helper exported | same |
| `interp/api.go` | extra fds (`extraFds`/`extraInFds`), signal trap state (`sigCallbacks`, `sigCh`), `QueueSignal`/`TrapRegistered` exported | same |
| `interp/signals_unix.go`, `interp/signals_windows.go` | **new files**: per-platform signal constants (`sigUSR1`, …) | Windows lacks most `syscall.SIG*` names |

The `stdin_*` split (os.Pipe vs io.Pipe) and everything else is stock
upstream.
