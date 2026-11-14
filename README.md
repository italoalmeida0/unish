# unish — one standalone bash, everywhere

<p align="center"><img src="assets/unish-icon-500x500.png" width="180" alt="unish logo"></p>

A single dependency-free binary that runs bash commands **the same on
Windows, Linux and macOS**. Built to be called from another program
(like an AI agent) as a regular bash:

```go
cmd := exec.Command(unishPath, "-c", "ls -la && grep -r foo . | head -20")
cmd.Dir = workdir
out, err := cmd.Output()
```

Non-interactive use is `-c`, a file, or stdin, like bash:

```
unish -c "command"        run the string (ideal for agents/AI)
unish script.sh [args...] run the file ($0=script, $1...=args)
unish < script.sh         run stdin (like bash -s)
unish --version           print version and exit
```

With no args and a terminal on stdin, unish starts an interactive
shell (or force it with `unish -i`):

```
unish                    interactive shell (TTY stdin)
unish -i                 force interactive shell (pipe-safe loop)
unish -i < script.sh     run with history + `!` expansion
```

Timeout and cancellation are the caller's job (`exec` context /
Ctrl-C).

## Go Library SDK (`pkg/unish`)

Embed `unish` directly inside any Go application or AI agent runtime without
spawning a shell process per turn or mutating host globals (`os.Chdir`,
`syscall.Umask`, `os.Environ`):

```go
package main

import (
	"context"
	"fmt"

	"github.com/italoalmeida0/unish/pkg/unish"
)

func main() {
	cfg := unish.DefaultJailConfig("/path/to/workspace")
	cfg.ReadOnlyRoots = []string{"/path/to/shared-docs"}
	cfg.OutsideAction = unish.DecisionAsk
	cfg.Rules = []unish.PathRule{
		{Pattern: "**/.env*", Decision: unish.DecisionDeny, Reason: "protect secrets"},
	}
	cfg.Net = unish.NetPolicy{
		Default:         unish.DecisionDeny,
		AllowLoopback:   true,
		BlockPrivateLAN: true, // blocks DNS rebinding to RFC1918 / link-local
		Rules: []unish.NetRule{
			{HostPattern: "api.example.com", Ports: []int{443}, Decision: unish.DecisionAllow},
		},
	}
	cfg.Exec = unish.ExecPolicy{
		Mode:          unish.ExecAllowlist, // or unish.ExecBuiltinsOnly
		AllowBinaries: []string{"git"},      // writable-root ./git spoofing is blocked
	}
	cfg.Proc = unish.ProcPolicy{Mode: unish.ProcOwnSessionOnly}
	cfg.Approver = func(ctx context.Context, req unish.AccessRequest) (unish.Decision, error) {
		// Return DecisionAllow (once), DecisionAllowSession (cached), or DecisionDeny.
		return unish.DecisionDeny, nil
	}

	jail, err := unish.NewJail(cfg)
	if err != nil {
		panic(err)
	}

	sess, err := unish.NewSession(
		unish.WithWorkDir("/path/to/workspace"),
		unish.WithJail(jail),
		unish.WithScrubEnv("*_SECRET*", "*_TOKEN", "AWS_*"),
	)
	if err != nil {
		panic(err)
	}
	defer sess.Close()

	_, _ = sess.Exec(context.Background(), "cd src && export MODE=prod")
	res, _ := sess.Exec(context.Background(), `rg -n "TODO" . | head -20`)
	fmt.Printf("exit=%d cwd=%s\n%s", res.ExitCode, res.WorkDir, res.Stdout)
}
```

Each `*unish.Session` maintains isolated working directory, shell & exported
variables, functions, aliases, `umask`, `$?`, `<(cmd)` / `>(cmd)` process
substitutions, and per-session background jobs (`&`, `jobs`, `fg`, `bg`,
`wait`, `kill %1`), so dozens of multi-tenant sessions can run concurrently in
one Go process.

