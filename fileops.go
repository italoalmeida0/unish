package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	extraCommands = append(extraCommands,
		extraCmd{"cp", cmdCp},
		extraCmd{"mv", cmdMv},
		extraCmd{"rm", cmdRm},
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

func cmdCp(_ context.Context, hc interp.HandlerContext, args []string) error {
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
	fs.Bool("d", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "cp: missing destination")
		return flag.ErrHelp
	}
	dst := resolve(hc.Dir, rest[len(rest)-1])
	srcs := rest[:len(rest)-1]
	dstFi, dstErr := os.Stat(dst)
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
		src := resolve(hc.Dir, s)
		target := dst
		if dstIsDir {
			target = filepath.Join(dst, filepath.Base(src))
		}
		// GNU: "cp: 'a' and 'b' are the same file" (exit 1).
		if sameFile(src, target) {
			fmt.Fprintf(hc.Stderr, "cp: '%s' and '%s' are the same file\n", s, rest[len(rest)-1])
			code = 1
			continue
		}
		if *noClobber {
			if _, err := os.Stat(target); err == nil {
				continue
			}
		}
		if err := copyOne(src, target, *rec, *force, *noDeref); err != nil {
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

func copyOne(src, dst string, rec, force, noDeref bool) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if !rec {
			return fmt.Errorf("%s is a directory (use -r)", src)
		}
		return copyDir(src, dst, force)
	}
	if fi.Mode()&os.ModeSymlink != 0 && noDeref {
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if force {
			os.Remove(dst)
		}
		return os.Symlink(link, dst)
	}
	// GNU default: follow the link, copying content into a new file.
	if st, err := os.Stat(src); err == nil {
		fi = st
	}
	return copyFile(src, dst, fi.Mode(), force)
}

func copyFile(src, dst string, mode os.FileMode, force bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if force {
		os.Remove(dst)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	cerr := out.Close()
	if err != nil {
		return err
	}
	return cerr
}

func copyDir(src, dst string, force bool) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, fi.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d, force); err != nil {
				return err
			}
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		if err := copyFile(s, d, fi.Mode(), force); err != nil {
			return err
		}
	}
	return nil
}

