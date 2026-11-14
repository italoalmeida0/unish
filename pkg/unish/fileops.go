package unish

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	extraCommands = append(extraCommands,
		extraCmd{"cp", cmdCp},
		extraCmd{"mv", cmdMv},
		extraCmd{"rm", cmdRm},
		extraCmd{"del", cmdDel},
		extraCmd{"erase", cmdDel},
		extraCmd{"dir", cmdDir},
		extraCmd{"mkdir", cmdMkdir},
		extraCmd{"touch", cmdTouch},
		extraCmd{"chmod", cmdChmod},
		extraCmd{"xargs", cmdXargs},
		extraCmd{"base64", cmdBase64},
		extraCmd{"tar", cmdTar},
		extraCmd{"gzip", cmdGzip},
		extraCmd{"gunzip", cmdGunzip},
		extraCmd{"gzcat", cmdGzcat},
		extraCmd{"zcat", cmdGzcat},
	)
}

func cmdCp(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("cp", hc.Stderr)
	rec := fs.Bool("r", false, "")
	fs.BoolVar(rec, "R", false, "")
	fs.BoolVar(rec, "recursive", false, "")
	force := fs.Bool("f", false, "")
	fs.BoolVar(force, "force", false, "")
	verb := fs.Bool("v", false, "")
	fs.BoolVar(verb, "verbose", false, "")
	noClobber := fs.Bool("n", false, "")
	fs.BoolVar(noClobber, "no-clobber", false, "")
	noDeref := fs.Bool("P", false, "")
	fs.BoolVar(noDeref, "no-dereference", false, "")
	fs.BoolVar(noDeref, "d", false, "")
	archive := fs.Bool("a", false, "")
	fs.BoolVar(archive, "archive", false, "")
	preserve := fs.Bool("p", false, "")
	fs.BoolVar(preserve, "preserve", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// GNU cp -a (archive) is -dR --preserve=all: recursive, keeping
	// symlinks and preserving mode/timestamps.
	if *archive {
		*rec, *preserve, *noDeref = true, true, true
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "cp: missing destination")
		return flag.ErrHelp
	}
	dstArg := rest[len(rest)-1]
	dst := resolveHostPath(hc.Dir, dstArg)
	srcs := rest[:len(rest)-1]
	dstFi, dstErr := fsStat(ctx, hc.Dir, "cp", dstArg)
	dstIsDir := dstErr == nil && dstFi.IsDir()
	if len(srcs) > 1 && !dstIsDir {
		fmt.Fprintln(hc.Stderr, "cp: target is not a directory")
		return exitError{1}
	}
	// GNU cp -n prints a portability warning to stderr (once per invocation).
	if *noClobber {
		fmt.Fprintln(hc.Stderr, "cp: warning: behavior of -n is non-portable and may change in future; use --update=none instead")
	}
	code := 0
	for _, s := range srcs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		src := resolveHostPath(hc.Dir, s)
		target := dst
		targetSpec := dstArg
		if dstIsDir {
			base := filepath.Base(src)
			target = filepath.Join(dst, base)
			targetSpec = filepath.Join(dstArg, base)
		}
		// GNU: "cp: 'a' and 'b' are the same file" (exit 1).
		if sameFile(ctx, hc.Dir, "cp", s, targetSpec) {
			fmt.Fprintf(hc.Stderr, "cp: '%s' and '%s' are the same file\n", s, dstArg)
			code = 1
			continue
		}
		if lfi, lerr := fsLstat(ctx, hc.Dir, "cp", s); lerr == nil {
			srcIsDir := lfi.IsDir()
			if !srcIsDir && !*noDeref {
				if sfi, serr := fsStat(ctx, hc.Dir, "cp", s); serr == nil && sfi.IsDir() {
					srcIsDir = true
				}
			}
			if srcIsDir && *rec && isSameOrSubdir(ctx, hc.Dir, "cp", s, targetSpec) {
				fmt.Fprintf(hc.Stderr, "cp: cannot copy a directory, '%s', into itself, '%s'\n", s, target)
				code = 1
				continue
			}
		}
		if *noClobber {
			if _, err := fsLstat(ctx, hc.Dir, "cp", targetSpec); err == nil {
				continue
			}
		}
		if err := copyOne(ctx, hc.Dir, s, targetSpec, src, target, *rec, *force, *noDeref, *preserve); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(hc.Stderr, "cp: %v\n", err)
			code = 1
			continue
		}
		if *verb {
			fmt.Fprintf(hc.Stdout, "'%s' -> '%s'\n", s, target)
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func copyOne(ctx context.Context, dir, srcSpec, dstSpec, srcFull, dstFull string, rec, force, noDeref, preserve bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fi, err := fsLstat(ctx, dir, "cp", srcSpec)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if !rec {
			return fmt.Errorf("%s is a directory (use -r)", srcFull)
		}
		return copyDir(ctx, dir, srcSpec, dstSpec, srcFull, dstFull, force, preserve)
	}
	if fi.Mode()&os.ModeSymlink != 0 && (noDeref || rec) {
		link, err := fsReadlink(ctx, dir, "cp", srcSpec)
		if err != nil {
			return err
		}
		if force {
			if rerr := fsRemove(ctx, dir, "cp", dstSpec); rerr != nil && !os.IsNotExist(rerr) && errors.Is(rerr, os.ErrPermission) {
				return rerr
			}
		}
		return fsSymlink(ctx, dir, "cp", link, dstSpec)
	}
	// GNU default: follow the link, copying content into a new file.
	st, err := fsStat(ctx, dir, "cp", srcSpec)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if !rec {
			return fmt.Errorf("%s is a directory (use -r)", srcFull)
		}
		return copyDir(ctx, dir, srcSpec, dstSpec, srcFull, dstFull, force, preserve)
	}
	return copyFile(ctx, dir, srcSpec, dstSpec, srcFull, dstFull, st.Mode(), force, preserve)
}

// preserveAttrs implements cp -p: copy mode and timestamps best-effort
// (Windows cannot represent full POSIX modes; failures are non-fatal).
func preserveAttrs(ctx context.Context, dir, srcSpec, dstSpec string) {
	fi, err := fsStat(ctx, dir, "cp", srcSpec)
	if err != nil {
		return
	}
	_ = fsChmod(ctx, dir, "cp", dstSpec, fi.Mode().Perm())
	mt := fi.ModTime()
	_ = fsChtimes(ctx, dir, "cp", dstSpec, mt, mt)
}