## JSONL Stdio Harness Mode (`unish --harness`)

For non-Go runtimes (Python, TypeScript, Rust), `unish --harness` runs a
persistent, non-blocking JSONL RPC session over stdin/stdout (combinable with
any `--jail*` flags):

```
unish --harness --jail ./workspace --deny-path "**/.env*" --outside-action ask
```

| Direction | `type` | Key fields & behavior |
|---|---|---|
| `->` stdin | `exec` | `id`, `script`, optional `stdin`, `cwd`, `env`, `timeout_ms` — runs in persistent session |
| `<-` stdout | `stdout` / `stderr` | `id`, `data` — live output chunks streamed while `exec` is running |
| `<-` stdout | `ask_permission` | `id`, `req_id`, `op`, `path`/`host`/`binary`, `request` — emitted when policy yields `DecisionAsk` |
| `->` stdin | `permission_reply` | `req_id`, `decision` (`"allow"`, `"allow_session"`, or `"deny"`) — unblocks paused command |
| `<-` stdout | `result` | `id`, `exit_code`, `stdout`, `stderr`, `cwd`, `duration_ms`, `cancelled`, `denials` — command completion (`exec_done`) |
| `->` stdin | `cancel` | optional `req_id` — interrupts in-flight or queued `exec` (`exit_code: 130`) without losing session state |
| `->` stdin | `set_env` | `id`, `env` (`{"KEY":"VAL"}`), optional `unset` (`["KEY"]`) — updates session env and replies with `state` |
| `->` stdin | `set_cwd` / `get_state` / `revoke_grants` | Updates directory, inspects `cwd`/`env`/`umask`/`last_exit_code`, or clears cached session grants |
| `->` / `<-` | `ping` / `pong` | `id` — liveness probe (processed immediately even while `exec` awaits `permission_reply`) |

## CLI Sandbox & Jail Flags

Any one-shot (`-c`, script), interactive (`-i`), or `--harness` invocation can
be sandboxed directly from the CLI:

```
unish --jail ./workspace \
      --jail-ro ./docs \
      --deny-path "**/.env*" \
      --ask-path "./workspace/deploy/**" \
      --outside-action deny \
      --net loopback \
      --exec-mode builtins \
      -c "jq -r .name package.json && rg -l main ."
```

| Flag | Description |
|---|---|
| `--jail <dir>` | Add allowed read/write workspace root directory (repeatable; `--allow-write` also accepts globs) |
| `--jail-ro <path>` | Add allowed read-only root directory or glob (repeatable; alias `--allow-read`) |
| `--deny-path <glob>` | Explicitly deny matching path or `**` glob, even inside `--jail` roots (repeatable) |
| `--ask-path <glob>` | Prompt interactively (TTY or `--harness` `ask_permission`) on matching path/glob (repeatable) |
| `--outside-action <mode>` | Policy for paths outside allowed roots: `deny`, `ask`, or `allow` (alias `--outside`) |
| `--net <mode>` | Network policy: `deny`, `loopback`, `ask`, or `allow` |
| `--allow-net <spec>` | Allow outbound `nc` connections to `host[:port[-end]]` or `CIDR` (repeatable) |
| `--exec-mode <mode>` | External OS binary policy: `builtins` (`deny`), `allowlist`, `ask`, or `allow` |
| `--allow-exec <bin>` | Allow executing named (`git`) or canonical absolute external binary (repeatable) |

## Why

- **Zero install**: one static binary (~7 MB), no glibc/msvcrt/cygwin
  dependency. Download and run — even on minimal Alpine/musl.
- **Same behavior on every OS**: `ps`, `free`, `df`, `uname`, `stat`,
  `grep`, `sed`, `sort`, `find`, `tar`, hashes… all embedded in pure Go.
  No line-length limits anywhere (GNU parity: a 1.3 MB minified line
  in the VSCode repo is matched, not silently skipped).
