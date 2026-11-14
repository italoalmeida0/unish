package unish

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func cmdDirname(_ context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) < 2 {
		return flag.ErrHelp
	}
	for _, p := range args[1:] {
		// GNU dirname preserves a leading "./" prefix; Go's
		// filepath.Dir cleans it away (Dir("./a/b") == "a").
		prefix := ""
		q := p
		if strings.HasPrefix(q, "./") {
			prefix = "./"
			q = q[2:]
		}
		// GNU strips ALL trailing slashes before taking the directory.
		q = strings.TrimRight(q, "/")
		if q == "" {
			if p != "" && strings.Trim(p, "/") == "" {
				fmt.Fprintln(hc.Stdout, "/")
			} else {
				fmt.Fprintln(hc.Stdout, ".")
			}
			continue
		}
		d := filepath.ToSlash(filepath.Dir(q))
		if prefix != "" && d != "." && d != "/" {
			d = prefix + strings.TrimPrefix(d, "/")
		}
		fmt.Fprintln(hc.Stdout, d)
	}
	return nil
}

func cmdBasename(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("basename", hc.Stderr)
	multi := fs.Bool("a", false, "")
	suffix := fs.String("s", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		return flag.ErrHelp
	}
	if !*multi && *suffix == "" && len(paths) >= 2 &&
		!strings.ContainsAny(paths[len(paths)-1], `/\`) {
		*suffix = paths[len(paths)-1]
		paths = paths[:len(paths)-1]
	}
	for _, p := range paths {
		base := gnuBasename(p)
		out := base
		// GNU only strips SUFFIX when it is strictly shorter than the
		// base name (basename c c -> "c").
		if *suffix != "" && len(*suffix) < len(out) && strings.HasSuffix(out, *suffix) {
			out = strings.TrimSuffix(out, *suffix)
		}
		fmt.Fprintln(hc.Stdout, out)
	}
	return nil
}

// gnuBasename implements the POSIX basename(1) path trimming: trailing
// slashes go away ("a/b///" -> "b"), all-slash names become "/", and an
// empty name stays empty.
func gnuBasename(p string) string {
	if p == "" {
		return ""
	}
	trimmed := strings.TrimRight(p, "/")
	if trimmed == "" {
		return "/"
	}
	if i := strings.LastIndexAny(trimmed, `/\`); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	return trimmed
}

const (
	canonAllExist = iota
	canonAllButLast
	canonMissingOK
)

func evalSymlinksChecked(ctx context.Context, cmdName, abs string) (string, error) {
	if CapabilityHookFromContext(ctx) == nil {
		return filepath.EvalSymlinks(abs)
	}
	if _, err := checkFSNoFollowLeaf(ctx, "", cmdName, abs, OpStat); err != nil {
		return "", err
	}
	p, err := checkFS(ctx, "", cmdName, abs, OpStat)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(p); err != nil {
		return "", err
	}
	return p, nil
}

func canonicalizePath(ctx context.Context, dir, cmdName, rawPath string, mode int) (string, error) {
	if _, err := checkFSNoFollowLeaf(ctx, dir, cmdName, rawPath, OpStat); err != nil {
		return "", err
	}
	if _, err := checkFS(ctx, dir, cmdName, rawPath, OpStat); err != nil {
		return "", err
	}
	abs := absFromDir(dir, rawPath)
	if rp, err := evalSymlinksChecked(ctx, cmdName, abs); err == nil {
		return rp, nil
	} else if errors.Is(err, os.ErrPermission) {
		return "", err
	}
	if mode == canonAllExist {
		if _, err := fsStat(ctx, "", cmdName, abs); err != nil {
			return "", err
		}
		return evalSymlinksChecked(ctx, cmdName, abs)
	}
	if mode == canonAllButLast {
		cur := abs
		for hops := 0; hops < 40; hops++ {
			lfi, lerr := fsLstat(ctx, "", cmdName, cur)
			if lerr != nil {
				if !os.IsNotExist(lerr) {
					return "", lerr
				}
				parent := filepath.Dir(cur)
				if parent == cur {
					return "", lerr
				}
				rpParent, perr := evalSymlinksChecked(ctx, cmdName, parent)
				if perr != nil {
					return "", perr
				}
				pfi, serr := fsStat(ctx, "", cmdName, rpParent)
				if serr != nil {
					if errors.Is(serr, os.ErrPermission) {
						return "", serr
					}
					return "", os.ErrNotExist
				}
				if !pfi.IsDir() {
					return "", os.ErrNotExist
				}
				res := filepath.Join(rpParent, filepath.Base(cur))
				if _, err := checkFS(ctx, "", cmdName, res, OpStat); err != nil {
					return "", err
				}
				return res, nil
			}
			if lfi.Mode()&os.ModeSymlink == 0 {
				return evalSymlinksChecked(ctx, cmdName, cur)
			}
			target, rerr := fsReadlink(ctx, "", cmdName, cur)
			if rerr != nil {
				return "", rerr
			}
			if filepath.IsAbs(target) {
				cur = filepath.Clean(target)
			} else {
				cur = filepath.Clean(filepath.Join(filepath.Dir(cur), target))
			}
			if _, err := checkFSNoFollowLeaf(ctx, "", cmdName, cur, OpStat); err != nil {
				return "", err
			}
			if rp, err := evalSymlinksChecked(ctx, cmdName, cur); err == nil {
				return rp, nil
			} else if errors.Is(err, os.ErrPermission) {
				return "", err
			}
		}
		return "", fmt.Errorf("too many levels of symbolic links")
	}

	// canonMissingOK: resolve symlinks on the deepest existing prefix, then append cleaned tail.
	cur := abs
	var tail []string
	for {
		if rp, err := evalSymlinksChecked(ctx, cmdName, cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				rp = filepath.Join(rp, tail[i])
			}
			res := filepath.Clean(rp)
			if _, err := checkFS(ctx, "", cmdName, res, OpStat); err != nil {
				return "", err
			}
			return res, nil
		} else if errors.Is(err, os.ErrPermission) {
			return "", err
		}
		// If cur itself is a dangling symlink, expand it and restart.
		if lfi, lerr := fsLstat(ctx, "", cmdName, cur); lerr == nil && lfi.Mode()&os.ModeSymlink != 0 {
			if link, rerr := fsReadlink(ctx, "", cmdName, cur); rerr == nil {
				var next string
				if filepath.IsAbs(link) {
					next = link
				} else {
					next = filepath.Join(filepath.Dir(cur), link)
				}
				for i := len(tail) - 1; i >= 0; i-- {
					next = filepath.Join(next, tail[i])
				}
				// Guard against symlink cycles in -m mode by falling back lexically after one pass.
				next = filepath.Clean(next)
				if _, err := checkFS(ctx, "", cmdName, next, OpStat); err != nil {
					return "", err
				}
				if rp, err := evalSymlinksChecked(ctx, cmdName, filepath.Dir(next)); err == nil {
					res := filepath.Join(rp, filepath.Base(next))
					if _, err := checkFS(ctx, "", cmdName, res, OpStat); err != nil {
						return "", err
					}
					return res, nil
				} else if errors.Is(err, os.ErrPermission) {
					return "", err
				}
				return next, nil
			}
		} else if errors.Is(lerr, os.ErrPermission) {
			return "", lerr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func cmdRealpath(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("realpath", hc.Stderr)
	missing := fs.Bool("m", false, "")
	fs.BoolVar(missing, "canonicalize-missing", false, "")
	exists := fs.Bool("e", false, "")
	fs.BoolVar(exists, "canonicalize-existing", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		return flag.ErrHelp
	}
	mode := canonMissingOK
	if *exists && !*missing {
		mode = canonAllExist
	}
	code := 0
	for _, p := range fs.Args() {
		rp, err := canonicalizePath(ctx, hc.Dir, "realpath", p, mode)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "realpath:", err)
			code = 1
			continue
		}
		fmt.Fprintln(hc.Stdout, rp)
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdReadlink(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("readlink", hc.Stderr)
	canonical := fs.Bool("f", false, "")
	fs.BoolVar(canonical, "canonicalize", false, "")
	missing := fs.Bool("m", false, "")
	fs.BoolVar(missing, "canonicalize-missing", false, "")
	exists := fs.Bool("e", false, "")
	fs.BoolVar(exists, "canonicalize-existing", false, "")
	noNewline := fs.Bool("n", false, "")
	fs.BoolVar(noNewline, "no-newline", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		return flag.ErrHelp
	}
	code := 0
	for _, p := range fs.Args() {
		var dst string
		if *canonical || *missing || *exists {
			mode := canonAllButLast
			if *missing {
				mode = canonMissingOK
			} else if *exists {
				mode = canonAllExist
			}
			rp, err := canonicalizePath(ctx, hc.Dir, "readlink", p, mode)
			if err != nil {
				fmt.Fprintln(hc.Stderr, "readlink:", err)
				code = 1
				continue
			}
			dst = rp
		} else {
			var err error
			dst, err = fsReadlink(ctx, hc.Dir, "readlink", p)
			if err != nil {
				if CapabilityHookFromContext(ctx) != nil && errors.Is(err, os.ErrPermission) {
					fmt.Fprintln(hc.Stderr, "readlink:", err)
				}
				// GNU readlink on a non-symlink fails silently (exit 1).
				code = 1
				continue
			}
		}
		if *noNewline && len(fs.Args()) == 1 {
			fmt.Fprint(hc.Stdout, dst)
		} else {
			fmt.Fprintln(hc.Stdout, dst)
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func precheckLnCapability(ctx context.Context, dir, src, targetSpec string, sym bool) error {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		return nil
	}
	newPath, err := checkFSNoFollowLeaf(ctx, dir, "ln", targetSpec, OpWrite)
	if err != nil {
		return err
	}
	var linkBase string
	if sym {
		linkBase = filepath.Dir(newPath)
	} else {
		linkBase = dir
	}
	resolved := resolveHostPath(linkBase, src)
	realPath, lexPath := canonicalizeForCheck(resolved)
	if err := hook.CheckAccess(ctx, AccessRequest{
		Op:         OpLink,
		Command:    "ln",
		Path:       realPath,
		RawPath:    src,
		TargetPath: newPath,
	}); err != nil {
		return err
	}
	checkLex := canonicalLexicalInDir(linkBase, src, lexPath)
	if !sameCleanPath(checkLex, realPath) {
		if err := hook.CheckAccess(ctx, AccessRequest{
			Op:         OpLink,
			Command:    "ln",
			Path:       checkLex,
			RawPath:    src,
			TargetPath: newPath,
		}); err != nil {
			return err
		}
	}
	return nil
}

func cmdLn(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("ln", hc.Stderr)
	sym := fs.Bool("s", false, "")
	fs.BoolVar(sym, "symbolic", false, "")
	force := fs.Bool("f", false, "")
	fs.BoolVar(force, "force", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "ln: missing destination")
		return flag.ErrHelp
	}
	dstArg := rest[len(rest)-1]
	dst := resolveHostPath(hc.Dir, dstArg)
	dstFi, dstErr := fsStat(ctx, hc.Dir, "ln", dstArg)
	dstIsDir := dstErr == nil && dstFi.IsDir()
	if len(rest) > 2 && !dstIsDir {
		fmt.Fprintf(hc.Stderr, "ln: target '%s' is not a directory\n", dstArg)
		return exitError{1}
	}
	code := 0
	for _, src := range rest[:len(rest)-1] {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		target := dst
		targetSpec := dstArg
		if dstIsDir {
			base := filepath.Base(src)
			target = filepath.Join(dst, base)
			targetSpec = filepath.Join(dstArg, base)
		}
		if err := precheckLnCapability(ctx, hc.Dir, src, targetSpec, *sym); err != nil {
			fmt.Fprintln(hc.Stderr, "ln:", err)
			code = 1
			continue
		}
		if !*sym {
			sfi, serr := fsLstat(ctx, hc.Dir, "ln", src)
			if serr != nil {
				if os.IsNotExist(serr) {
					fmt.Fprintf(hc.Stderr, "ln: failed to access '%s': No such file or directory\n", src)
				} else {
					fmt.Fprintf(hc.Stderr, "ln: failed to access '%s': %v\n", src, serr)
				}
				code = 1
				continue
			}
			if tfi, terr := fsLstat(ctx, hc.Dir, "ln", targetSpec); terr == nil && os.SameFile(sfi, tfi) {
				fmt.Fprintf(hc.Stderr, "ln: '%s' and '%s' are the same file\n", src, target)
				code = 1
				continue
			}
		} else {
			if tfi, terr := fsLstat(ctx, hc.Dir, "ln", targetSpec); terr == nil {
				linkDest := resolveHostPath(filepath.Dir(target), src)
				if sfi, serr := fsLstat(ctx, "", "ln", linkDest); serr == nil && os.SameFile(sfi, tfi) {
					fmt.Fprintf(hc.Stderr, "ln: '%s' and '%s' are the same file\n", src, target)
					code = 1
					continue
				}
			}
		}
		if *force {
			if runtime.GOOS == "windows" {
				if tfi, terr := fsLstat(ctx, hc.Dir, "ln", targetSpec); terr == nil && tfi.Mode()&os.ModeSymlink == 0 {
					_ = fsChmod(ctx, hc.Dir, "ln", targetSpec, 0o666)
				}
			}
			if rerr := fsRemove(ctx, hc.Dir, "ln", targetSpec); rerr != nil && !os.IsNotExist(rerr) && errors.Is(rerr, os.ErrPermission) {
				fmt.Fprintln(hc.Stderr, "ln:", rerr)
				code = 1
				continue
			}
		}
		var err error
		if *sym {
			err = fsSymlink(ctx, hc.Dir, "ln", src, targetSpec)
		} else {
			err = fsLink(ctx, hc.Dir, "ln", src, targetSpec)
		}
		if err != nil {
			// GNU wording: "ln: failed to create symbolic link 'DST': File exists".
			if os.IsExist(err) {
				kind := "hard link"
				if *sym {
					kind = "symbolic link"
				}
				fmt.Fprintf(hc.Stderr, "ln: failed to create %s '%s': File exists\n", kind, dstArg)
			} else {
				fmt.Fprintln(hc.Stderr, "ln:", err)
			}
			code = 1
			continue
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdDu(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("du", hc.Stderr)
	summary := fs.Bool("s", false, "")
	human := fs.Bool("h", false, "")
	apparent := fs.Bool("b", false, "")
	fs.BoolVar(apparent, "bytes", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	hookActive := CapabilityHookFromContext(ctx) != nil
	code := 0
	for _, p := range paths {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fi, err := fsLstat(ctx, hc.Dir, "du", p)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(hc.Stderr, "du: cannot access '%s': No such file or directory\n", p)
			} else {
				fmt.Fprintf(hc.Stderr, "du: cannot access '%s': %v\n", p, err)
			}
			code = 1
			continue
		}
		// GNU du without -L does not follow a symlink operand: it
		// reports 0 blocks for the link itself.
		if fi.Mode()&os.ModeSymlink != 0 {
			if *human {
				fmt.Fprintf(hc.Stdout, "%s\t%s\n", humanDu(0), p)
			} else {
				fmt.Fprintf(hc.Stdout, "0\t%s\n", p)
			}
			continue
		}
		// GNU du reports disk usage (blocks), minimum 4K per file/dir
		// entry on most filesystems; unish approximates with apparent
		// sizes rounded up to 4K blocks for parity on small trees.
		var total int64
		var dirs int64
		var walkDu func(rawCur string, info os.FileInfo)
		walkDu = func(rawCur string, info os.FileInfo) {
			if ctx.Err() != nil {
				return
			}
			if !info.IsDir() {
				total += info.Size()
				return
			}
			dirs++
			entries, rerr := fsReadDir(ctx, hc.Dir, "du", rawCur)
			if rerr != nil {
				fmt.Fprintf(hc.Stderr, "du: cannot read directory '%s': %v\n", rawCur, rerr)
				code = 1
				return
			}
			for _, e := range entries {
				if ctx.Err() != nil {
					return
				}
				childRaw := filepath.Join(rawCur, e.Name())
				var cfi os.FileInfo
				var cerr error
				if hookActive {
					cfi, cerr = fsLstat(ctx, hc.Dir, "du", childRaw)
				} else {
					cfi, cerr = e.Info()
				}
				if cerr != nil {
					if errors.Is(cerr, os.ErrPermission) {
						fmt.Fprintf(hc.Stderr, "du: cannot access '%s': %v\n", childRaw, cerr)
						code = 1
					}
					continue
				}
				walkDu(childRaw, cfi)
			}
		}
		walkDu(p, fi)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// GNU sums one 4K block per file plus one per directory.
		fblocks := (total + 4095) / 4096
		if total > 0 && fblocks < 1 {
			fblocks = 1
		}
		if total == 0 {
			fblocks = 0
		}
		if *apparent {
			// GNU du -b: apparent size in bytes (exact, portable).
			fmt.Fprintf(hc.Stdout, "%d\t%s\n", total, p)
			continue
		}
		kb := (fblocks + dirs) * 4
		if kb < 4 {
			kb = 4
		}
		_ = summary
		if *human {
			fmt.Fprintf(hc.Stdout, "%s\t%s\n", humanDu(kb*1024), p)
		} else {
			fmt.Fprintf(hc.Stdout, "%d\t%s\n", kb, p)
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ci", float64(n)/float64(div), "KMGTPE"[exp])
}

func cmdSleep(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) < 2 {
		fmt.Fprintln(hc.Stderr, "sleep: missing operand")
		return flag.ErrHelp
	}
	var total time.Duration
	for _, a := range args[1:] {
		// GNU sleep rejects negative intervals as invalid options.
		if strings.HasPrefix(a, "-") {
			fmt.Fprintf(hc.Stderr, "sleep: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
			fmt.Fprintln(hc.Stderr, "Try 'sleep --help' for more information.")
			return exitError{1}
		}
		d, err := parseDurationLoose(a)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "sleep: invalid time interval \u2018%s\u2019\n", a)
			fmt.Fprintln(hc.Stderr, "Try 'sleep --help' for more information.")
			return exitError{1}
		}
		total += d
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(total):
		return nil
	}
}

func parseDurationLoose(s string) (time.Duration, error) {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(v * float64(time.Second)), nil
	}
	return time.ParseDuration(s)
}

func cmdKill(ctx context.Context, hc interp.HandlerContext, args []string) error {
	_ = ctx
	if len(args) > 1 && (args[1] == "-l" || args[1] == "-L" || args[1] == "--list") {
		fmt.Fprintln(hc.Stdout, "HUP INT QUIT ILL TRAP ABRT BUS FPE KILL USR1 SEGV USR2 PIPE ALRM TERM")
		return nil
	}
	sig := "TERM"
	var pids []string
	argv2 := args[1:]
	for i := 0; i < len(argv2); i++ {
		a := argv2[i]
		if a == "--" {
			continue
		}
		// GNU: -s SIG / -n SIGNUM (separate arg) select the signal.
		if (a == "-s" || a == "-n") && i+1 < len(argv2) {
			i++
			name := strings.ToUpper(argv2[i])
			if strings.HasPrefix(name, "SIG") {
				name = name[3:]
			}
			if a == "-n" {
				if !validSignalNumber(name) {
					fmt.Fprintf(hc.Stderr, "kill: %s: invalid signal specification\n", argv2[i])
					return exitError{1}
				}
			}
			sig = name
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			name := strings.ToUpper(strings.TrimPrefix(a, "-"))
			if strings.HasPrefix(name, "SIG") {
				name = name[3:]
			}
			// -N (all digits): signal number if valid, else GNU
			// reports "invalid signal specification".
			if isDigits(name) {
				if validSignalNumber(name) {
					sig = name
					continue
				}
				fmt.Fprintf(hc.Stderr, "kill: %s: invalid signal specification\n", strings.TrimPrefix(a, "-"))
				return exitError{1}
			}
			if a[1] < '0' || a[1] > '9' {
				sig = name
				continue
			}
		}
		pids = append(pids, a)
	}
	if len(pids) == 0 {
		fmt.Fprintln(hc.Stderr, "kill: missing operand")
		return flag.ErrHelp
	}
	code := 0
	for _, p := range pids {
		// Killing ourselves must run the trap, not terminate the process:
		// bash defers the signal to the next statement boundary. Enqueue it
		// instead of signalling the OS, otherwise `kill -TERM $$` would end
		// the shell before its own trap could run.
		if selfPID(p) {
			if !queueSelfSignal(sig) {
				// No trap registered: behave like bash, which dies.
				if p2, err := os.FindProcess(os.Getpid()); err == nil {
					if err := killProc(p2, sig); err != nil {
						code = 1
						continue
					}
				}
				// On Unix, Go's runtime installs handlers with _SigNotify (without
				// _SigKill) for signals such as SIGUSR1/SIGUSR2/SIGALRM, and main.go
				// catches SIGINT/SIGTERM/SIGHUP/SIGQUIT asynchronously. Exit with the
				// POSIX 128+signum status immediately for default-terminating signals.
				if ec := defaultSelfExitCode(sig); ec > 0 {
					os.Exit(ec)
				}
			}
			continue
		}
		// Job table first: fake $! ids ("g1"), real tracked pids,
		// and jobspecs (%1, %%/\%+, \%-, \%name, \%?substr).
		if j := globalJobs.resolveJobSpec(p); j != nil && j.proc != nil {
			if err := killProc(j.proc, sig); err != nil {
				fmt.Fprintf(hc.Stderr, "kill: (%d) - %s\n", j.pid, killErrText(err))
				code = 1
			}
			continue
		}
		pid, err := strconv.Atoi(p)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "kill:", err)
			code = 1
			continue
		}
		// Untracked numeric pid (possibly a fake id that never ran
		// anything external): newest-live-child fallback keeps
		// `kill $!` useful right after `cmd &`.
		if j := globalJobs.byPID(pid); j != nil && j.proc != nil {
			if err := killProc(j.proc, sig); err != nil {
				fmt.Fprintf(hc.Stderr, "kill: (%d) - %s\n", j.pid, killErrText(err))
				code = 1
			}
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "kill: (%d) - No such process\n", pid)
			code = 1
			continue
		}
		if err := killProc(proc, sig); err != nil {
			fmt.Fprintf(hc.Stderr, "kill: (%d) - %s\n", pid, killErrText(err))
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdDate(ctx context.Context, hc interp.HandlerContext, args []string) error {
	now := time.Now()
	// Honor TZ from the environment like GNU date (at minimum UTC and
	// IANA zones when tzdata is available).
	if tz := shellGetenv(hc, "TZ"); tz != "" {
		if tz == "UTC" || tz == "UTC0" || tz == "Z" {
			now = now.UTC()
		} else if loc, err := time.LoadLocation(tz); err == nil {
			now = now.In(loc)
		}
	}
	var format string
	argv := splitAttached("date", args)[1:]
	for k := 0; k < len(argv); k++ {
		a := argv[k]
		switch {
		case a == "-u" || a == "--utc" || a == "--universal":
			now = now.UTC()
		case a == "-d" || a == "--date":
			if k+1 >= len(argv) {
				fmt.Fprintln(hc.Stderr, "date: option requires an argument -- 'd'")
				return exitError{1}
			}
			k++
			t, err := parseTouchDate(argv[k])
			if err != nil {
				fmt.Fprintf(hc.Stderr, "date: invalid date '%s'\n", argv[k])
				return exitError{1}
			}
			// Keep wall clock, like GNU -d (no forced zone shift).
			now = t
		case a == "-r" || a == "--reference":
			// GNU date -r FILE: use FILE's modification time.
			if k+1 >= len(argv) {
				fmt.Fprintln(hc.Stderr, "date: option requires an argument -- 'r'")
				return exitError{1}
			}
			k++
			fi, err := fsStat(ctx, hc.Dir, "date", argv[k])
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Fprintf(hc.Stderr, "date: %s: No such file or directory\n", argv[k])
				} else {
					fmt.Fprintf(hc.Stderr, "date: %s: %v\n", argv[k], err)
				}
				return exitError{1}
			}
			now = fi.ModTime()
		case strings.HasPrefix(a, "+"):
			format = a[1:]
		}
	}
	if format == "" {
		fmt.Fprintln(hc.Stdout, now.Format("Mon Jan _2 15:04:05 MST 2006"))
		return nil
	}
	fmt.Fprintln(hc.Stdout, strftime(now, format))
	return nil
}

func strftime(t time.Time, format string) string {
	var sb strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			sb.WriteByte(format[i])
			continue
		}
		i++
		switch format[i] {
		case 'Y':
			fmt.Fprintf(&sb, "%04d", t.Year())
		case 'm':
			fmt.Fprintf(&sb, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&sb, "%02d", t.Day())
		case 'H':
			fmt.Fprintf(&sb, "%02d", t.Hour())
		case 'M':
			fmt.Fprintf(&sb, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&sb, "%02d", t.Second())
		case 's':
			fmt.Fprintf(&sb, "%d", t.Unix())
		case 'N':
			fmt.Fprintf(&sb, "%09d", t.Nanosecond())
		case 'F':
			fmt.Fprintf(&sb, "%04d-%02d-%02d", t.Year(), int(t.Month()), t.Day())
		case 'T':
			fmt.Fprintf(&sb, "%02d:%02d:%02d", t.Hour(), t.Minute(), t.Second())
		case 'Z':
			name, _ := t.Zone()
			sb.WriteString(name)
		case 'z':
			sb.WriteString(t.Format("-0700"))
		case 'A':
			sb.WriteString(t.Weekday().String())
		case 'a':
			sb.WriteString(t.Weekday().String()[:3])
		case 'B':
			sb.WriteString(t.Month().String())
		case 'b', 'h':
			sb.WriteString(t.Month().String()[:3])
		case 'j':
			fmt.Fprintf(&sb, "%03d", t.YearDay())
		case 'u':
			d := int(t.Weekday())
			if d == 0 {
				d = 7
			}
			fmt.Fprintf(&sb, "%d", d)
		case 'w':
			fmt.Fprintf(&sb, "%d", int(t.Weekday()))
		case 'V':
			_, w := t.ISOWeek()
			fmt.Fprintf(&sb, "%02d", w)
		case 'G':
			y, _ := t.ISOWeek()
			fmt.Fprintf(&sb, "%04d", y)
		case 'C':
			fmt.Fprintf(&sb, "%02d", t.Year()/100)
		case 'y':
			fmt.Fprintf(&sb, "%02d", t.Year()%100)
		case 'e':
			fmt.Fprintf(&sb, "%2d", t.Day())
		case 'k':
			fmt.Fprintf(&sb, "%2d", t.Hour())
		case 'l':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			fmt.Fprintf(&sb, "%2d", h)
		case 'I':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			fmt.Fprintf(&sb, "%02d", h)
		case 'p':
			if t.Hour() < 12 {
				sb.WriteString("AM")
			} else {
				sb.WriteString("PM")
			}
		case 'P':
			if t.Hour() < 12 {
				sb.WriteString("am")
			} else {
				sb.WriteString("pm")
			}
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case '%':
			sb.WriteByte('%')
		default:
			sb.WriteByte('%')
			sb.WriteByte(format[i])
		}
	}
	return sb.String()
}

func cmdMktemp(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("mktemp", hc.Stderr)
	isDir := fs.Bool("d", false, "")
	fs.BoolVar(isDir, "directory", false, "")
	tmpdir := fs.String("p", "", "")
	fs.StringVar(tmpdir, "tmpdir", "", "")
	dryRun := fs.Bool("u", false, "")
	fs.BoolVar(dryRun, "dry-run", false, "")
	fs.Bool("t", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	baseTmp := shellGetenv(hc, "TMPDIR")
	dirSpec := *tmpdir
	if dirSpec == "" {
		dirSpec = baseTmp
	}
	pattern := "tmp.XXXXXXXXXX"
	if len(fs.Args()) > 0 {
		pattern = fs.Args()[0]
	}
	if *tmpdir == "" {
		if d := filepath.Dir(pattern); d != "." && d != "" {
			// A template directory of /tmp (or any absolute POSIX path
			// that does not exist, e.g. on Windows) falls back to the
			// temp dir, matching GNU's "create in /tmp" intent.
			cand := d
			st, serr := fsStat(ctx, hc.Dir, "mktemp", cand)
			if serr != nil {
				if errors.Is(serr, os.ErrPermission) {
					if runtime.GOOS == "windows" && (d == "/tmp" || strings.HasPrefix(d, "/tmp/")) && baseTmp != "" {
						cand = baseTmp
					} else {
						fmt.Fprintln(hc.Stderr, "mktemp:", serr)
						return exitError{1}
					}
				} else if filepath.IsAbs(d) || strings.HasPrefix(d, "/") {
					cand = baseTmp
				}
			} else if !st.IsDir() {
				if filepath.IsAbs(d) || strings.HasPrefix(d, "/") {
					cand = baseTmp
				}
			}
			dirSpec = cand
			pattern = filepath.Base(pattern)
		}
	}
	prefix, suffix, xLen := splitMktempPattern(pattern)
	if *dryRun {
		rawDir := dirSpec
		if rawDir == "" {
			rawDir = os.TempDir()
		}
		checkedDir, err := checkFS(ctx, hc.Dir, "mktemp", rawDir, OpWrite)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "mktemp:", err)
			return exitError{1}
		}
		const alnum = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
		raw := make([]byte, xLen)
		if _, err := rand.Read(raw); err != nil {
			fmt.Fprintln(hc.Stderr, "mktemp:", err)
			return exitError{1}
		}
		var rnd strings.Builder
		rnd.Grow(xLen)
		for _, b := range raw {
			rnd.WriteByte(alnum[int(b)%len(alnum)])
		}
		nm := filepath.Join(checkedDir, prefix+rnd.String()+suffix)
		fmt.Fprintln(hc.Stdout, filepath.ToSlash(nm))
		return nil
	}
	if *isDir {
		name, err := fsMkdirTemp(ctx, hc.Dir, "mktemp", dirSpec, prefix+"*"+suffix)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "mktemp:", err)
			return exitError{1}
		}
		fmt.Fprintln(hc.Stdout, filepath.ToSlash(name))
		return nil
	}
	f, err := fsCreateTemp(ctx, hc.Dir, "mktemp", dirSpec, prefix+"*"+suffix)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "mktemp:", err)
		return exitError{1}
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(hc.Stderr, "mktemp:", err)
		return exitError{1}
	}
	fmt.Fprintln(hc.Stdout, filepath.ToSlash(f.Name()))
	return nil
}

func splitMktempPattern(pattern string) (prefix, suffix string, xLen int) {
	if i := strings.Index(pattern, "XX"); i >= 0 {
		j := i
		for j < len(pattern) && pattern[j] == 'X' {
			j++
		}
		return pattern[:i], pattern[j:], j - i
	}
	return pattern + ".", "", 10
}

type findNode struct {
	ctx        context.Context
	baseDir    string
	hookActive bool
	display    string
	full       string
	dirFull    string
	name       string
	ent        os.DirEntry
	fi         os.FileInfo // loaded lazily (find -type f doesn't need it)
	typ        os.FileMode // from d_type when known
	typKnown   bool
	depth      int
	pruned     bool
	follow     bool
}

func (n *findNode) fullPath() string {
	if n.full == "" && n.dirFull != "" && n.name != "" {
		n.full = filepath.Join(n.dirFull, n.name)
	}
	return n.full
}

func entTypeKnown(t os.FileMode) bool {
	return t != ^os.FileMode(0) && t != os.ModeIrregular
}

// stat lazily loads the file info; GNU find only stats entries whose
// predicates need the metadata (the type comes from readdir otherwise).
// When !follow and DirEntry is available, DirEntry.Info() reuses cached
// directory metadata on Windows without extra syscalls.
func (n *findNode) stat() os.FileInfo {
	if n.fi == nil {
		var (
			fi  os.FileInfo
			err error
		)
		if n.hookActive {
			if n.follow {
				fi, err = fsStat(n.ctx, n.baseDir, "find", n.display)
				if err != nil && !errors.Is(err, os.ErrPermission) {
					fi, err = fsLstat(n.ctx, n.baseDir, "find", n.display)
				}
			} else {
				fi, err = fsLstat(n.ctx, n.baseDir, "find", n.display)
			}
		} else if n.follow {
			fp := n.fullPath()
			fi, err = os.Stat(fp)
			if err != nil {
				fi, err = os.Lstat(fp)
			}
		} else if n.ent != nil {
			fi, err = n.ent.Info()
			if err != nil {
				fi, err = os.Lstat(n.fullPath())
			}
		} else {
			fi, err = os.Lstat(n.fullPath())
		}
		if err != nil {
			fi = zeroFileInfo{}
		}
		n.fi = fi
	}
	return n.fi
}

// zeroFileInfo stands in for entries that disappeared mid-walk.
type zeroFileInfo struct{}

func (zeroFileInfo) Name() string       { return "" }
func (zeroFileInfo) Size() int64        { return 0 }
func (zeroFileInfo) Mode() os.FileMode  { return 0 }
func (zeroFileInfo) ModTime() time.Time { return time.Time{} }
func (zeroFileInfo) IsDir() bool        { return false }
func (zeroFileInfo) Sys() any           { return nil }

// isDir reports whether the node is a directory, preferring d_type.
func (n *findNode) isDir() bool {
	if n.typKnown {
		return n.typ.IsDir()
	}
	fi := n.stat()
	return fi != nil && fi.IsDir()
}

// typeIs matches a find -type letter, preferring d_type over a stat
// (GNU find only stats entries when it must).
func (n *findNode) typeIs(letter string) bool {
	m := n.typ
	if !n.typKnown {
		fi := n.stat()
		if fi == nil {
			return false
		}
		m = fi.Mode()
	}
	switch letter {
	case "d":
		return m.IsDir()
	case "f":
		return m.IsRegular()
	case "l":
		return m&os.ModeSymlink != 0
	case "s":
		return m&os.ModeSocket != 0
	case "p":
		return m&os.ModeNamedPipe != 0
	case "b":
		return m&os.ModeDevice != 0 && m&os.ModeCharDevice == 0
	case "c":
		return m&os.ModeCharDevice != 0
	}
	return false
}

type findPredicate func(node *findNode, hc interp.HandlerContext) bool

func cmdFind(ctx context.Context, hc interp.HandlerContext, args []string) error {
	followMode := 0 // 0 = -P (never), 1 = -H (root operands only), 2 = -L (always)
	idx := 1
	for idx < len(args) {
		switch args[idx] {
		case "-P":
			followMode = 0
			idx++
			continue
		case "-H":
			followMode = 1
			idx++
			continue
		case "-L":
			followMode = 2
			idx++
			continue
		case "--":
			idx++
		}
		break
	}
	var paths, expr []string
	seenExpr := false
	for _, a := range args[idx:] {
		if !seenExpr && !strings.HasPrefix(a, "-") && a != "!" && a != "(" && a != ")" {
			paths = append(paths, a)
		} else {
			seenExpr = true
			expr = append(expr, a)
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}

	maxdepth := -1
	mindepth := 0
	depthFirst := false
	hasAction := false
	stop := false // set when stdout pipe closes (head -c/N): abort walk
	code := 0

	type pendingExecBatch struct {
		template []string
		paths    []string
	}
	var plusBatches []*pendingExecBatch

	pos := 0
	peek := func() string {
		if pos < len(expr) {
			return expr[pos]
		}
		return ""
	}
	next := func() string {
		if pos < len(expr) {
			tok := expr[pos]
			pos++
			return tok
		}
		return ""
	}

	matchPath := func(pat, str string, ignoreCase bool) bool {
		pat = filepath.ToSlash(pat)
		str = filepath.ToSlash(str)
		if ignoreCase {
			pat = strings.ToLower(pat)
			str = strings.ToLower(str)
		}
		if ok, _ := filepath.Match(pat, str); ok {
			return true
		}
		cleanStr := strings.TrimPrefix(str, "./")
		if ok, _ := filepath.Match(pat, cleanStr); ok {
			return true
		}
		if ok, _ := filepath.Match("./"+pat, str); ok {
			return true
		}
		cleanPat := strings.Trim(pat, "/")
		if !strings.ContainsAny(pat, "*?[") {
			if strings.Contains(str, "/"+cleanPat+"/") || strings.HasSuffix(str, "/"+cleanPat) || cleanStr == cleanPat || strings.Contains(str, cleanPat) {
				return true
			}
		}
		return false
	}

	var parseOr func() findPredicate
	var parseAnd func() findPredicate
	var parseNot func() findPredicate
	var parsePrimary func() findPredicate
	var parseErr error

	parseOr = func() findPredicate {
		left := parseAnd()
		for peek() == "-o" || peek() == "-or" {
			next()
			right := parseAnd()
			l, r := left, right
			left = func(node *findNode, hc interp.HandlerContext) bool {
				return l(node, hc) || r(node, hc)
			}
		}
		return left
	}

	parseAnd = func() findPredicate {
		left := parseNot()
		for {
			p := peek()
			if p == "" || p == ")" || p == "-o" || p == "-or" {
				break
			}
			if p == "-a" || p == "-and" {
				next()
			}
			right := parseNot()
			l, r := left, right
			left = func(node *findNode, hc interp.HandlerContext) bool {
				return l(node, hc) && r(node, hc)
			}
		}
		return left
	}

	parseNot = func() findPredicate {
		if peek() == "!" || peek() == "-not" {
			next()
			inner := parseNot()
			return func(node *findNode, hc interp.HandlerContext) bool {
				return !inner(node, hc)
			}
		}
		return parsePrimary()
	}

	parsePrimary = func() findPredicate {
		if pos >= len(expr) {
			return func(*findNode, interp.HandlerContext) bool { return true }
		}
		tok := next()
		if tok == "(" {
			if peek() == ")" || pos >= len(expr) {
				fmt.Fprintln(hc.Stderr, "find: invalid expression; empty parentheses are not allowed.")
				parseErr = fmt.Errorf("empty parens")
				return func(*findNode, interp.HandlerContext) bool { return false }
			}
			sub := parseOr()
			if peek() != ")" {
				fmt.Fprintln(hc.Stderr, "find: invalid expression; I was expecting to find a ')' somewhere but did not see one.")
				parseErr = fmt.Errorf("missing )")
				return func(*findNode, interp.HandlerContext) bool { return false }
			}
			next()
			return sub
		}
		if tok == ")" {
			fmt.Fprintln(hc.Stderr, "find: invalid expression; you have too many ')'")
			parseErr = fmt.Errorf("unexpected )")
			return func(*findNode, interp.HandlerContext) bool { return false }
		}
		switch tok {
		case "-name":
			pat := next()
			hasMeta := strings.ContainsAny(pat, "*?[")
			dotPrefix := strings.HasPrefix(pat, ".") && !hasMeta
			return func(n *findNode, _ interp.HandlerContext) bool {
				// GNU matches -name against the basename of the path
				// as given ("." for the root), not the resolved name.
				base := n.name
				if base == "" {
					if b := pathBase(n.display); b != "" {
						base = b
					} else {
						base = filepath.Base(n.fullPath())
					}
				}
				if !hasMeta {
					if base == pat {
						return true
					}
					if dotPrefix {
						return strings.HasSuffix(base, pat)
					}
					return false
				}
				if ok, _ := filepath.Match(pat, base); ok {
					return true
				}
				return false
			}
		case "-iname":
			pat := strings.ToLower(next())
			hasMeta := strings.ContainsAny(pat, "*?[")
			dotPrefix := strings.HasPrefix(pat, ".") && !hasMeta
			return func(n *findNode, _ interp.HandlerContext) bool {
				base := n.name
				if base == "" {
					if b := pathBase(n.display); b != "" {
						base = b
					} else {
						base = filepath.Base(n.fullPath())
					}
				}
				name := strings.ToLower(base)
				if !hasMeta {
					if name == pat {
						return true
					}
					if dotPrefix {
						return strings.HasSuffix(name, pat)
					}
					return false
				}
				if ok, _ := filepath.Match(pat, name); ok {
					return true
				}
				return false
			}
		case "-path", "-wholename":
			pat := next()
			return func(n *findNode, _ interp.HandlerContext) bool {
				return matchPath(pat, n.display, false)
			}
		case "-ipath", "-iwholename":
			pat := next()
			return func(n *findNode, _ interp.HandlerContext) bool {
				return matchPath(pat, n.display, true)
			}
		case "-type":
			t := next()
			return func(n *findNode, _ interp.HandlerContext) bool {
				return n.typeIs(t)
			}
		case "-maxdepth":
			if v, err := strconv.Atoi(next()); err == nil {
				maxdepth = v
			}
			return func(*findNode, interp.HandlerContext) bool { return true }
		case "-mindepth":
			if v, err := strconv.Atoi(next()); err == nil {
				mindepth = v
			}
			return func(*findNode, interp.HandlerContext) bool { return true }
		case "-prune":
			return func(n *findNode, _ interp.HandlerContext) bool {
				if n.isDir() {
					n.pruned = true
				}
				return true
			}
		case "-empty":
			return func(n *findNode, _ interp.HandlerContext) bool {
				if n.isDir() {
					var entries []os.DirEntry
					var err error
					if n.hookActive {
						entries, err = fsReadDir(n.ctx, n.baseDir, "find", n.display)
					} else {
						entries, err = os.ReadDir(n.fullPath())
					}
					return err == nil && len(entries) == 0
				}
				return n.stat().Size() == 0
			}
		case "-size":
			s := next()
			sign := 0
			if strings.HasPrefix(s, "+") {
				sign = 1
				s = s[1:]
			} else if strings.HasPrefix(s, "-") {
				sign = -1
				s = s[1:]
			}
			unit := int64(512)
			if len(s) > 0 {
				switch s[len(s)-1] {
				case 'c':
					unit = 1
					s = s[:len(s)-1]
				case 'w':
					unit = 2
					s = s[:len(s)-1]
				case 'b':
					unit = 512
					s = s[:len(s)-1]
				case 'k':
					unit = 1024
					s = s[:len(s)-1]
				case 'M':
					unit = 1024 * 1024
					s = s[:len(s)-1]
				case 'G':
					unit = 1024 * 1024 * 1024
					s = s[:len(s)-1]
				}
			}
			val, _ := strconv.ParseInt(s, 10, 64)
			targetBytes := val * unit
			return func(n *findNode, _ interp.HandlerContext) bool {
				sz := n.stat().Size()
				if sign > 0 {
					return sz > targetBytes
				} else if sign < 0 {
					return sz < targetBytes
				}
				return sz == targetBytes
			}
		case "-mtime":
			s := next()
			sign := 0
			if strings.HasPrefix(s, "+") {
				sign = 1
				s = s[1:]
			} else if strings.HasPrefix(s, "-") {
				sign = -1
				s = s[1:]
			}
			days, _ := strconv.Atoi(s)
			now := time.Now()
			return func(n *findNode, _ interp.HandlerContext) bool {
				ageDays := int(now.Sub(n.stat().ModTime()).Hours() / 24)
				if sign > 0 {
					return ageDays > days
				} else if sign < 0 {
					return ageDays < days
				}
				return ageDays == days
			}
		case "-mmin":
			s := next()
			sign := 0
			if strings.HasPrefix(s, "+") {
				sign = 1
				s = s[1:]
			} else if strings.HasPrefix(s, "-") {
				sign = -1
				s = s[1:]
			}
			mins, _ := strconv.Atoi(s)
			now := time.Now()
			return func(n *findNode, _ interp.HandlerContext) bool {
				ageMins := int(now.Sub(n.stat().ModTime()).Minutes())
				if sign > 0 {
					return ageMins > mins
				} else if sign < 0 {
					return ageMins < mins
				}
				return ageMins == mins
			}
		case "-print":
			hasAction = true
			return func(n *findNode, hc interp.HandlerContext) bool {
				if _, err := fmt.Fprintln(hc.Stdout, filepath.ToSlash(n.display)); err != nil {
					if isPipeClosed(err) {
						stop = true
					}
				}
				return true
			}
		case "-print0":
			hasAction = true
			return func(n *findNode, hc interp.HandlerContext) bool {
				if _, err := fmt.Fprintf(hc.Stdout, "%s\x00", filepath.ToSlash(n.display)); err != nil {
					if isPipeClosed(err) {
						stop = true
					}
				}
				return true
			}
		case "-follow":
			followMode = 2
			return func(*findNode, interp.HandlerContext) bool { return true }
		case "-depth", "-d":
			depthFirst = true
			return func(*findNode, interp.HandlerContext) bool { return true }
		case "-delete":
			hasAction = true
			depthFirst = true
			return func(n *findNode, _ interp.HandlerContext) bool {
				if n.depth == 0 && isDotOperand(n.display) {
					return true
				}
				err := fsRemove(ctx, hc.Dir, "find", n.display)
				if err != nil && runtime.GOOS == "windows" && !errors.Is(err, os.ErrPermission) && !n.typeIs("l") {
					_ = fsChmod(ctx, hc.Dir, "find", n.display, 0o666)
					err = fsRemove(ctx, hc.Dir, "find", n.display)
				}
				if err != nil {
					fmt.Fprintf(hc.Stderr, "find: cannot delete '%s': %v\n", n.display, err)
					code = 1
					return false
				}
				return true
			}
		case "-exec":
			hasAction = true
			var execArgs []string
			term := ""
			for pos < len(expr) {
				a := next()
				if a == ";" || a == "+" {
					term = a
					break
				}
				execArgs = append(execArgs, a)
			}
			if term == "" || len(execArgs) == 0 {
				fmt.Fprintln(hc.Stderr, "find: missing argument to `-exec'")
				parseErr = fmt.Errorf("missing -exec terminator")
				return func(*findNode, interp.HandlerContext) bool { return false }
			}
			if term == "+" {
				pb := &pendingExecBatch{template: execArgs}
				plusBatches = append(plusBatches, pb)
				return func(n *findNode, _ interp.HandlerContext) bool {
					pb.paths = append(pb.paths, filepath.ToSlash(n.display))
					return true
				}
			}
			return func(n *findNode, hc interp.HandlerContext) bool {
				disp := filepath.ToSlash(n.display)
				cmdLine := make([]string, len(execArgs))
				for i, a := range execArgs {
					cmdLine[i] = strings.ReplaceAll(a, "{}", disp)
				}
				subHC := hc
				subHC.Stdin = bytes.NewReader(nil)
				err := runSubcommandNamed(ctx, subHC, "find", cmdLine)
				if ctx.Err() != nil {
					stop = true
					return false
				}
				if errors.Is(err, os.ErrPermission) {
					fmt.Fprintf(hc.Stderr, "find: '%s': %v\n", cmdLine[0], err)
					code = 1
				}
				return err == nil
			}
		case "-perm":
			modeStr := next()
			if !validPermSpec(modeStr) {
				fmt.Fprintf(hc.Stderr, "find: invalid mode \u2018%s\u2019\n", modeStr)
				parseErr = fmt.Errorf("invalid mode")
			}
			return func(n *findNode, _ interp.HandlerContext) bool {
				return matchPerm(n, modeStr)
			}
		case "-newer", "-anewer", "-cnewer":
			ref := next()
			fi, err := fsStat(ctx, hc.Dir, "find", ref)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Fprintf(hc.Stderr, "find: '%s': No such file or directory\n", ref)
				} else {
					fmt.Fprintf(hc.Stderr, "find: '%s': %v\n", ref, err)
				}
				if errors.Is(err, os.ErrPermission) {
					code = 1
				}
				return func(*findNode, interp.HandlerContext) bool { return false }
			}
			refTime := fi.ModTime()
			return func(n *findNode, _ interp.HandlerContext) bool {
				return n.stat().ModTime().After(refTime)
			}
		case "-true":
			return func(*findNode, interp.HandlerContext) bool { return true }
		case "-false":
			return func(*findNode, interp.HandlerContext) bool { return false }
		default:
			fmt.Fprintf(hc.Stderr, "find: unknown predicate `%s'\n", tok)
			parseErr = fmt.Errorf("unknown predicate")
			return func(*findNode, interp.HandlerContext) bool { return true }
		}
	}

	var rootPred findPredicate
	if len(expr) > 0 {
		rootPred = parseOr()
		if parseErr == nil && pos < len(expr) {
			if expr[pos] == ")" {
				fmt.Fprintln(hc.Stderr, "find: invalid expression; you have too many ')'")
			} else {
				fmt.Fprintf(hc.Stderr, "find: paths must precede expression: `%s'\n", expr[pos])
			}
			parseErr = fmt.Errorf("unexpected trailing token")
		}
	} else {
		rootPred = func(*findNode, interp.HandlerContext) bool { return true }
	}
	if parseErr != nil {
		// GNU find reports the bad predicate and exits 1 without any
		// traversal output.
		return exitError{1}
	}
	// Buffered output when no actions run: -print is line-per-file and
	// slow unbuffered. Actions (-exec) must interleave with output in
	// order, so those runs stay unbuffered.
	var outBW *bufio.Writer
	if !hasAction {
		outBW = getBufWriter64K(hc.Stdout)
		hc.Stdout = outBW
		defer func() {
			_ = outBW.Flush()
			putBufWriter64K(outBW)
		}()
	}

	opts := findWalkOpts{
		baseDir:    hc.Dir,
		hookActive: CapabilityHookFromContext(ctx) != nil,
		maxdepth:   maxdepth,
		mindepth:   mindepth,
		followMode: followMode,
		depthFirst: depthFirst,
		hasAction:  hasAction,
		outBW:      outBW,
	}
	for _, p := range paths {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		opts.needSlash = strings.ContainsRune(p, '\\')
		full := resolveHostPath(hc.Dir, p)
		walkFind(ctx, hc, p, full, "", "", 0, opts, rootPred, &code, &stop, nil, nil)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, pb := range plusBatches {
		if len(pb.paths) == 0 {
			continue
		}
		const chunkMax = 1024
		for i := 0; i < len(pb.paths); i += chunkMax {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			end := i + chunkMax
			if end > len(pb.paths) {
				end = len(pb.paths)
			}
			chunk := pb.paths[i:end]
			var cmdLine []string
			substituted := false
			for _, a := range pb.template {
				if a == "{}" {
					cmdLine = append(cmdLine, chunk...)
					substituted = true
				} else if strings.Contains(a, "{}") {
					for _, cp := range chunk {
						cmdLine = append(cmdLine, strings.ReplaceAll(a, "{}", cp))
					}
					substituted = true
				} else {
					cmdLine = append(cmdLine, a)
				}
			}
			if !substituted {
				cmdLine = append(cmdLine, chunk...)
			}
			subHC := hc
			subHC.Stdin = bytes.NewReader(nil)
			if err := runSubcommandNamed(ctx, subHC, "find", cmdLine); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.Is(err, os.ErrPermission) {
					fmt.Fprintf(hc.Stderr, "find: '%s': %v\n", cmdLine[0], err)
				}
				code = 1
			}
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

type findWalkOpts struct {
	baseDir    string
	hookActive bool
	maxdepth   int
	mindepth   int
	followMode int
	depthFirst bool
	hasAction  bool
	needSlash  bool
	outBW      *bufio.Writer
}

func walkFind(ctx context.Context, hc interp.HandlerContext, display, full, dirFull, name string, depth int, opts findWalkOpts, pred findPredicate, code *int, stop *bool, ent os.DirEntry, ancestors []os.FileInfo) {
	if ctx.Err() != nil || (stop != nil && *stop) {
		return
	}
	followHere := opts.followMode == 2 || (opts.followMode == 1 && depth == 0)
	node := findNode{
		ctx:        ctx,
		baseDir:    opts.baseDir,
		hookActive: opts.hookActive,
		display:    display,
		full:       full,
		dirFull:    dirFull,
		name:       name,
		ent:        ent,
		depth:      depth,
		follow:     followHere,
	}
	if opts.hookActive {
		if followHere {
			sfi, serr := fsStat(ctx, opts.baseDir, "find", display)
			if serr == nil {
				node.fi = sfi
				node.typ = sfi.Mode().Type()
				node.typKnown = true
			} else if errors.Is(serr, os.ErrPermission) {
				fmt.Fprintf(hc.Stderr, "find: '%s': %v\n", display, serr)
				*code = 1
				return
			} else {
				lfi, lerr := fsLstat(ctx, opts.baseDir, "find", display)
				if lerr != nil {
					if os.IsNotExist(lerr) {
						fmt.Fprintf(hc.Stderr, "find: '%s': No such file or directory\n", display)
					} else {
						fmt.Fprintf(hc.Stderr, "find: '%s': %v\n", display, lerr)
					}
					*code = 1
					return
				}
				if strings.Contains(strings.ToLower(serr.Error()), "too many links") ||
					strings.Contains(strings.ToLower(serr.Error()), "too many levels") {
					fmt.Fprintf(hc.Stderr, "find: File system loop detected; '%s' is part of the same file system loop\n", filepath.ToSlash(display))
					*code = 1
					return
				}
				node.fi = lfi
				node.typ = lfi.Mode().Type()
				node.typKnown = true
			}
		} else {
			lfi, lerr := fsLstat(ctx, opts.baseDir, "find", display)
			if lerr != nil {
				if errors.Is(lerr, os.ErrPermission) && depth > 0 {
					return
				}
				if os.IsNotExist(lerr) {
					fmt.Fprintf(hc.Stderr, "find: '%s': No such file or directory\n", display)
				} else {
					fmt.Fprintf(hc.Stderr, "find: '%s': %v\n", display, lerr)
				}
				*code = 1
				return
			}
			node.fi = lfi
			node.typ = lfi.Mode().Type()
			node.typKnown = true
		}
	} else if followHere {
		if ent == nil || !entTypeKnown(ent.Type()) || ent.Type()&os.ModeSymlink != 0 {
			fp := node.fullPath()
			sfi, serr := os.Stat(fp)
			if serr == nil {
				node.fi = sfi
				node.typ = sfi.Mode().Type()
				node.typKnown = true
			} else {
				lfi, lerr := os.Lstat(fp)
				if lerr != nil {
					fmt.Fprintf(hc.Stderr, "find: '%s': No such file or directory\n", display)
					*code = 1
					return
				}
				if strings.Contains(strings.ToLower(serr.Error()), "too many links") ||
					strings.Contains(strings.ToLower(serr.Error()), "too many levels") {
					fmt.Fprintf(hc.Stderr, "find: File system loop detected; '%s' is part of the same file system loop\n", filepath.ToSlash(display))
					*code = 1
					return
				}
				node.fi = lfi
				node.typ = lfi.Mode().Type()
				node.typKnown = true
			}
		} else {
			node.typ = ent.Type()
			node.typKnown = true
		}
	} else if ent != nil {
		if entTypeKnown(ent.Type()) {
			node.typ = ent.Type()
			node.typKnown = true
		}
	} else {
		fp := node.fullPath()
		fi, err := os.Lstat(fp)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "find: '%s': No such file or directory\n", display)
			*code = 1
			return
		}
		node.fi = fi
		node.typ = fi.Mode().Type()
		node.typKnown = true
	}

	var dirFi os.FileInfo
	if node.isDir() && opts.followMode != 0 {
		dirFi = node.fi
		if dirFi == nil {
			var sfi os.FileInfo
			var serr error
			if opts.hookActive {
				sfi, serr = fsStat(ctx, opts.baseDir, "find", display)
			} else {
				sfi, serr = os.Stat(node.fullPath())
			}
			if serr == nil {
				dirFi = sfi
			}
		}
		if dirFi != nil {
			for _, anc := range ancestors {
				if os.SameFile(dirFi, anc) {
					fmt.Fprintf(hc.Stderr, "find: File system loop detected; '%s' is part of the same file system loop\n", filepath.ToSlash(display))
					*code = 1
					return
				}
			}
		}
	}

	evalCurrent := func() {
		if depth < opts.mindepth {
			return
		}
		match := pred(&node, hc)
		if match && !opts.hasAction && (stop == nil || !*stop) {
			outDisp := display
			if opts.needSlash {
				outDisp = filepath.ToSlash(display)
			}
			var err error
			if opts.outBW != nil {
				_, err = opts.outBW.WriteString(outDisp)
				if err == nil {
					err = opts.outBW.WriteByte('\n')
				}
			} else {
				_, err = fmt.Fprintln(hc.Stdout, outDisp)
			}
			if err != nil && isPipeClosed(err) {
				*stop = true
			}
		}
	}

	descendChildren := func() {
		if node.pruned || !node.isDir() {
			return
		}
		if opts.maxdepth >= 0 && depth >= opts.maxdepth {
			return
		}
		curFull := node.fullPath()
		var entries []os.DirEntry
		var err error
		if opts.hookActive {
			entries, err = fsReadDir(ctx, opts.baseDir, "find", display)
		} else {
			entries, err = os.ReadDir(curFull)
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "find: '%s': %v\n", display, err)
			*code = 1
			return
		}
		nextAncestors := ancestors
		if dirFi != nil {
			nextAncestors = append(ancestors, dirFi)
		}
		dispPrefix := display
		if dispPrefix == "." {
			dispPrefix = "./"
		} else if !strings.HasSuffix(dispPrefix, "/") {
			dispPrefix = dispPrefix + "/"
		}
		for _, e := range entries {
			if ctx.Err() != nil || (stop != nil && *stop) {
				return
			}
			ename := e.Name()
			walkFind(ctx, hc, dispPrefix+ename, "", curFull, ename, depth+1, opts, pred, code, stop, e, nextAncestors)
		}
	}

	if opts.depthFirst {
		descendChildren()
		if ctx.Err() != nil || (stop != nil && *stop) {
			return
		}
		evalCurrent()
		return
	}

	evalCurrent()
	if ctx.Err() != nil || (stop != nil && *stop) {
		return
	}
	descendChildren()
}