func cmdMv(_ context.Context, hc interp.HandlerContext, args []string) error {
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
	dst := resolve(hc.Dir, rest[len(rest)-1])
	srcs := rest[:len(rest)-1]
	dstFi, dstErr := os.Stat(dst)
	dstIsDir := dstErr == nil && dstFi.IsDir()
	if len(srcs) > 1 && !dstIsDir {
		fmt.Fprintln(hc.Stderr, "mv: target is not a directory")
		return exitError{1}
	}
	code := 0
	for _, s := range srcs {
		src := resolve(hc.Dir, s)
		target := dst
		if dstIsDir {
			target = filepath.Join(dst, filepath.Base(src))
		}
		if *noClobber {
			if _, err := os.Stat(target); err == nil {
				continue
			}
		}
		if *update {
			if sfi, serr := os.Stat(src); serr == nil {
				if dfi, derr := os.Stat(target); derr == nil && !sfi.ModTime().After(dfi.ModTime()) {
					continue
				}
			}
		}
		// GNU: "mv: 'a' and 'b' are the same file" (exit 1).
		if sameFile(src, target) {
			fmt.Fprintf(hc.Stderr, "mv: '%s' and '%s' are the same file\n", s, rest[len(rest)-1])
			code = 1
			continue
		}
		if *force {
			os.Remove(target)
		}
		if err := os.Rename(src, target); err != nil {
			fi, serr := os.Lstat(src)
			if serr != nil {
				fmt.Fprintf(hc.Stderr, "mv: %v\n", serr)
				code = 1
				continue
			}
			if fi.IsDir() {
				if err := copyDir(src, target, true); err != nil {
					fmt.Fprintf(hc.Stderr, "mv: %v\n", err)
					code = 1
					continue
				}
				os.RemoveAll(src)
			} else {
				if err := copyFile(src, target, fi.Mode(), true); err != nil {
					fmt.Fprintf(hc.Stderr, "mv: %v\n", err)
					code = 1
					continue
				}
				os.Remove(src)
			}
			continue
		}
		if *verb {
			dispDst := rest[len(rest)-1]
			if dstIsDir {
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

func cmdRm(_ context.Context, hc interp.HandlerContext, args []string) error {
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
		// GNU: never remove '.' or '..', under ANY spelling ('.', './',
		// 'sub/..', '/abs/path/.', ...), even with --no-preserve-root.
		// Only the operand spelling matters: 'rm -rf /abs/cwd' is
		// allowed while 'rm -rf .' is refused for the same directory.
		if isDotOperand(p) {
			fmt.Fprintf(hc.Stderr, "rm: refusing to remove '.' or '..' directory: skipping %q\n", p)
			code = 1
			continue
		}
		full := resolve(hc.Dir, p)
		fi, err := os.Lstat(full)
		if err != nil {
			if *force {
				continue
			}
			fmt.Fprintf(hc.Stderr, "rm: cannot remove '%s': No such file or directory\n", p)
			code = 1
			continue
		}
		if fi.IsDir() {
			if !*rec {
				fmt.Fprintf(hc.Stderr, "rm: cannot remove '%s': Is a directory\n", p)
				code = 1
				continue
			}
			err = os.RemoveAll(full)
		} else {
			err = os.Remove(full)
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

func cmdMkdir(_ context.Context, hc interp.HandlerContext, args []string) error {
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
		full := resolve(hc.Dir, p)
		var err error
		if *parents {
			err = os.MkdirAll(full, mode)
		} else {
			err = os.Mkdir(full, mode)
		}
		if err != nil {
			if *parents {
				if _, serr := os.Stat(full); serr == nil {
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

func cmdTouch(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("touch", hc.Stderr)
	noCreate := fs.Bool("c", false, "")
	fs.BoolVar(noCreate, "no-create", false, "")
	dateStr := fs.String("d", "", "")
	fs.StringVar(dateStr, "date", "", "")
	refFile := fs.String("r", "", "")
	fs.StringVar(refFile, "reference", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		fmt.Fprintln(hc.Stderr, "touch: missing operand")
		return flag.ErrHelp
	}
	mtime := time.Now()
	if *refFile != "" {
		fi, err := os.Stat(resolve(hc.Dir, *refFile))
		if err != nil {
			fmt.Fprintf(hc.Stderr, "touch: failed to get attributes of '%s': No such file or directory\n", *refFile)
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
		full := resolve(hc.Dir, p)
		if _, err := os.Stat(full); err != nil {
			if *noCreate {
				continue
			}
			f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY, 0o666)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "touch: cannot touch '%s': %v\n", p, err)
				code = 1
				continue
			}
			f.Close()
		}
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			fmt.Fprintf(hc.Stderr, "touch: cannot touch '%s': %v\n", p, err)
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdChmod(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("chmod", hc.Stderr)
	rec := fs.Bool("R", false, "")
	fs.BoolVar(rec, "recursive", false, "")
	verb := fs.Bool("v", false, "")
	fs.BoolVar(verb, "verbose", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "chmod: missing operand")
		return flag.ErrHelp
	}
	modeStr, paths := rest[0], rest[1:]
	apply := func(full, display string) error {
		mode, err := parseChmodMode(modeStr, full)
		if err != nil {
			return err
		}
		if err := os.Chmod(full, mode); err != nil {
			return err
		}
		if *verb {
			fmt.Fprintf(hc.Stdout, "mode of '%s' changed to %04o\n", display, mode.Perm())
		}
		return nil
	}
	code := 0
	for _, p := range paths {
		full := resolve(hc.Dir, p)
		if *rec {
			filepath.Walk(full, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				rel, _ := filepath.Rel(hc.Dir, path)
				if e := apply(path, rel); e != nil {
					fmt.Fprintf(hc.Stderr, "chmod: %v\n", e)
					code = 1
				}
				return nil
			})
			continue
		}
		if err := apply(full, p); err != nil {
			// GNU wording: "chmod: invalid mode: 'X'\nTry 'chmod --help' for more information."
			msg := err.Error()
			if strings.HasPrefix(msg, "invalid mode") {
				fmt.Fprintf(hc.Stderr, "chmod: invalid mode: \u2018%s\u2019\n", modeStr)
				fmt.Fprintln(hc.Stderr, "Try 'chmod --help' for more information.")
			} else if perr, ok := err.(*os.PathError); ok {
				fmt.Fprintf(hc.Stderr, "chmod: cannot access '%s': No such file or directory\n", p)
				_ = perr
			} else {
				fmt.Fprintf(hc.Stderr, "chmod: %v\n", err)
			}
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func parseChmodMode(s, path string) (os.FileMode, error) {
	if m, err := strconv.ParseUint(s, 8, 32); err == nil {
		return os.FileMode(m), nil
	}
	var cur os.FileMode
	if fi, err := os.Stat(path); err == nil {
		cur = fi.Mode().Perm()
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
		for _, c := range perms {
			switch c {
			case 'r':
				bits |= 0o444
			case 'w':
				bits |= 0o222
			case 'x':
				bits |= 0o111
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
		bits &= mask
		switch action {
		case '+':
			cur |= bits
		case '-':
			cur &^= bits
		case '=':
			cur = (cur &^ mask) | bits
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
	_ = maxProcs
	rest := fs.Args()
	if len(rest) == 0 {
		rest = []string{"echo"}
	}
	var data []byte
	var err error
	data, err = io.ReadAll(hc.Stdin)
	if err != nil {
		return err
	}
	var items []string
	if *nullDelim {
		for _, part := range strings.Split(string(data), "\x00") {
			if part != "" {
				items = append(items, part)
			}
		}
	} else if *delim != "" {
		for _, part := range strings.Split(string(data), *delim) {
			if part != "" {
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
		items = strings.Fields(string(data))
	}
	if len(items) == 0 {
		// GNU xargs runs the command ONCE with no arguments on empty
		// input, unless -r/--no-run-if-empty is given.
		if *noRun {
			return nil
		}
		if err := runSubcommand(ctx, hc, rest); err != nil {
			if es, ok := err.(exitError); ok && es.code == 127 {
				return err
			}
			return exitError{1}
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
	code := 0
	for i := 0; i < len(items); i += batch {
		end := i + batch
		if end > len(items) {
			end = len(items)
		}
		chunk := items[i:end]
		var cmdArgs []string
		if *replace != "" {
			cmdArgs = make([]string, len(rest))
			for k, a := range rest {
				cmdArgs[k] = strings.ReplaceAll(a, *replace, strings.Join(chunk, " "))
			}
		} else {
			cmdArgs = append(append([]string{}, rest...), chunk...)
		}
		if *trace {
			fmt.Fprintf(hc.Stderr, "+ %s\n", strings.Join(cmdArgs, " "))
		}
		if err := runSubcommand(ctx, hc, cmdArgs); err != nil {
			if es, ok := err.(exitError); ok && es.code == 127 {
				return err
			}
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func runSubcommand(ctx context.Context, hc interp.HandlerContext, cmdArgs []string) error {
	if len(cmdArgs) == 0 {
		return nil
	}
	if cmd := lookupExtra(cmdArgs[0]); cmd != nil {
		return cmd.main(ctx, hc, splitAttached(cmdArgs[0], cmdArgs))
	}
	return interp.DefaultExecHandler(2)(ctx, cmdArgs)
}

func cmdBase64(_ context.Context, hc interp.HandlerContext, args []string) error {
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
	_ = ignoreGarbage // accepted for parity; decoder already skips whitespace; strict alphabet kept for real garbage
	var data []byte
	if len(fs.Args()) > 0 {
		var err error
		data, err = readShellFile(resolve(hc.Dir, fs.Args()[0]))
		if err != nil {
			fmt.Fprintln(hc.Stderr, "base64:", err)
			return exitError{1}
		}
	} else {
		var err error
		data, err = io.ReadAll(hc.Stdin)
		if err != nil {
			return err
		}
	}
	if *decode {
		clean := strings.Join(strings.Fields(string(data)), "")
		out, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "base64: invalid input")
			return exitError{1}
		}
		_, err = hc.Stdout.Write(out)
		return err
	}
	enc := base64.StdEncoding.EncodeToString(data)
	if *wrap <= 0 {
		fmt.Fprintln(hc.Stdout, enc)
		return nil
	}
	for len(enc) > *wrap {
		fmt.Fprintln(hc.Stdout, enc[:*wrap])
		enc = enc[*wrap:]
	}
	fmt.Fprintln(hc.Stdout, enc)
	return nil
}

var _ = runtime.GOOS

// parseTouchDate parses GNU touch -d operands: RFC3339, "2006-01-02",
// "2006-01-02 15:04:05", "@epoch", and common variants.
func parseTouchDate(s string) (time.Time, error) {
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
		if t, err := time.Parse(f, s); err == nil {
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

// sameFile reports whether two paths resolve to the same file.
func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		// Fall back to lexical identity (covers missing-dst cases
		// like `cp a.txt a.txt` where dst == src path).
		aa, _ := filepath.Abs(a)
		bb, _ := filepath.Abs(b)
		return aa == bb && err1 == nil
	}
	return os.SameFile(fa, fb)
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
	full := resolve(dir, p)
	// Normalize: root has no parent.
	parent := filepath.Dir(full)
	if parent == full {
		return true
	}
	// Windows drive root like C:\ (resolve may produce it for "/").
	if len(full) == 3 && full[1] == ':' && (full[2] == '\\' || full[2] == '/') {
		return true
	}
	return false
}