- **Fast on Windows**: 3–18x faster than Git Bash on loops, pipes and
  command substitution (no fork emulation); ~8 ms startup.
- **Script-friendly**: GNU-style attached flags (`cut -d: -f2`,
  `sort -rn`, `head -2`), `ls --color=auto` accepted, `/`-style paths
  from `grep -r` even on Windows.

## Performance

Because the utilities are embedded (no fork/exec, no external
dependencies), unish is fast exactly where shells usually hurt. Measured
with `bench/` (same workloads, medians over repeated runs; GNU tools in
the bash column are the external C binaries):

**Windows** (vs Git Bash and MSYS2): faster in **every** workload.
`$(cmd)` in a loop is ~2300x faster (20s → 0.009s), the text tools are
7–12x, pipelines ~5x, `sort` ~2.6x, shell startup ~5x.

**Linux** (vs bash 5.2 + coreutils 9.4): command substitution **20x**,
`cat` **4x**, arithmetic loops ~2x, `seq` ~2x, plain `sort` and
`sort -u` ahead, `sed`/`wc`/`head` at parity. The remaining narrow
losses are documented in `bench/README.md` (tar over thousands of tiny
files, `echo` with thousands of arguments, `grep` which is mostly
process startup and file I/O).

Numbers vary between machines (WSL2 in particular quantizes timers
below ~10ms); `bench/README.md` explains how to reproduce and how to
read the results.

## Embedded commands (work on any OS)

**Shell builtins** (~40, via mvdan/sh): `cd`, `echo`, `printf`
(full `%g`/`%f`/`%e` precision specs), `test`/`[`, `export`, `pwd`,
`true`, `false`, `exit`, `read`, `eval`, `source`, `trap`, `alias`,
`pushd`/`popd`, `getopts`, `shopt`, …

**Coreutils** (17, via mvdan/sh/x): `ls`, `cat`, `cp`, `mv`, `rm`,
`mkdir`, `touch`, `chmod`, `find`, `mktemp`, `xargs`, `gzip`/`gunzip`,
`tar`, `base64`, `shasum`.

**Own extras** (~100, pure stdlib, no cgo):

