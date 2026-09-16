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
		if err := copyOne(src, target, *rec, *force); err != nil {
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

func copyOne(src, dst string, rec, force bool) error {
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
	if fi.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if force {
			os.Remove(dst)
		}
		return os.Symlink(link, dst)
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
			fmt.Fprintf(hc.Stdout, "'%s' -> '%s'\n", s, target)
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
	force := fs.Bool("f", false, "")
	verb := fs.Bool("v", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		if *force {
			return nil
		}
		fmt.Fprintln(hc.Stderr, "rm: missing operand")
		return flag.ErrHelp
	}
	code := 0
	for _, p := range rest {
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
			fmt.Fprintf(hc.Stderr, "mkdir: cannot create directory '%s': %v\n", p, err)
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
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		fmt.Fprintln(hc.Stderr, "touch: missing operand")
		return flag.ErrHelp
	}
	mtime := time.Now()
	if *dateStr != "" {
		if t, err := time.Parse(time.RFC3339, *dateStr); err == nil {
			mtime = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", *dateStr); err == nil {
			mtime = t
		} else {
			fmt.Fprintf(hc.Stderr, "touch: invalid date '%s'\n", *dateStr)
			return exitError{1}
		}
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
			fmt.Fprintf(hc.Stderr, "chmod: %v\n", err)
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
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		rest = []string{"echo"}
	}
	var data []byte
	var err error
	if *nullDelim {
		data, err = io.ReadAll(hc.Stdin)
	} else {
		data, err = io.ReadAll(hc.Stdin)
	}
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
	} else {
		items = strings.Fields(string(data))
	}
	if len(items) == 0 {
		return nil
	}
	batch := *maxArgs
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
		if err := runXargsCmd(ctx, hc, cmdArgs); err != nil {
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

func runXargsCmd(ctx context.Context, hc interp.HandlerContext, cmdArgs []string) error {
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
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	var data []byte
	if len(fs.Args()) > 0 {
		var err error
		data, err = os.ReadFile(resolve(hc.Dir, fs.Args()[0]))
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
