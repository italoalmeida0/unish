# unish — one standalone bash, everywhere

A single dependency-free binary that runs bash commands **the same on
Windows, Linux and macOS**. Built to be called from another program
(like an AI agent) as a regular bash:

```go
cmd := exec.Command(unishPath, "-c", "ls -la && grep -r foo . | head -20")
cmd.Dir = workdir
out, err := cmd.Output()
```

There is only the `-c` flag, but it also accepts a file and stdin
like bash:

```
unish -c "command"        run the string (ideal for agents/AI)
unish script.sh [args...] run the file ($0=script, $1...=args)
unish < script.sh         run stdin (like bash -s)
```

No interactive mode, no readline. Timeout and cancellation are the
caller's job (`exec` context / Ctrl-C).

## Why

- **Zero install**: one static binary (~4 MB), no glibc/msvcrt/cygwin
  dependency. Download and run — even on minimal Alpine/musl.
- **Same behavior on every OS**: `ps`, `free`, `df`, `uname`, `stat`,
  `grep`, `sed`, `sort`, `find`, `tar`, hashes… all embedded in pure Go.
- **Fast on Windows**: 3–18x faster than Git Bash on loops, pipes and
  command substitution (no fork emulation); ~8 ms startup.
- **Script-friendly**: GNU-style attached flags (`cut -d: -f2`,
  `sort -rn`, `head -2`), `ls --color=auto` accepted, `/`-style paths
  from `grep -r` even on Windows.

## Embedded commands (work on any OS)

**Shell builtins** (~40, via mvdan/sh): `cd`, `echo`, `printf`,
`test`/`[`, `export`, `pwd`, `true`, `false`, `exit`, `read`, `eval`,
`source`, `trap`, `alias`, `pushd`/`popd`, `getopts`, `shopt`, …

**Coreutils** (17, via mvdan/sh/x): `ls`, `cat`, `cp`, `mv`, `rm`,
`mkdir`, `touch`, `chmod`, `find`, `mktemp`, `xargs`, `gzip`/`gunzip`,
`tar`, `base64`, `shasum`.

**Own extras** (~60, pure stdlib, no cgo):

| Group | Commands |
|---|---|
| text | `grep` (-E BRE/ERE, -r, -o, -A/-B/-C, --include/--exclude), `sed` (s///, addresses, ranges, hold space, -n/-e/-f/-i/-E), `head`, `tail`, `sort` (-n, -r, -u, -c, -k), `uniq`, `wc`, `tee`, `tr`, `seq`, `cut`, `paste`, `comm`, `split`, `diff` (-q, -u), `cmp`, `hexdump`, `strings`, `od`, `nl`, `tac`, `rev`, `fold`, `expand`, `unexpand`, `join`, `cat` (-n/-b/-s/-E/-T) |
| files | `ls` (-a with `.`/`..`, -l, -R, -S, -t, …), `find` (-name, -type, -maxdepth), `mktemp`, `cp`, `mv`, `rm`, `mkdir`, `touch`, `chmod`, `xargs`, `base64`, `tar`, `gzip`/`gunzip`/`gzcat`, `dirname`, `basename`, `realpath`, `readlink`, `ln`, `du` |
| system | `sleep`, `timeout` (-s, -k, --preserve-status), `kill`, `pwd`, `true`, `false`, `yes`, `which`, `printenv`, `whoami`, `nproc`, `clear`, `echo`, `date` (+strftime), `uname`, `hostname`, `md5sum`, `sha1sum`, `sha256sum`, `shasum` |
| sysinfo | `df`, `ps` (aux/-ef), `free`, `uptime`, `env`, `stat` (-c), `ss` (-t/-u/-l), `id`, `arch`, `tty`, `logname`, `sync`, `nohup`, `nice` |

Anything else (`git`, `curl`, `ssh`, `awk`, `python`, …) falls through
to the system binary.

## Compatibility notes

- `FOO=42 printenv FOO` works (reads the shell environment).
- `kill` works (declared but unimplemented upstream; intercepted).
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

Or with Go (1.24+):

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

## License

MIT — see [LICENSE](LICENSE).