| Group | Commands |
|---|---|
| text | `grep` (-E BRE/ERE, -r, -o, -a/--text, -q/-s/-h/-H/-m/-l/-c/-n/-i/-v/-x/-w/-F, -A/-B/-C, --include/--exclude), `sed` (s/// with `w` flag, addresses, ranges, hold space, `y`, `a`/`i`/`c`, `=`, `N`, -n/-e/-f/-i/-E), `head` (-n, -c, -q/-v), `tail` (-n `+N`, -c, -q/-v), `sort` (-n, -r, -u, -c, -k, -t, -f, -s, -V, -o, -z), `uniq` (-c/-d/-u/-i/-f/-s/-w, --group), `wc` (-l/-w/-c/-m/-L), `tee`, `tr` (-d/-s/-c/-t, classes), `seq` (-s, -w, -f), `cut` (-d/-f/-c/-b, --complement, --output-delimiter), `paste` (-d, -s, `- -`), `comm` (-1/-2/-3, --check-order), `split` (-l/-b/-a/-d, --suffix-length, -n chunks), `diff` (-q, -u, -s/--brief), `cmp` (-s, -l), `hexdump` (-C, -n, -s, -e format), `strings` (-n, -t, -a), `od` (-An, -t, -A, -j, -N), `nl` (-ba, -v), `tac`, `rev`, `fold` (-w, -s, -b), `expand` (-t, -i), `unexpand` (-a), `join` (-1/-2/-j/-t/-e/-a/-o, -v), `cat` (-n/-b/-s/-E/-T, unbounded lines) |
| automation | `awk`/`gawk` (POSIX + GNU gawk: `BEGIN`/`END`, regex/ranges, arrays, functions, `printf`/`sprintf`, `sub`/`gsub`/`gensub`, `getline`, pipes, `system()`), `jq` (1.7 filters, exact 64-bit `int64`, `=`/`|=`, `del`, `--arg`/`--argjson`, `-r`/`-c`/`-s`/`-R`/`-e`, `@base64`/`@uri`/`@csv`/`@tsv`/`@sh`), `rg` (`.gitignore`-aware recursive search, `-F`/`-i`/`-S`/`-w`/`-g`/`-t`/`-C`/`--files`/`-r`), `patch` (`-pN`, create/delete, fuzzy hunks, `-R`, `--dry-run`), `bc` (`-l` mathlib, arbitrary precision, `ibase`/`obase`, `define`), `sponge` (`-a`), `flock` (`-s`/`-x`/`-n`/`-w`/`-E`, file & FD locks), `column` (`-t`/`-s`/`-o`/`-n`, UTF-8/CJK width), `xxd` (`-p`/`-r`/`-i`/`-b`/`-s`/`-l`/`-c`/`-g`) |
| files | `ls` (-a with `.`/`..`, -l, -R, -S, -t, -m, -C, -x, -i, -F, -Q, -d, …), `find` (-name/-iname, -path, -type, -maxdepth/-mindepth, -size, -mtime/-mmin, -newer, -empty, -perm, -not, -prune, -exec, -delete, -print0), `mktemp`, `cp` (-r, -v, -n, -p), `mv` (-v `renamed …`, -n), `rm` (never removes `.`/`..`, --preserve-root), `mkdir`, `touch` (-d, -r/--reference, -c), `chmod` (symbolic), `xargs` (-0, -n, -I, -r), `base64` (-d, -w, -i), `tar` (-cf/-xf/-tf/-czf, --exclude, slip defense), `gzip`/`gunzip`/`gzcat`, `dirname`, `basename`, `realpath`, `readlink`, `ln` (-s, -f), `du` (-s, -h, -b) |
| system | `sleep`, `timeout` (-s, -k, --preserve-status), `kill` (-s/-n, `%jobspec`, `-0` existence check), `pwd`, `true`, `false`, `yes`, `which`, `printenv`, `whoami`, `nproc`, `clear`, `echo`, `date` (+strftime, -r, -d), `uname` (-s/-n/-r/-v/-m/-p/-i/-a, real kernel + machine), `hostname`, `md5sum`, `sha1sum`, `sha256sum`, `shasum` (-a, -c, -q/-s) |
| sysinfo | `df` (-h, -k, -P), `ps` (aux/-ef/-o columns), `free` (-h/-m/-g), `uptime`, `env` (-i), `stat` (full + -c), `ss` (-t/-u/-l/-a/-n/-p), `id`, `arch`, `tty`, `logname`, `sync`, `nohup`, `nice` |
| shell | `umask`, `ulimit` (-n/-a, -Sn/-Hn), `hash` (real table), `type`, `jobs` (real PIDs), `fg`, `bg`, `wait`, `history` (interactive), `compgen` |
| network | `nc`/`netcat` (-l, -u, -z, -w, -p, -v), `pgrep` (-f/-x/-i/-l/-c), `pkill` (-f/-x/-i/-e/-s/-l/-n/-a/-g) |

## Interactive shell

No args on a terminal (or `unish -i`) starts a readline shell:

- **Emacs editing**: arrows/Home/End/Delete, `^A/^E/^B/^F/^K/^U/^W`,
  `Alt-B/F/D`, `^T`, `^L`, UTF-8 + CJK wide chars, `\e` colors in PS1
- **Kill-ring + undo + paste**: `^K/^U/^W/Alt-D` kill to the ring,
  `^Y` yanks, `Alt-Y` cycles, `^_` undoes; bracketed paste inserts
  literally (multiline included, no mid-paste submit)
- **History**: `~/.unish_history` (HISTFILE/HISTSIZE/HISTFILESIZE),
  HISTCONTROL, `history` builtin (`-c/-d/-s/-p/-a/-n/-r/-w`),
  `!`/`!!`/`!n`/`!-n`/`!str`/`!?str?` expansion, `Ctrl-R` search
- **Completion**: `Tab` completes commands (builtins + PATH + `hash`),
  `$vars`, files (dirs get `/`); second `Tab` lists
- **Prompt**: PS1/PS2 with `\u \h \w \W \$ \# \!` + `$vars` +
  `$(cmds)` + `\[...\]` colors, PROMPT_COMMAND, `~/.unishrc`

Anything else (`git`, `curl`, `ssh`, `python`, …) falls through
to the system binary.

## Compatibility notes

- `FOO=42 printenv FOO` works (reads the shell environment).
- **Job control works**: `$!` is a real pid, so `kill $!`, `kill %1`,
  `jobs`, `wait` and `trap ... TERM` behave like bash. Background
  statements run as real child processes.
- `/dev/stdin` works as an operand (`-`, `/dev/stdin`, repeated) even
  on Windows, where the OS has no such file (`/dev/null` is mapped to
  `NUL`).
- `ps`, `pgrep` and `pkill` read the real process table on every OS
  (`/proc` on Linux, `kern.proc.all` on macOS, `CreateToolhelp32Snapshot`
  on Windows).
- Ctrl-C / timeout: exit 130 / 124 (bash/timeout convention).
- Syntax errors: exit 2, like bash.

## Install

Download the binary for your OS/arch from
[Releases](https://github.com/italoalmeida0/unish/releases) and run it.
No installer, no dependencies:

| File | OS | Arch |
|---|---|---|
| `unish-windows-amd64.exe` | Windows | x86_64 |
| `unish-windows-arm64.exe` | Windows | ARM64 |
| `unish-linux-amd64` | Linux | x86_64 |
| `unish-linux-arm64` | Linux | ARM64 |
| `unish-macos-amd64` | macOS | x86_64 (Intel) |
| `unish-macos-arm64` | macOS | Apple Silicon |

Or with Go (1.26+):

```
go install github.com/italoalmeida0/unish@latest
```

## Build from source

```
go build -trimpath -ldflags="-s -w" -o unish .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o unish-linux-arm64 .
```

Pure Go, `CGO_ENABLED=0`: the Linux binaries are fully static
(no glibc/musl needed), the Windows ones only link system DLLs.

## Development

```
go vet ./...
go test ./...        # unit + GNU parity regression oracles
```

The differential suite in `parity/` runs ~1100 cases under both unish and
a real GNU bash and requires identical stdout, exit codes and file
effects. It covers every embedded command, every declared flag, the shell
builtins, edge cases and end-to-end process behaviour, and it runs in CI
on every push (and weekly). See `parity/README.md` and `parity/REPORT.md`
(the latter records what the campaign found).

The oracle is GNU on every platform — bash + coreutils on Linux, Git Bash
on Windows, Homebrew's GNU tools + bash on macOS (the system bash there
is 3.2 over BSD tools) and GNU tools asserted on Alpine — so a pass means
"behaves like GNU", not "looks like whatever the host ships".

CI runs the full suite on **six native targets** (linux, windows and
macOS, each amd64 + arm64), plus:

- a **musl** job that proves the Linux binaries are glibc-free: they are
  executed on a bare Alpine image and checked with `file(1)` for
  `statically linked`, and the whole suite runs on a musl userland
- a **docker sandbox** job for the process/network tools (`pgrep`,
  `pkill`, `ss`, `nc`), which cannot be tested safely against the runner

The release workflow repeats the full differential suite on Linux,
Windows and macOS before publishing. Release history: [CHANGELOG.md](CHANGELOG.md).

Benchmarks live in `bench/`; see `bench/README.md`.

## License

MIT — see [LICENSE](LICENSE).