func copyFile(ctx context.Context, dir, srcSpec, dstSpec, srcFull, dstFull string, mode os.FileMode, force, preserve bool) error {
	if sameFile(ctx, dir, "cp", srcSpec, dstSpec) {
		return fmt.Errorf("'%s' and '%s' are the same file", srcFull, dstFull)
	}
	in, err := fsOpen(ctx, dir, "cp", srcSpec)
	if err != nil {
		return err
	}
	defer in.Close()
	if force {
		if runtime.GOOS == "windows" {
			_ = fsChmod(ctx, dir, "cp", dstSpec, 0o666)
		}
		if rerr := fsRemove(ctx, dir, "cp", dstSpec); rerr != nil && !os.IsNotExist(rerr) && errors.Is(rerr, os.ErrPermission) {
			return rerr
		}
	}
	out, err := fsOpenFile(ctx, dir, "cp", dstSpec, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	cerr := out.Close()
	if preserve {
		preserveAttrs(ctx, dir, srcSpec, dstSpec)
	}
	if err != nil {
		return err
	}
	return cerr
}

func copyDir(ctx context.Context, dir, srcSpec, dstSpec, srcFull, dstFull string, force, preserve bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fi, err := fsStat(ctx, dir, "cp", srcSpec)
	if err != nil {
		return err
	}
	if err := fsMkdirAll(ctx, dir, "cp", dstSpec, fi.Mode().Perm()); err != nil {
		return err
	}
	entries, err := fsReadDir(ctx, dir, "cp", srcSpec)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sSpec := filepath.Join(srcSpec, e.Name())
		dSpec := filepath.Join(dstSpec, e.Name())
		sFull := filepath.Join(srcFull, e.Name())
		dFull := filepath.Join(dstFull, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			link, err := fsReadlink(ctx, dir, "cp", sSpec)
			if err != nil {
				return err
			}
			if dfi, derr := fsLstat(ctx, dir, "cp", dSpec); derr == nil {
				if dfi.IsDir() {
					return fmt.Errorf("cannot overwrite directory '%s' with non-directory", dFull)
				}
				if sameFile(ctx, dir, "cp", sSpec, dSpec) {
					return fmt.Errorf("'%s' and '%s' are the same file", sFull, dFull)
				}
				if rerr := fsRemove(ctx, dir, "cp", dSpec); rerr != nil && !os.IsNotExist(rerr) {
					return rerr
				}
			}
			if err := fsSymlink(ctx, dir, "cp", link, dSpec); err != nil {
				return err
			}
			continue
		}
		if e.IsDir() {
			if err := copyDir(ctx, dir, sSpec, dSpec, sFull, dFull, force, preserve); err != nil {
				return err
			}
			continue
		}
		efi, err := e.Info()
		if err != nil {
			return err
		}
		if err := copyFile(ctx, dir, sSpec, dSpec, sFull, dFull, efi.Mode(), force, preserve); err != nil {
			return err
		}
	}
	if preserve {
		preserveAttrs(ctx, dir, srcSpec, dstSpec)
	}
	return nil
}