func cmdMd5sum(ctx context.Context, hc interp.HandlerContext, args []string) error {
	return cmdHash(ctx, "md5sum", md5.New, hc, args)
}

func cmdSha1sum(ctx context.Context, hc interp.HandlerContext, args []string) error {
	return cmdHash(ctx, "sha1sum", sha1.New, hc, args)
}

func cmdSha256sum(ctx context.Context, hc interp.HandlerContext, args []string) error {
	return cmdHash(ctx, "sha256sum", sha256.New, hc, args)
}

func cmdShasum(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("shasum", hc.Stderr)
	algo := fs.String("a", "1", "")
	fs.StringVar(algo, "algorithm", "1", "")
	fs.Bool("b", false, "")
	fs.Bool("t", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	newHash := sha1.New
	switch *algo {
	case "256":
		newHash = sha256.New
	case "512":
		newHash = sha512.New
	case "384":
		newHash = sha512.New384
	case "224":
		newHash = sha256.New224
	case "1":
		newHash = sha1.New
	default:
		fmt.Fprintf(hc.Stderr, "shasum: unrecognized algorithm '%s'\n", *algo)
		return exitError{1}
	}
	return runHash(ctx, "shasum", newHash, hc, fs.Args())
}

func cmdHash(ctx context.Context, name string, newHash func() hash.Hash, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet(name, hc.Stderr)
	fs.Bool("b", false, "")
	fs.Bool("binary", false, "")
	fs.Bool("t", false, "")
	fs.Bool("text", false, "")
	check := fs.Bool("c", false, "")
	fs.BoolVar(check, "check", false, "")
	quiet := fs.Bool("quiet", false, "")
	fs.BoolVar(quiet, "q", false, "")
	status := fs.Bool("status", false, "")
	fs.BoolVar(status, "s", false, "")
	strict := fs.Bool("strict", false, "")
	fs.BoolVar(strict, "z", false, "")
	fs.Bool("tag", false, "")
	warn := fs.Bool("w", false, "")
	fs.BoolVar(warn, "warn", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *check {
		return runHashCheck(ctx, name, newHash, hc, fs.Args(), *quiet, *status, *strict, *warn)
	}
	return runHash(ctx, name, newHash, hc, fs.Args())
}

func runHash(ctx context.Context, name string, newHash func() hash.Hash, hc interp.HandlerContext, files []string) error {
	if len(files) == 0 {
		files = []string{"-"}
	}
	code := 0
	for _, f := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var r io.Reader = hc.Stdin
		label := f
		if f != "-" && f != "/dev/stdin" {
			fh, err := fsOpenShellFile(ctx, hc.Dir, name, f)
			if err != nil {
				fmt.Fprintln(hc.Stderr, name+":", err)
				code = 1
				continue
			}
			func() {
				defer fh.Close()
				h := newHash()
				if _, err := io.Copy(h, fh); err != nil {
					fmt.Fprintln(hc.Stderr, name+":", err)
					code = 1
					return
				}
				fmt.Fprintf(hc.Stdout, "%s  %s\n", hex.EncodeToString(h.Sum(nil)), f)
			}()
			continue
		}
		h := newHash()
		if _, err := io.Copy(h, r); err != nil {
			fmt.Fprintln(hc.Stderr, name+":", err)
			code = 1
			continue
		}
		fmt.Fprintf(hc.Stdout, "%s  %s\n", hex.EncodeToString(h.Sum(nil)), label)
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

var _ = runtime.GOOS

// runHashCheck implements `md5sum/sha*sum -c FILE`: reads checksum lines
// ("<hex> [* ]<path>") and verifies each file, printing "path: OK" or
// "path: FAILED" like GNU. Returns exit 0 iff all checked files match.
func runHashCheck(ctx context.Context, name string, newHash func() hash.Hash, hc interp.HandlerContext, files []string, quiet, status, strict, warn bool) error {
	if len(files) == 0 {
		files = []string{"-"}
	}
	code := 0
	checked := 0
	failed := 0
	badFmt := 0
	for _, f := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var data []byte
		var err error
		if f == "-" || f == "/dev/stdin" {
			data, err = io.ReadAll(hc.Stdin)
		} else {
			data, err = fsReadShellFile(ctx, hc.Dir, name, f)
		}
		if err != nil {
			fmt.Fprintln(hc.Stderr, name+":", err)
			code = 1
			continue
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			sum, path, ok := parseHashLine(line)
			if !ok {
				badFmt++
				if warn && !status {
					fmt.Fprintf(hc.Stderr, "%s: %s: %d: improperly formatted checksum line\n", name, f, lineNo+1)
				}
				continue
			}
			checked++
			raw, err := fsReadShellFile(ctx, hc.Dir, name, path)
			if err != nil {
				if !status {
					fmt.Fprintf(hc.Stderr, "%s: %s: %v\n", name, path, err)
				}
				code = 1
				failed++
				continue
			}
			h := newHash()
			h.Write(raw)
			got := hex.EncodeToString(h.Sum(nil))
			if !strings.EqualFold(got, sum) {
				if !status {
					if !quiet {
						fmt.Fprintf(hc.Stdout, "%s: FAILED\n", path)
					}
					fmt.Fprintf(hc.Stderr, "%s: WARNING: 1 computed checksum did NOT match\n", name)
				}
				code = 1
				failed++
				continue
			}
			if !status && !quiet {
				fmt.Fprintf(hc.Stdout, "%s: OK\n", path)
			}
		}
	}
	if checked == 0 {
		for _, f := range files {
			if f != "-" {
				fmt.Fprintf(hc.Stderr, "%s: %s: no properly formatted checksum lines found\n", name, f)
			}
		}
		return exitError{1}
	}
	if strict && badFmt > 0 {
		code = 1
	}
	_ = failed
	if code != 0 {
		return exitError{code}
	}
	return nil
}

// parseHashLine splits a GNU checksum line: "<hex>[ *]<path>" or BSD
// "ALGO (path) = <hex>". Returns sum, path, ok.
func parseHashLine(line string) (string, string, bool) {
	if i := strings.Index(line, " = "); i >= 0 {
		left, sum := line[:i], strings.TrimSpace(line[i+3:])
		if o := strings.Index(left, "("); o >= 0 {
			if c := strings.LastIndex(left, ")"); c > o {
				if isHex(sum) {
					return sum, left[o+1 : c], true
				}
			}
		}
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", false
	}
	sum := fields[0]
	if !isHex(sum) {
		return "", "", false
	}
	rest := strings.TrimSpace(line[len(sum):])
	if rest == "" {
		return "", "", false
	}
	// GNU separates sum and path with "  " (text), " *" (binary),
	// or " *" with escaped backslash/line: strip one leading marker.
	rest = strings.TrimPrefix(rest, "*")
	rest = strings.TrimPrefix(rest, " ")
	if rest == "" {
		return "", "", false
	}
	return sum, rest, true
}

func isHex(s string) bool {
	if len(s) < 8 || len(s)%2 != 0 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// humanDu formats bytes GNU-du -h style: "4.0K", "1.2M" (powers of 1024,
// one decimal, K/M/G suffix without "iB").
func humanDu(n int64) string {
	if n < 1024 {
		// GNU rounds sub-K sizes up into K display via block count;
		// direct byte values under 1K show as e.g. "4.0K" after blocks.
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	}
	units := []string{"K", "M", "G", "T", "P", "E"}
	v := float64(n) / 1024
	u := 0
	for v >= 1024 && u < len(units)-1 {
		v /= 1024
		u++
	}
	if v >= 10 {
		return fmt.Sprintf("%.0f%s", v, units[u])
	}
	return fmt.Sprintf("%.1f%s", v, units[u])
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// validSignalNumber reports whether n names a real signal (1-64,
// excluding 32-33 on Linux). Used for `kill -N` parsing like GNU.
func validSignalNumber(n string) bool {
	v, err := strconv.Atoi(n)
	if err != nil {
		return false
	}
	// 0 is valid: it checks whether the process exists without signalling
	// it (kill -0 PID).
	if v == 0 {
		return true
	}
	if v >= 1 && v <= 31 {
		return true
	}
	if v >= 34 && v <= 64 {
		return true
	}
	return false
}

// validPermSpec reports whether a find -perm MODE operand parses.
func validPermSpec(spec string) bool {
	s := strings.TrimLeft(spec, "-/")
	if s == "" {
		return false
	}
	if _, err := strconv.ParseUint(s, 8, 32); err == nil {
		return true
	}
	for _, clause := range strings.Split(spec, ",") {
		op := strings.IndexAny(clause, "+-=")
		if op <= 0 {
			return false
		}
		for _, c := range clause[op+1:] {
			if c != 'r' && c != 'w' && c != 'x' && c != 'X' && c != 's' && c != 't' && c != 'u' && c != 'g' && c != 'o' {
				return false
			}
		}
	}
	return true
}

// matchPerm implements find -perm MODE: exact match (644), all-bits
// (-mode), any-bit (/mode). Symbolic specs (u+x) match when all listed
// bits are present.
func matchPerm(n *findNode, spec string) bool {
	perm := n.stat().Mode().Perm()
	if strings.HasPrefix(spec, "-") {
		want, err := strconv.ParseUint(spec[1:], 8, 32)
		if err != nil {
			return false
		}
		return uint32(perm)&uint32(want) == uint32(want)
	}
	if strings.HasPrefix(spec, "/") {
		want, err := strconv.ParseUint(spec[1:], 8, 32)
		if err != nil {
			return false
		}
		return uint32(perm)&uint32(want) != 0
	}
	if want, err := strconv.ParseUint(spec, 8, 32); err == nil {
		return uint32(perm) == uint32(want)
	}
	// Symbolic: all of u=rwx,g=..,o=.. bits present (best-effort).
	want := os.FileMode(0)
	for _, clause := range strings.Split(spec, ",") {
		op := strings.IndexAny(clause, "+-=")
		if op < 0 {
			return false
		}
		who, perms := clause[:op], clause[op+1:]
		if who == "" || who == "a" {
			who = "ugo"
		}
		var bits os.FileMode
		for _, c := range perms {
			switch c {
			case 'r':
				bits |= 0o444
			case 'w':
				bits |= 0o222
			case 'x':
				bits |= 0o111
			}
		}
		mask := os.FileMode(0)
		for _, c := range who {
			switch c {
			case 'u':
				mask |= 0o700
			case 'g':
				mask |= 0o070
			case 'o':
				mask |= 0o007
			}
		}
		want |= bits & mask
	}
	return perm&want == want
}

// pathBase returns the basename of a find display path ("." for roots
// like "." or "./").
func pathBase(display string) string {
	d := strings.TrimSuffix(display, "/")
	if d == "." || d == "" {
		return "."
	}
	if i := strings.LastIndex(d, "/"); i >= 0 {
		return d[i+1:]
	}
	return d
}

// killErrText maps OS kill errors to GNU-style diagnostics.
func killErrText(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "already finished") || strings.Contains(msg, "not found") ||
		strings.Contains(msg, "OpenProcess") || strings.Contains(msg, "parameter is incorrect") ||
		strings.Contains(msg, "Access is denied") {
		return "No such process"
	}
	return msg
}