func cmdMv(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("mv", hc.Stderr)
	force := fs.Bool("f", false, "")
	update := fs.Bool("u", false, "")
	fs.BoolVar(update, "update", false, "")
	noClobber := fs.Bool("n", false, "")
	fs.BoolVar(noClobber, "no-clobber", false, "")
	verb := fs.Bool("v", false, "")
	fs.BoolVar(verb, "verbose", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "mv: missing destination")
		return flag.ErrHelp
	}
	dstArg := rest[len(rest)-1]
	dst := resolveHostPath(hc.Dir, dstArg)
	srcs := rest[:len(rest)-1]
	dstFi, dstErr := fsStat(ctx, hc.Dir, "mv", dstArg)
	dstIsDir := dstErr == nil && dstFi.IsDir()
	if len(srcs) > 1 && !dstIsDir {
		fmt.Fprintln(hc.Stderr, "mv: target is not a directory")
		return exitError{1}
	}
	code := 0
	for _, s := range srcs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		src := resolveHostPath(hc.Dir, s)
		srcFi, serr := fsLstat(ctx, hc.Dir, "mv", s)
		if serr != nil {
			if os.IsNotExist(serr) {
				fmt.Fprintf(hc.Stderr, "mv: cannot stat '%s': No such file or directory\n", s)
			} else {
				fmt.Fprintf(hc.Stderr, "mv: cannot stat '%s': %v\n", s, serr)
			}
			code = 1
			continue
		}
		target := dst
		targetSpec := dstArg
		if dstIsDir {
			// Guard `mv dir dir` when dst == src or case-only dir rename.
			if len(srcs) == 1 && isCaseOnlyRename(ctx, hc.Dir, "mv", s, dstArg) {
				target = dst
				targetSpec = dstArg
			} else {
				base := filepath.Base(src)
				target = filepath.Join(dst, base)
				targetSpec = filepath.Join(dstArg, base)
			}
		}
		caseRename := isCaseOnlyRename(ctx, hc.Dir, "mv", s, targetSpec)
		if !caseRename && *noClobber {
			if _, err := fsLstat(ctx, hc.Dir, "mv", targetSpec); err == nil {
				continue
			}
		}
		if !caseRename && *update {
			if sfi, serr := fsStat(ctx, hc.Dir, "mv", s); serr == nil {
				if dfi, derr := fsStat(ctx, hc.Dir, "mv", targetSpec); derr == nil && !sfi.ModTime().After(dfi.ModTime()) {
					continue
				}
			}
		}
		// GNU: "mv: 'a' and 'b' are the same file" (exit 1).
		if !caseRename && sameFile(ctx, hc.Dir, "mv", s, targetSpec) {
			fmt.Fprintf(hc.Stderr, "mv: '%s' and '%s' are the same file\n", s, dstArg)
			code = 1
			continue
		}
		if srcFi.IsDir() && !caseRename && isSameOrSubdir(ctx, hc.Dir, "mv", s, targetSpec) {
			fmt.Fprintf(hc.Stderr, "mv: cannot move '%s' to a subdirectory of itself, '%s'\n", s, target)
			code = 1
			continue
		}
		if CapabilityHookFromContext(ctx) != nil {
			if err := checkMvCapability(ctx, hc.Dir, s, targetSpec, srcFi); err != nil {
				fmt.Fprintf(hc.Stderr, "mv: %v\n", err)
				code = 1
				continue
			}
		}
		if !caseRename {
			if tfi, terr := fsLstat(ctx, hc.Dir, "mv", targetSpec); terr == nil {
				if srcFi.IsDir() {
					if !tfi.IsDir() {
						fmt.Fprintf(hc.Stderr, "mv: cannot overwrite non-directory '%s' with directory '%s'\n", target, s)
						code = 1
						continue
					}
					entries, rerr := fsReadDir(ctx, hc.Dir, "mv", targetSpec)
					if rerr != nil {
						fmt.Fprintf(hc.Stderr, "mv: %v\n", rerr)
						code = 1
						continue
					}
					if len(entries) > 0 {
						fmt.Fprintf(hc.Stderr, "mv: cannot move '%s' to '%s': Directory not empty\n", s, target)
						code = 1
						continue
					}
					if len(entries) == 0 && runtime.GOOS == "windows" {
						_ = fsRemove(ctx, hc.Dir, "mv", targetSpec)
					}
				} else {
					if tfi.IsDir() {
						fmt.Fprintf(hc.Stderr, "mv: cannot overwrite directory '%s' with non-directory '%s'\n", target, s)
						code = 1
						continue
					}
					if *force && runtime.GOOS == "windows" {
						_ = fsChmod(ctx, hc.Dir, "mv", targetSpec, 0o666)
					}
				}
			} else if errors.Is(terr, os.ErrPermission) {
				fmt.Fprintf(hc.Stderr, "mv: %v\n", terr)
				code = 1
				continue
			}
		}
		if err := fsRename(ctx, hc.Dir, "mv", s, targetSpec); err != nil {
			if !isCrossDevice(err) {
				fmt.Fprintf(hc.Stderr, "mv: %v\n", err)
				code = 1
				continue
			}
			if err := moveCrossDevice(ctx, hc.Dir, s, targetSpec, src, target, srcFi); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				fmt.Fprintf(hc.Stderr, "mv: %v\n", err)
				code = 1
				continue
			}
		}
		if *verb {
			dispDst := dstArg
			if dstIsDir && !caseRename {
				dispDst = dispDst + "/" + filepath.Base(s)
			}
			fmt.Fprintf(hc.Stdout, "renamed '%s' -> '%s'\n", s, dispDst)
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdRm(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("rm", hc.Stderr)
	rec := fs.Bool("r", false, "")
	fs.BoolVar(rec, "R", false, "")
	fs.BoolVar(rec, "recursive", false, "")
	force := fs.Bool("f", false, "")
	fs.BoolVar(force, "force", false, "")
	verb := fs.Bool("v", false, "")
	fs.BoolVar(verb, "verbose", false, "")
	noPreserve := fs.Bool("no-preserve-root", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		if *force {
			return nil
		}
		fmt.Fprintln(hc.Stderr, "rm: missing operand")
		fmt.Fprintln(hc.Stderr, "Try 'rm --help' for more information.")
		return exitError{1}
	}
	// GNU --preserve-root (default): refuse `rm -rf /`.
	for _, p := range rest {
		if *rec && !*noPreserve && isRootPath(hc.Dir, p) {
			fmt.Fprintln(hc.Stderr, "rm: it is dangerous to operate recursively on '/'")
			fmt.Fprintln(hc.Stderr, "rm: use --no-preserve-root to override this failsafe")
			return exitError{1}
		}
	}
	code := 0
	for _, p := range rest {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// GNU: never remove '.' or '..', under ANY spelling ('.', './',
		// 'sub/..', '/abs/path/.', ...), even with --no-preserve-root.
		// Only the operand spelling matters: 'rm -rf /abs/cwd' is
		// allowed while 'rm -rf .' is refused for the same directory.
		if isDotOperand(p) {
			fmt.Fprintf(hc.Stderr, "rm: refusing to remove '.' or '..' directory: skipping %q\n", p)
			code = 1
			continue
		}
		fi, err := fsLstat(ctx, hc.Dir, "rm", p)
		if err != nil {
			if *force && os.IsNotExist(err) {
				continue
			}
			if os.IsNotExist(err) {
				fmt.Fprintf(hc.Stderr, "rm: cannot remove '%s': No such file or directory\n", p)
			} else {
				fmt.Fprintf(hc.Stderr, "rm: cannot remove '%s': %v\n", p, err)
			}
			code = 1
			continue
		}
		if fi.IsDir() {
			if !*rec {
				fmt.Fprintf(hc.Stderr, "rm: cannot remove '%s': Is a directory\n", p)
				code = 1
				continue
			}
			err = fsRemoveTree(ctx, hc.Dir, "rm", p)
		} else {
			err = fsRemove(ctx, hc.Dir, "rm", p)
			if err != nil && runtime.GOOS == "windows" && !errors.Is(err, os.ErrPermission) && fi.Mode()&os.ModeSymlink == 0 {
				_ = fsChmod(ctx, hc.Dir, "rm", p, 0o666)
				err = fsRemove(ctx, hc.Dir, "rm", p)
			}
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "rm: cannot remove '%s': %v\n", p, err)
			code = 1
			continue
		}
		if *verb {
			fmt.Fprintf(hc.Stdout, "removed '%s'\n", p)
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

// cmdDel implements the cmd.exe DEL/ERASE command for AX parity on
// Windows (agents coming from cmd habitually type `del file`).
// Mapping: /S -> -r (recurse), /Q /F -> -f (quiet/force), /P -> noop
// (rm never prompts non-interactively anyway). /A (attribute filter)
// changes WHICH files are deleted, so it is refused loudly instead of
// silently deleting the wrong set. Long GNU flags (-r -f) are accepted
// too. Everything else delegates to cmdRm, including dot/root guards.
func cmdDel(ctx context.Context, hc interp.HandlerContext, args []string) error {
	norm := make([]string, 0, len(args))
	norm = append(norm, "rm")
	for _, a := range args[1:] {
		if len(a) == 2 && (a[0] == '/' || a[0] == '\\') {
			switch a[1] {
			case 'S', 's':
				norm = append(norm, "-r")
				continue
			case 'Q', 'q', 'F', 'f':
				norm = append(norm, "-f")
				continue
			case 'P', 'p':
				continue // no prompt in non-interactive shells
			case 'A', 'a':
				fmt.Fprintln(hc.Stderr, "del: /A (attribute filter) is not supported; use rm with explicit paths")
				return exitError{1}
			}
		}
		if strings.HasPrefix(a, "/A:") || strings.HasPrefix(a, "/a:") {
			fmt.Fprintln(hc.Stderr, "del: /A (attribute filter) is not supported; use rm with explicit paths")
			return exitError{1}
		}
		norm = append(norm, a)
	}
	return cmdRm(ctx, hc, norm)
}

// cmdDir implements the cmd.exe DIR command for AX parity on Windows
// (bare `dir` / `dir /b` / `dir /s` from cmd-habituated agents).
// Mapping: /B -> -1 (bare names), /S -> -R (recurse), /A -> -a (all),
// /W -> -C (wide/columns). Display-only switches (/O, /L, /P, /Q, /T,
// /D, /N, /X, /R, /C, /4 and their :args forms) are accepted and
// ignored: same file set, only layout differs. Delegates to cmdLs.
func cmdDir(ctx context.Context, hc interp.HandlerContext, args []string) error {
	norm := make([]string, 0, len(args))
	norm = append(norm, "ls")
	for _, a := range args[1:] {
		up := strings.ToUpper(a)
		if len(a) >= 2 && (a[0] == '/' || a[0] == '\\') {
			letter := up[1]
			rest := ""
			if idx := strings.IndexByte(a, ':'); idx >= 0 {
				rest = a[idx:]
			}
			switch letter {
			case 'B':
				norm = append(norm, "-1")
				continue
			case 'S':
				norm = append(norm, "-R")
				continue
			case 'A':
				if rest != "" {
					// /A:attrs (e.g. /A:D, /A:-H): attribute filter,
					// same rationale as del /A: refuse loudly.
					fmt.Fprintln(hc.Stderr, "dir: /A:attrs (attribute filter) is not supported")
					return exitError{1}
				}
				norm = append(norm, "-a")
				continue
			case 'W':
				norm = append(norm, "-C")
				continue
			case 'O', 'L', 'P', 'Q', 'T', 'D', 'N', 'X', 'R', 'C':
				continue // display-only: same file set
			case '4':
				continue // /4 (four-digit year): noop
			}
		}
		norm = append(norm, a)
	}
	return cmdLs(ctx, hc, norm)
}

func cmdMkdir(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("mkdir", hc.Stderr)
	parents := fs.Bool("p", false, "")
	verb := fs.Bool("v", false, "")
	fs.BoolVar(verb, "verbose", false, "")
	modeStr := fs.String("m", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		fmt.Fprintln(hc.Stderr, "mkdir: missing operand")
		return flag.ErrHelp
	}
	mode := os.FileMode(0o777)
	if *modeStr != "" {
		if m, err := strconv.ParseUint(*modeStr, 8, 32); err == nil {
			mode = os.FileMode(m)
		}
	}
	code := 0
	for _, p := range fs.Args() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var err error
		if *parents {
			err = fsMkdirAll(ctx, hc.Dir, "mkdir", p, mode)
		} else {
			err = fsMkdir(ctx, hc.Dir, "mkdir", p, mode)
		}
		if err != nil {
			if *parents && !errors.Is(err, os.ErrPermission) {
				if sfi, serr := fsStat(ctx, hc.Dir, "mkdir", p); serr == nil && sfi.IsDir() {
					continue
				}
			}
			// GNU wording: "mkdir: cannot create directory 'd': File exists".
			if os.IsExist(err) {
				fmt.Fprintf(hc.Stderr, "mkdir: cannot create directory \u2018%s\u2019: File exists\n", p)
			} else {
				fmt.Fprintf(hc.Stderr, "mkdir: cannot create directory '%s': %v\n", p, err)
			}
			code = 1
			continue
		}
		if *verb {
			fmt.Fprintf(hc.Stdout, "mkdir: created directory '%s'\n", p)
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdTouch(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("touch", hc.Stderr)
	noCreate := fs.Bool("c", false, "")
	fs.BoolVar(noCreate, "no-create", false, "")
	dateStr := fs.String("d", "", "")
	fs.StringVar(dateStr, "date", "", "")
	refFile := fs.String("r", "", "")
	fs.StringVar(refFile, "reference", "", "")
	mtimeOnly := fs.Bool("m", false, "")
	_ = mtimeOnly // -m: only modify mtime; we always set both (atime==mtime), accepted for parity.
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		fmt.Fprintln(hc.Stderr, "touch: missing operand")
		return flag.ErrHelp
	}
	mtime := time.Now()
	if *refFile != "" {
		fi, err := fsStat(ctx, hc.Dir, "touch", *refFile)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(hc.Stderr, "touch: failed to get attributes of '%s': No such file or directory\n", *refFile)
			} else {
				fmt.Fprintf(hc.Stderr, "touch: failed to get attributes of '%s': %v\n", *refFile, err)
			}
			return exitError{1}
		}
		mtime = fi.ModTime()
	}
	if *dateStr != "" {
		t, err := parseTouchDate(*dateStr)
		if err != nil {
			// GNU wording: "touch: invalid date format '...'".
			fmt.Fprintf(hc.Stderr, "touch: invalid date format \u2018%s\u2019\n", *dateStr)
			return exitError{1}
		}
		mtime = t
	}
	code := 0
	for _, p := range fs.Args() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := fsStat(ctx, hc.Dir, "touch", p); err != nil {
			if errors.Is(err, os.ErrPermission) {
				fmt.Fprintf(hc.Stderr, "touch: cannot touch '%s': %v\n", p, err)
				code = 1
				continue
			}
			if *noCreate && os.IsNotExist(err) {
				continue
			}
			f, err := fsOpenFile(ctx, hc.Dir, "touch", p, os.O_CREATE|os.O_WRONLY, 0o666)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "touch: cannot touch '%s': %v\n", p, err)
				code = 1
				continue
			}
			f.Close()
		}
		if err := fsChtimes(ctx, hc.Dir, "touch", p, mtime, mtime); err != nil {
			fmt.Fprintf(hc.Stderr, "touch: cannot touch '%s': %v\n", p, err)
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdChmod(ctx context.Context, hc interp.HandlerContext, args []string) error {
	rec := false
	verb := false
	quiet := false
	var rest []string
	endFlags := false
	for _, a := range args[1:] {
		if endFlags || len(rest) > 0 {
			rest = append(rest, a)
			continue
		}
		switch a {
		case "--":
			endFlags = true
		case "-R", "--recursive":
			rec = true
		case "-v", "--verbose", "-c", "--changes":
			verb = true
		case "-f", "--silent", "--quiet":
			quiet = true
		case "--help":
			return flag.ErrHelp
		default:
			if strings.HasPrefix(a, "-") && !isChmodSymbolicMode(a) {
				fs := newFlagSet("chmod", hc.Stderr)
				return fs.Parse([]string{a})
			}
			rest = append(rest, a)
		}
	}
	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "chmod: missing operand")
		return flag.ErrHelp
	}
	modeStr, paths := rest[0], rest[1:]
	apply := func(rawPath, display string) error {
		mode, err := parseChmodMode(ctx, hc.Dir, modeStr, rawPath)
		if err != nil {
			return err
		}
		if err := fsChmod(ctx, hc.Dir, "chmod", rawPath, mode); err != nil {
			return err
		}
		if verb {
			fmt.Fprintf(hc.Stdout, "mode of '%s' changed to %04o\n", display, mode.Perm())
		}
		return nil
	}
	reportErr := func(p string, err error) {
		if quiet {
			return
		}
		msg := err.Error()
		if strings.HasPrefix(msg, "invalid mode") {
			fmt.Fprintf(hc.Stderr, "chmod: invalid mode: \u2018%s\u2019\n", modeStr)
			fmt.Fprintln(hc.Stderr, "Try 'chmod --help' for more information.")
		} else if os.IsNotExist(err) {
			fmt.Fprintf(hc.Stderr, "chmod: cannot access '%s': No such file or directory\n", p)
		} else {
			fmt.Fprintf(hc.Stderr, "chmod: %v\n", err)
		}
	}
	code := 0
	var walkChmod func(rawPath, display string, isRoot bool) error
	walkChmod = func(rawPath, display string, isRoot bool) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info, err := fsLstat(ctx, hc.Dir, "chmod", rawPath)
		if err != nil {
			reportErr(display, err)
			code = 1
			return nil
		}
		// POSIX/GNU chmod -R skips symlinks encountered during directory traversal.
		if !isRoot && info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if e := apply(rawPath, display); e != nil {
			reportErr(display, e)
			code = 1
		}
		if !info.IsDir() {
			return nil
		}
		entries, err := fsReadDir(ctx, hc.Dir, "chmod", rawPath)
		if err != nil {
			reportErr(display, err)
			code = 1
			return nil
		}
		for _, ent := range entries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			childRaw := filepath.Join(rawPath, ent.Name())
			childDisp := filepath.Join(display, ent.Name())
			if display == "." {
				childDisp = ent.Name()
			}
			if err := walkChmod(childRaw, childDisp, false); err != nil {
				return err
			}
		}
		return nil
	}
	for _, p := range paths {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rec {
			disp := p
			if filepath.IsAbs(p) && hc.Dir != "" {
				if rel, rerr := filepath.Rel(hc.Dir, p); rerr == nil && !strings.HasPrefix(rel, "..") {
					disp = rel
				}
			}
			if walkErr := walkChmod(p, disp, true); walkErr != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if err := apply(p, p); err != nil {
			reportErr(p, err)
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func isChmodSymbolicMode(s string) bool {
	if s == "" {
		return false
	}
	for _, clause := range strings.Split(s, ",") {
		op := strings.IndexAny(clause, "+-=")
		if op < 0 {
			return false
		}
		who, perms := clause[:op], clause[op+1:]
		if perms == "" {
			return false
		}
		for _, c := range who {
			if c != 'u' && c != 'g' && c != 'o' && c != 'a' {
				return false
			}
		}
		for _, c := range perms {
			switch c {
			case 'r', 'w', 'x', 'X', 's', 't', 'u', 'g', 'o', '+', '-', '=':
			default:
				return false
			}
		}
	}
	return true
}

func parseChmodMode(ctx context.Context, dir, s, rawPath string) (os.FileMode, error) {
	if m, err := strconv.ParseUint(s, 8, 32); err == nil {
		return os.FileMode(m), nil
	}
	var cur os.FileMode
	isDir := false
	if fi, err := fsStat(ctx, dir, "chmod", rawPath); err == nil {
		cur = fi.Mode()
		isDir = fi.IsDir()
	} else if errors.Is(err, os.ErrPermission) {
		return 0, err
	} else {
		cur = 0o644
	}
	for _, clause := range strings.Split(s, ",") {
		op := strings.IndexAny(clause, "+-=")
		if op < 0 {
			return 0, fmt.Errorf("invalid mode '%s'", s)
		}
		who, action, perms := clause[:op], clause[op], clause[op+1:]
		if who == "" || who == "a" {
			who = "ugo"
		}
		var bits os.FileMode
		var specialBits os.FileMode
		for _, c := range perms {
			switch c {
			case 'r':
				bits |= 0o444
			case 'w':
				bits |= 0o222
			case 'x':
				bits |= 0o111
			case 'X':
				if isDir || cur&0o111 != 0 {
					bits |= 0o111
				}
			case 's':
				if strings.ContainsRune(who, 'u') {
					specialBits |= os.ModeSetuid
				}
				if strings.ContainsRune(who, 'g') {
					specialBits |= os.ModeSetgid
				}
			case 't':
				specialBits |= os.ModeSticky
			case 'u':
				p := (cur & 0o700) >> 6
				bits |= p | (p << 3) | (p << 6)
			case 'g':
				p := (cur & 0o070) >> 3
				bits |= p | (p << 3) | (p << 6)
			case 'o':
				p := cur & 0o007
				bits |= p | (p << 3) | (p << 6)
			default:
				return 0, fmt.Errorf("invalid mode '%s'", s)
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
			default:
				return 0, fmt.Errorf("invalid mode '%s'", s)
			}
		}
		bits = (bits & mask) | specialBits
		clearMask := mask
		if action == '=' {
			if strings.ContainsRune(who, 'u') {
				clearMask |= os.ModeSetuid
			}
			if strings.ContainsRune(who, 'g') {
				clearMask |= os.ModeSetgid
			}
			if strings.ContainsRune(who, 'o') {
				clearMask |= os.ModeSticky
			}
		}
		switch action {
		case '+':
			cur |= bits
		case '-':
			cur &^= bits
		case '=':
			cur = (cur &^ clearMask) | bits
		}
	}
	return cur, nil
}

func cmdXargs(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("xargs", hc.Stderr)
	maxArgs := fs.Int("n", 0, "")
	trace := fs.Bool("t", false, "")
	nullDelim := fs.Bool("0", false, "")
	replace := fs.String("I", "", "")
	noRun := fs.Bool("r", false, "")
	fs.BoolVar(noRun, "no-run-if-empty", false, "")
	maxProcs := fs.Int("P", 1, "")
	fs.IntVar(maxProcs, "max-procs", 1, "")
	delim := fs.String("d", "", "")
	fs.StringVar(delim, "delimiter", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *maxProcs < 0 {
		fmt.Fprintln(hc.Stderr, "xargs: max-procs must be non-negative")
		return exitError{1}
	}
	rest := fs.Args()
	if len(rest) == 0 {
		rest = []string{"echo"}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var data []byte
	if hc.Stdin != nil {
		var err error
		data, err = io.ReadAll(hc.Stdin)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(hc.Stderr, "xargs:", err)
			return exitError{1}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var items []string
	if *nullDelim {
		for _, part := range strings.Split(string(data), "\x00") {
			if part != "" {
				items = append(items, part)
			}
		}
	} else if *delim != "" {
		d := unescapeXargsDelim(*delim)
		if d == "" {
			d = "\x00"
		}
		s := string(data)
		// Strip a single trailing delimiter before splitting (e.g. final '\n').
		s = strings.TrimSuffix(s, d)
		if s != "" {
			for _, part := range strings.Split(s, d) {
				if d == "\n" {
					part = strings.TrimSuffix(part, "\r")
				}
				items = append(items, part)
			}
		}
	} else if *replace != "" {
		for _, part := range strings.Split(string(data), "\n") {
			part = strings.TrimRight(part, "\r")
			if part != "" {
				items = append(items, part)
			}
		}
	} else {
		var qerr error
		items, qerr = parseXargsItems(string(data))
		if qerr != nil {
			fmt.Fprintln(hc.Stderr, "xargs:", qerr)
			return exitError{1}
		}
	}

	childHC := hc
	childHC.Stdin = bytes.NewReader(nil)

	if len(items) == 0 {
		// GNU xargs runs the command ONCE with no arguments on empty
		// input, unless -r/--no-run-if-empty is given.
		if *noRun {
			return nil
		}
		if *trace {
			fmt.Fprintf(hc.Stderr, "+ %s\n", strings.Join(rest, " "))
		}
		if err := runSubcommandNamed(ctx, childHC, "xargs", rest); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isExecNotFound(err) {
				fmt.Fprintf(hc.Stderr, "xargs: %s: No such file or directory\n", rest[0])
				return exitError{127}
			}
			if errors.Is(err, os.ErrPermission) {
				fmt.Fprintf(hc.Stderr, "xargs: %s: %v\n", rest[0], err)
				return exitError{126}
			}
			if ec, ok := subcommandExitCode(err); ok && ec == 255 {
				return exitError{124}
			}
			return exitError{123}
		}
		return nil
	}

	batch := *maxArgs
	if *replace != "" && batch <= 0 {
		batch = 1
	}
	if batch <= 0 {
		batch = len(items)
	}
	var batches [][]string
	for i := 0; i < len(items); i += batch {
		end := i + batch
		if end > len(items) {
			end = len(items)
		}
		chunk := items[i:end]
		var cmdArgs []string
		if *replace != "" {
			cmdArgs = make([]string, len(rest))
			joined := strings.Join(chunk, " ")
			for k, a := range rest {
				cmdArgs[k] = strings.ReplaceAll(a, *replace, joined)
			}
		} else {
			cmdArgs = make([]string, 0, len(rest)+len(chunk))
			cmdArgs = append(cmdArgs, rest...)
			cmdArgs = append(cmdArgs, chunk...)
		}
		batches = append(batches, cmdArgs)
	}

	workers := *maxProcs
	if workers == 0 || workers > 64 {
		workers = 64
	}
	if workers > len(batches) {
		workers = len(batches)
	}

	if workers <= 1 {
		code := 0
		for _, cmdArgs := range batches {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if *trace {
				fmt.Fprintf(hc.Stderr, "+ %s\n", strings.Join(cmdArgs, " "))
			}
			if err := runSubcommandNamed(ctx, childHC, "xargs", cmdArgs); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if isExecNotFound(err) {
					fmt.Fprintf(hc.Stderr, "xargs: %s: No such file or directory\n", cmdArgs[0])
					return exitError{127}
				}
				if errors.Is(err, os.ErrPermission) {
					fmt.Fprintf(hc.Stderr, "xargs: %s: %v\n", cmdArgs[0], err)
					return exitError{126}
				}
				if ec, ok := subcommandExitCode(err); ok && ec == 255 {
					// GNU xargs aborts when a command exits 255.
					return exitError{124}
				}
				code = 123
			}
		}
		if code != 0 {
			return exitError{code}
		}
		return nil
	}

	batchCtx, cancelBatches := context.WithCancel(ctx)
	defer cancelBatches()

	outSync := &syncWriter{w: hc.Stdout}
	errSync := &syncWriter{w: hc.Stderr}

	jobs := make(chan []string, len(batches))
	for _, b := range batches {
		jobs <- b
	}
	close(jobs)

	var (
		resMu     sync.Mutex
		finalCode int
		wg        sync.WaitGroup
	)
	recordCode := func(c int) {
		resMu.Lock()
		defer resMu.Unlock()
		// Fatal xargs codes (126/127/124) outrank normal batch failure (123).
		if finalCode == 0 || (finalCode == 123 && (c == 124 || c == 126 || c == 127)) {
			finalCode = c
		}
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cmdArgs := range jobs {
				if batchCtx.Err() != nil {
					return
				}
				wOut := &lineSyncWriter{dst: outSync}
				wErr := &lineSyncWriter{dst: errSync}
				if *trace {
					fmt.Fprintf(wErr, "+ %s\n", strings.Join(cmdArgs, " "))
				}
				wHC := childHC
				wHC.Stdout = wOut
				wHC.Stderr = wErr
				err := runSubcommandNamed(batchCtx, wHC, "xargs", cmdArgs)
				_ = wOut.Flush()
				_ = wErr.Flush()
				if err != nil {
					if ctx.Err() != nil {
						cancelBatches()
						return
					}
					if batchCtx.Err() != nil {
						return
					}
					if isExecNotFound(err) {
						fmt.Fprintf(errSync, "xargs: %s: No such file or directory\n", cmdArgs[0])
						recordCode(127)
						cancelBatches()
						return
					}
					if errors.Is(err, os.ErrPermission) {
						fmt.Fprintf(errSync, "xargs: %s: %v\n", cmdArgs[0], err)
						recordCode(126)
						cancelBatches()
						return
					}
					if ec, ok := subcommandExitCode(err); ok && ec == 255 {
						recordCode(124)
						cancelBatches()
						return
					}
					recordCode(123)
				}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if finalCode != 0 {
		return exitError{finalCode}
	}
	return nil
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	if s.w == nil {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

type lineSyncWriter struct {
	dst *syncWriter
	buf bytes.Buffer
}

func (l *lineSyncWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			l.buf.Write(p)
			break
		}
		l.buf.Write(p[:idx+1])
		if _, err := l.dst.Write(l.buf.Bytes()); err != nil {
			l.buf.Reset()
			return 0, err
		}
		l.buf.Reset()
		p = p[idx+1:]
	}
	return total, nil
}

func (l *lineSyncWriter) Flush() error {
	if l.buf.Len() == 0 {
		return nil
	}
	_, err := l.dst.Write(l.buf.Bytes())
	l.buf.Reset()
	return err
}

func unescapeXargsDelim(s string) string {
	if !strings.HasPrefix(s, `\`) || len(s) < 2 {
		return s
	}
	switch s[1] {
	case 'n':
		return "\n"
	case 't':
		return "\t"
	case 'r':
		return "\r"
	case '0':
		if len(s) == 2 {
			return "\x00"
		}
		if v, err := strconv.ParseUint(s[1:], 8, 8); err == nil {
			return string([]byte{byte(v)})
		}
	case 'a':
		return "\a"
	case 'b':
		return "\b"
	case 'f':
		return "\f"
	case 'v':
		return "\v"
	case '\\':
		return `\`
	case 'x':
		if v, err := strconv.ParseUint(s[2:], 16, 8); err == nil {
			return string([]byte{byte(v)})
		}
	default:
		if s[1] >= '0' && s[1] <= '7' {
			if v, err := strconv.ParseUint(s[1:], 8, 8); err == nil {
				return string([]byte{byte(v)})
			}
		}
	}
	return s
}

func parseXargsItems(s string) ([]string, error) {
	var items []string
	var cur strings.Builder
	inToken := false
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			inToken = true
		case '\\':
			inToken = true
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			}
		case ' ', '\t', '\n', '\r':
			if inToken {
				items = append(items, cur.String())
				cur.Reset()
				inToken = false
			}
		default:
			inToken = true
			cur.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unmatched %c quote", quote)
	}
	if inToken {
		items = append(items, cur.String())
	}
	return items, nil
}

func subcommandExitCode(err error) (int, bool) {
	var ee exitError
	if errors.As(err, &ee) {
		return ee.code, true
	}
	if es, ok := interp.IsExitStatus(err); ok {
		return int(es), true
	}
	return 0, false
}

// isExecNotFound reports whether err is the "executable not found"
// failure (from os/exec or our own pre-check).
func isExecNotFound(err error) bool {
	if errors.Is(err, errExecNotFound) || errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var ee *exec.Error
	return errors.As(err, &ee) && ee.Err == exec.ErrNotFound
}

// errExecNotFound marks "command could not be found" failures so
// callers can map them to GNU's exit codes/messages.
var errExecNotFound = errors.New("executable file not found in $PATH")

func runSubcommand(ctx context.Context, hc interp.HandlerContext, cmdArgs []string) error {
	return runSubcommandNamed(ctx, hc, "xargs", cmdArgs)
}

func runSubcommandNamed(ctx context.Context, hc interp.HandlerContext, callerCmd string, cmdArgs []string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(cmdArgs) == 0 {
		return nil
	}
	switch cmdArgs[0] {
	case "printf":
		return cmdPrintf(ctx, hc, cmdArgs)
	case "test", "[":
		_, ovErr := fastCallOverride(ctx, hc, cmdArgs)
		if code, ok := interp.IsExitStatus(ovErr); ok {
			if code == 0 {
				return nil
			}
			return exitError{int(code)}
		}
		return ovErr
	}
	if cmd := lookupExtra(cmdArgs[0]); cmd != nil {
		return invokeExtra(ctx, hc, cmd, cmdArgs)
	}
	if callerCmd == "" {
		callerCmd = cmdArgs[0]
	}
	var checkedPath string
	if CapabilityHookFromContext(ctx) == nil {
		path, err := interp.LookPathDir(hc.Dir, hc.Env, cmdArgs[0])
		if err != nil {
			return errExecNotFound
		}
		checkedPath = path
	} else {
		if strings.ContainsAny(cmdArgs[0], `/\`) {
			cp, cerr := checkExec(ctx, callerCmd, cmdArgs[0], cmdArgs, hc.Dir)
			if cerr != nil {
				return cerr
			}
			checkedPath = cp
		}
		path, err := interp.LookPathDir(hc.Dir, hc.Env, cmdArgs[0])
		if err != nil {
			return errExecNotFound
		}
		pathReal, _ := canonicalizeForCheck(resolveHostPath(hc.Dir, path))
		if checkedPath == "" || !sameCleanPath(checkedPath, pathReal) {
			cp, cerr := checkExec(ctx, callerCmd, path, cmdArgs, hc.Dir)
			if cerr != nil {
				return cerr
			}
			checkedPath = cp
		}
	}
	c := exec.CommandContext(ctx, checkedPath, cmdArgs[1:]...)
	c.Dir = hc.Dir
	c.Stdin = hc.Stdin
	c.Stdout = hc.Stdout
	c.Stderr = hc.Stderr
	c.Env = shellExecEnv(hc)
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitError{ee.ExitCode()}
		}
		return err
	}
	return nil
}

func cmdBase64(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("base64", hc.Stderr)
	decode := fs.Bool("d", false, "")
	fs.BoolVar(decode, "decode", false, "")
	wrap := fs.Int("w", 76, "")
	fs.IntVar(wrap, "wrap", 76, "")
	ignoreGarbage := fs.Bool("i", false, "")
	fs.BoolVar(ignoreGarbage, "ignore-garbage", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	var data []byte
	if len(fs.Args()) > 0 && fs.Args()[0] != "-" && fs.Args()[0] != "/dev/stdin" {
		var err error
		data, err = fsReadShellFile(ctx, hc.Dir, "base64", fs.Args()[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, "base64:", err)
			return exitError{1}
		}
	} else {
		var err error
		data, err = io.ReadAll(hc.Stdin)
		if err != nil {
			if isPipeClosed(err) {
				return err
			}
			fmt.Fprintln(hc.Stderr, "base64:", err)
			return exitError{1}
		}
	}
	if *decode {
		s := string(data)
		if *ignoreGarbage {
			s = stripNonAlphabet(s)
		} else {
			s = strings.Join(strings.Fields(s), "")
		}
		out, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "base64: invalid input")
			return exitError{1}
		}
		if _, err := hc.Stdout.Write(out); err != nil {
			if isPipeClosed(err) {
				return err
			}
			return exitError{1}
		}
		return nil
	}
	enc := base64.StdEncoding.EncodeToString(data)
	if len(enc) == 0 {
		return nil
	}
	if *wrap <= 0 {
		// GNU -w0: one single line, no trailing newline.
		fmt.Fprint(hc.Stdout, enc)
		return nil
	}
	for len(enc) > *wrap {
		if _, err := fmt.Fprintln(hc.Stdout, enc[:*wrap]); isPipeClosed(err) {
			return err
		}
		enc = enc[*wrap:]
	}
	if _, err := fmt.Fprintln(hc.Stdout, enc); isPipeClosed(err) {
		return err
	}
	return nil
}

var _ = runtime.GOOS

// stripNonAlphabet drops every byte outside the base64 alphabet (plus
// '=' padding), for `base64 -d -i/--ignore-garbage` (GNU recovers from
// non-alphabet bytes in the stream instead of failing).
func stripNonAlphabet(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
			c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// parseTouchDate parses GNU touch -d operands: RFC3339, "2006-01-02",
// "2006-01-02 15:04:05", "@epoch", and common variants.
func parseTouchDate(s string) (time.Time, error) {
	// GNU interprets a date without a zone in the LOCAL timezone. Go's
	// time.Parse returns UTC, which shifts the wall clock by the offset
	// and made `touch -d '2020-01-02 03:04:05'` read back as 2020-01-01
	// in negative-offset zones (seen on a Windows ARM64 runner). Parse in
	// time.Local to match.
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"01/02/2006",
		"01/02/2006 15:04:05",
		"20060102",
		time.ANSIC,
		time.RubyDate,
		time.RFC1123,
		time.RFC1123Z,
		time.RFC822,
		time.RFC822Z,
		time.Kitchen,
	}
	for _, f := range formats {
		if t, err := time.ParseInLocation(f, s, time.Local); err == nil {
			return t, nil
		}
	}
	if strings.HasPrefix(s, "@") {
		if n, err := strconv.ParseInt(s[1:], 10, 64); err == nil {
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date")
}

// absFromDir resolves p against dir without touching process-global os.Getwd()
// whenever dir is absolute.
func absFromDir(dir, p string) string {
	resolved := resolveHostPath(dir, p)
	if resolved != "" && !filepath.IsAbs(resolved) {
		if abs, err := filepath.Abs(resolved); err == nil {
			return filepath.Clean(abs)
		}
	}
	return filepath.Clean(resolved)
}

func fsReadlink(ctx context.Context, dir, cmdName, rawPath string) (string, error) {
	p, err := checkFSNoFollowLeaf(ctx, dir, cmdName, rawPath, OpStat)
	if err != nil {
		return "", err
	}
	return os.Readlink(p)
}

// fsRemoveTree recursively removes rawPath under dir, enforcing OpList and
// OpDelete on every traversed entry when a CapabilityHook is active so
// intra-workspace deny rules (e.g. Deny **/.env*) cannot be bypassed via rm -rf.
func fsRemoveTree(ctx context.Context, dir, cmdName, rawPath string) error {
	if CapabilityHookFromContext(ctx) == nil {
		return fsRemoveAll(ctx, dir, cmdName, rawPath)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, err := checkFSNoFollowLeaf(ctx, dir, cmdName, rawPath, OpDelete); err != nil {
		return err
	}
	lfi, err := fsLstat(ctx, dir, cmdName, rawPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !lfi.IsDir() {
		rerr := fsRemove(ctx, dir, cmdName, rawPath)
		if rerr != nil && runtime.GOOS == "windows" && !errors.Is(rerr, os.ErrPermission) && lfi.Mode()&os.ModeSymlink == 0 {
			_ = fsChmod(ctx, dir, cmdName, rawPath, 0o666)
			rerr = fsRemove(ctx, dir, cmdName, rawPath)
		}
		if os.IsNotExist(rerr) {
			return nil
		}
		return rerr
	}
	entries, err := fsReadDir(ctx, dir, cmdName, rawPath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		child := filepath.Join(rawPath, e.Name())
		if err := fsRemoveTree(ctx, dir, cmdName, child); err != nil {
			return err
		}
	}
	rerr := fsRemove(ctx, dir, cmdName, rawPath)
	if rerr != nil && runtime.GOOS == "windows" && !errors.Is(rerr, os.ErrPermission) {
		_ = fsChmod(ctx, dir, cmdName, rawPath, 0o777)
		rerr = fsRemove(ctx, dir, cmdName, rawPath)
	}
	if os.IsNotExist(rerr) {
		return nil
	}
	return rerr
}

// checkMvCapability verifies that moving srcSpec to dstSpec does not violate
// intra-directory deny rules or shift a relative symlink outside the jail.
func checkMvCapability(ctx context.Context, dir, srcSpec, dstSpec string, srcFi os.FileInfo) error {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		return nil
	}
	checkRelLink := func(sSpec, dSpec string) error {
		link, err := fsReadlink(ctx, dir, "mv", sSpec)
		if err != nil {
			return err
		}
		newNoFollow, err := checkFSNoFollowLeaf(ctx, dir, "mv", dSpec, OpWrite)
		if err != nil {
			return err
		}
		targetResolved := resolveHostPath(filepath.Dir(newNoFollow), link)
		targetReal, targetLex := canonicalizeForCheck(targetResolved)
		if err := hook.CheckAccess(ctx, AccessRequest{
			Op:         OpLink,
			Command:    "mv",
			Path:       targetReal,
			RawPath:    link,
			TargetPath: newNoFollow,
		}); err != nil {
			return err
		}
		checkLex := canonicalLexicalInDir(filepath.Dir(newNoFollow), link, targetLex)
		if !sameCleanPath(checkLex, targetReal) {
			if err := hook.CheckAccess(ctx, AccessRequest{
				Op:         OpLink,
				Command:    "mv",
				Path:       checkLex,
				RawPath:    link,
				TargetPath: newNoFollow,
			}); err != nil {
				return err
			}
		}
		return nil
	}
	if srcFi.Mode()&os.ModeSymlink != 0 {
		return checkRelLink(srcSpec, dstSpec)
	}
	if !srcFi.IsDir() {
		return nil
	}
	var walk func(sCur, dCur string) error
	walk = func(sCur, dCur string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := checkFSNoFollowLeaf(ctx, dir, "mv", sCur, OpDelete); err != nil {
			return err
		}
		entries, err := fsReadDir(ctx, dir, "mv", sCur)
		if err != nil {
			return err
		}
		for _, e := range entries {
			sChild := filepath.Join(sCur, e.Name())
			dChild := filepath.Join(dCur, e.Name())
			if e.Type()&os.ModeSymlink != 0 {
				if _, err := checkFSNoFollowLeaf(ctx, dir, "mv", sChild, OpDelete); err != nil {
					return err
				}
				if err := checkRelLink(sChild, dChild); err != nil {
					return err
				}
				continue
			}
			if e.IsDir() {
				if err := walk(sChild, dChild); err != nil {
					return err
				}
				continue
			}
			if _, err := checkFS(ctx, dir, "mv", sChild, OpDelete); err != nil {
				return err
			}
			if _, err := checkFSNoFollowLeaf(ctx, dir, "mv", dChild, OpWrite); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(srcSpec, dstSpec)
}

// sameFile reports whether two paths resolve to the same file.
func sameFile(ctx context.Context, dir, cmdName, a, b string) bool {
	fa, err1 := fsStat(ctx, dir, cmdName, a)
	fb, err2 := fsStat(ctx, dir, cmdName, b)
	if err1 == nil && err2 == nil {
		return os.SameFile(fa, fb)
	}
	lfa, lerr1 := fsLstat(ctx, dir, cmdName, a)
	lfb, lerr2 := fsLstat(ctx, dir, cmdName, b)
	if lerr1 == nil && lerr2 == nil && os.SameFile(lfa, lfb) {
		return true
	}
	aa := absFromDir(dir, a)
	bb := absFromDir(dir, b)
	return aa == bb && lerr1 == nil
}

// isSameOrSubdir reports whether target resolves to srcDir or any path inside srcDir.
func isSameOrSubdir(ctx context.Context, dir, cmdName, srcDirRaw, targetRaw string) bool {
	srcFi, err := fsStat(ctx, dir, cmdName, srcDirRaw)
	if err != nil {
		return false
	}
	absTarget := absFromDir(dir, targetRaw)
	if absTarget == "" {
		return false
	}
	// Find the deepest existing prefix of absTarget.
	cur := absTarget
	for {
		if _, err := fsStat(ctx, "", cmdName, cur); err == nil {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
	if resolved, _ := canonicalizeForCheck(cur); resolved != "" {
		cur = resolved
	}
	for {
		fi, err := fsStat(ctx, "", cmdName, cur)
		if err == nil && os.SameFile(srcFi, fi) {
			return true
		}
		if errors.Is(err, os.ErrPermission) {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return false
}

// isCaseOnlyRename reports whether src and dst refer to a single directory entry
// whose spelling differs only in ASCII/Unicode case on a case-insensitive FS.
func isCaseOnlyRename(ctx context.Context, dir, cmdName, srcRaw, dstRaw string) bool {
	src := resolveHostPath(dir, srcRaw)
	dst := resolveHostPath(dir, dstRaw)
	baseSrc := filepath.Base(src)
	baseDst := filepath.Base(dst)
	if baseSrc == baseDst || !strings.EqualFold(baseSrc, baseDst) {
		return false
	}
	srcParent := filepath.Dir(srcRaw)
	dstParent := filepath.Dir(dstRaw)
	pSrc, err1 := fsStat(ctx, dir, cmdName, srcParent)
	pDst, err2 := fsStat(ctx, dir, cmdName, dstParent)
	if err1 != nil || err2 != nil || !os.SameFile(pSrc, pDst) {
		return false
	}
	entries, err := fsReadDir(ctx, dir, cmdName, srcParent)
	if err != nil {
		return false
	}
	matches := 0
	for _, e := range entries {
		if strings.EqualFold(e.Name(), baseSrc) {
			matches++
		}
	}
	return matches == 1
}

// isCrossDevice reports whether an os.Rename error is EXDEV / ERROR_NOT_SAME_DEVICE.
func isCrossDevice(err error) bool {
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(17)) {
		return true
	}
	return false
}

// moveCrossDevice copies src to target across filesystems, syncs destination data,
// and removes src only after the destination is completely committed.
func moveCrossDevice(ctx context.Context, dir, srcSpec, targetSpec, srcFull, targetFull string, srcFi os.FileInfo) error {
	if srcFi.IsDir() {
		if err := copyDir(ctx, dir, srcSpec, targetSpec, srcFull, targetFull, true, true); err != nil {
			return err
		}
		return fsRemoveTree(ctx, dir, "mv", srcSpec)
	}
	if srcFi.Mode()&os.ModeSymlink != 0 {
		link, err := fsReadlink(ctx, dir, "mv", srcSpec)
		if err != nil {
			return err
		}
		if rerr := fsRemove(ctx, dir, "mv", targetSpec); rerr != nil && !os.IsNotExist(rerr) && errors.Is(rerr, os.ErrPermission) {
			return rerr
		}
		if err := fsSymlink(ctx, dir, "mv", link, targetSpec); err != nil {
			return err
		}
		return fsRemove(ctx, dir, "mv", srcSpec)
	}
	if _, err := checkFS(ctx, dir, "mv", targetSpec, OpWrite); err != nil {
		return err
	}
	in, err := fsOpen(ctx, dir, "mv", srcSpec)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := fsCreateTemp(ctx, dir, "mv", filepath.Dir(targetSpec), ".unish-mv-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, err = io.Copy(tmp, in)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if err == nil {
		err = syncErr
	}
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = fsRemove(ctx, "", "mv", tmpName)
		return err
	}
	_ = in.Close()
	_ = fsChmod(ctx, "", "mv", tmpName, srcFi.Mode().Perm())
	mt := srcFi.ModTime()
	_ = fsChtimes(ctx, "", "mv", tmpName, mt, mt)
	if runtime.GOOS == "windows" {
		_ = fsChmod(ctx, dir, "mv", targetSpec, 0o666)
	}
	if err := fsRename(ctx, "", "mv", tmpName, resolveHostPath(dir, targetSpec)); err != nil {
		_ = fsRemove(ctx, "", "mv", tmpName)
		return err
	}
	return fsRemove(ctx, dir, "mv", srcSpec)
}

// isDotOperand reports whether the rm operand p spells '.' or '..'
// (GNU refuses those under ANY spelling: '.', './', 'sub/..',
// '/abs/path/.', 'C:\dir\..' on Windows, ...). Only the operand
// spelling is considered: '/abs/cwd' is allowed while '.' for the
// same directory is refused.
func isDotOperand(p string) bool {
	q := filepath.ToSlash(p)
	// Strip trailing slashes (but keep '/' itself out: that's root).
	for len(q) > 1 && strings.HasSuffix(q, "/") {
		q = strings.TrimSuffix(q, "/")
	}
	if q == "." || q == ".." {
		return true
	}
	// Last component is '.' or '..' ('./', 'sub/..', '/abs/.').
	if strings.HasSuffix(q, "/.") || strings.HasSuffix(q, "/..") {
		return true
	}
	return false
}

// isRootPath reports whether p resolves to the filesystem root (or the
// shell's drive root on Windows). Used for rm --preserve-root.
func isRootPath(dir, p string) bool {
	if p == "/" {
		return true
	}
	full := resolveHostPath(dir, p)
	// Normalize: root has no parent.
	parent := filepath.Dir(full)
	if parent == full {
		return true
	}
	// Windows drive root like C:\ (resolveHostPath may produce it for "/").
	if len(full) == 3 && full[1] == ':' && (full[2] == '\\' || full[2] == '/') {
		return true
	}
	return false
}
