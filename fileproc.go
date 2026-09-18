package main

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
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
		d := filepath.Dir(strings.TrimSuffix(p, "/"))
		fmt.Fprintln(hc.Stdout, filepath.ToSlash(d))
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
		base := filepath.Base(strings.TrimSuffix(p, "/"))
		fmt.Fprintln(hc.Stdout, strings.TrimSuffix(base, *suffix))
	}
	return nil
}

func cmdRealpath(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("realpath", hc.Stderr)
	missing := fs.Bool("m", false, "")
	exists := fs.Bool("e", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		return flag.ErrHelp
	}
	code := 0
	for _, p := range fs.Args() {
		full := resolve(hc.Dir, p)
		if !*missing {
			if _, err := os.Stat(full); err != nil {
				fmt.Fprintln(hc.Stderr, "realpath:", err)
				code = 1
				continue
			}
		}
		abs, err := filepath.Abs(full)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "realpath:", err)
			code = 1
			continue
		}
		if *exists || !*missing {
			if rp, err := filepath.EvalSymlinks(abs); err == nil {
				abs = rp
			} else if *exists {
				fmt.Fprintln(hc.Stderr, "realpath:", err)
				code = 1
				continue
			}
		}
		fmt.Fprintln(hc.Stdout, abs)
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdReadlink(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("readlink", hc.Stderr)
	canonical := fs.Bool("f", false, "")
	_ = fs.Bool("m", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		return flag.ErrHelp
	}
	code := 0
	for _, p := range fs.Args() {
		full := resolve(hc.Dir, p)
		var dst string
		if *canonical {
			abs, err := filepath.Abs(full)
			if err != nil {
				fmt.Fprintln(hc.Stderr, "readlink:", err)
				code = 1
				continue
			}
			rp, err := filepath.EvalSymlinks(abs)
			if err != nil {
				fmt.Fprintln(hc.Stderr, "readlink:", err)
				code = 1
				continue
			}
			dst = rp
		} else {
			var err error
			dst, err = os.Readlink(full)
			if err != nil {
				fmt.Fprintln(hc.Stderr, "readlink:", err)
				code = 1
				continue
			}
		}
		fmt.Fprintln(hc.Stdout, dst)
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdLn(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("ln", hc.Stderr)
	sym := fs.Bool("s", false, "")
	force := fs.Bool("f", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "ln: missing destination")
		return flag.ErrHelp
	}
	dst := resolve(hc.Dir, rest[len(rest)-1])
	for _, src := range rest[:len(rest)-1] {
		target := dst
		if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
			target = filepath.Join(dst, filepath.Base(src))
		}
		if *force {
			os.Remove(target)
		}
		var err error
		if *sym {
			err = os.Symlink(resolve(hc.Dir, src), target)
		} else {
			err = os.Link(resolve(hc.Dir, src), target)
		}
		if err != nil {
			fmt.Fprintln(hc.Stderr, "ln:", err)
			return exitError{1}
		}
	}
	return nil
}

func cmdDu(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("du", hc.Stderr)
	summary := fs.Bool("s", false, "")
	human := fs.Bool("h", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	for _, p := range paths {
		full := resolve(hc.Dir, p)
		var total int64
		filepath.Walk(full, func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				total += info.Size()
			}
			return nil
		})
		if *human {
			fmt.Fprintf(hc.Stdout, "%s\t%s\n", humanSize(total), p)
		} else {
			fmt.Fprintf(hc.Stdout, "%d\t%s\n", total/1024+1, p)
		}
		_ = summary
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
		d, err := parseDurationLoose(a)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "sleep: invalid time interval:", a)
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
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") && len(a) > 1 && (a[1] < '0' || a[1] > '9') {
			sig = strings.ToUpper(strings.TrimPrefix(a, "-"))
			if strings.HasPrefix(sig, "SIG") {
				sig = sig[3:]
			}
			continue
		}
		pids = append(pids, a)
	}
	if len(pids) == 0 {
		fmt.Fprintln(hc.Stderr, "kill: missing operand")
		return flag.ErrHelp
	}
	code := 0
	for _, p := range pids {
		pid, err := strconv.Atoi(p)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "kill:", err)
			code = 1
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "kill:", err)
			code = 1
			continue
		}
		if err := killProc(proc, sig); err != nil {
			fmt.Fprintf(hc.Stderr, "kill: (%d) - %v\n", pid, err)
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdDate(_ context.Context, hc interp.HandlerContext, args []string) error {
	now := time.Now()
	var format string
	for _, a := range args[1:] {
		switch {
		case a == "-u" || a == "--utc" || a == "--universal":
			now = now.UTC()
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

func cmdMktemp(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("mktemp", hc.Stderr)
	isDir := fs.Bool("d", false, "")
	tmpdir := fs.String("p", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	dir := *tmpdir
	if dir == "" {
		dir = os.TempDir()
	} else {
		dir = resolve(hc.Dir, dir)
	}
	pattern := "tmp.XXXXXXXXXX"
	if len(fs.Args()) > 0 {
		pattern = fs.Args()[0]
	}
	if *tmpdir == "" {
		if d := filepath.Dir(pattern); d != "." && d != "" {
			dir = resolve(hc.Dir, d)
			pattern = filepath.Base(pattern)
		}
	}
	prefix, suffix := splitMktempPattern(pattern)
	if *isDir {
		name, err := os.MkdirTemp(dir, prefix+"*"+suffix)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "mktemp:", err)
			return exitError{1}
		}
		fmt.Fprintln(hc.Stdout, filepath.ToSlash(name))
		return nil
	}
	f, err := os.CreateTemp(dir, prefix+"*"+suffix)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "mktemp:", err)
		return exitError{1}
	}
	f.Close()
	fmt.Fprintln(hc.Stdout, filepath.ToSlash(f.Name()))
	return nil
}

func splitMktempPattern(pattern string) (prefix, suffix string) {
	if i := strings.Index(pattern, "XX"); i >= 0 {
		j := i
		for j < len(pattern) && pattern[j] == 'X' {
			j++
		}
		return pattern[:i], pattern[j:]
	}
	return pattern + ".", ""
}

func cmdFind(_ context.Context, hc interp.HandlerContext, args []string) error {
	var paths, expr []string
	seenFlag := false
	for _, a := range args[1:] {
		if !seenFlag && !strings.HasPrefix(a, "-") {
			paths = append(paths, a)
		} else {
			seenFlag = true
			expr = append(expr, a)
		}
	}
	fs := newFlagSet("find", hc.Stderr)
	maxdepth := fs.Int("maxdepth", -1, "")
	name := fs.String("name", "", "")
	iname := fs.String("iname", "", "")
	typ := fs.String("type", "", "")
	print := fs.Bool("print", false, "")
	if err := fs.Parse(expr); err != nil {
		return err
	}
	_ = print
	if len(paths) == 0 {
		paths = []string{"."}
	}
	code := 0
	for _, p := range paths {
		full := resolve(hc.Dir, p)
		base := p
		if base == "." {
			base = "."
		}
		walkFind(hc, base, full, 0, *maxdepth, *name, *iname, *typ, &code)
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func walkFind(hc interp.HandlerContext, display, full string, depth, maxdepth int, name, iname, typ string, code *int) {
	fi, err := os.Lstat(full)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "find: '%s': No such file or directory\n", display)
		*code = 1
		return
	}
	match := true
	if name != "" {
		ok, _ := filepath.Match(name, fi.Name())
		match = ok
	}
	if iname != "" {
		ok, _ := filepath.Match(strings.ToLower(iname), strings.ToLower(fi.Name()))
		match = match && ok
	}
	if typ != "" {
		isDir := fi.IsDir()
		wantDir := typ == "d"
		wantFile := typ == "f" || typ == "l" && fi.Mode()&os.ModeSymlink != 0
		if typ == "l" {
			match = match && fi.Mode()&os.ModeSymlink != 0
		} else {
			match = match && ((wantDir && isDir) || (wantFile && !isDir))
		}
	}
	if match {
		fmt.Fprintln(hc.Stdout, filepath.ToSlash(display))
	}
	if !fi.IsDir() {
		return
	}
	if maxdepth >= 0 && depth >= maxdepth {
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "find: '%s': %v\n", display, err)
		*code = 1
		return
	}
	for _, e := range entries {
		walkFind(hc, display+"/"+e.Name(), filepath.Join(full, e.Name()), depth+1, maxdepth, name, iname, typ, code)
	}
}

func cmdMd5sum(_ context.Context, hc interp.HandlerContext, args []string) error {
	return cmdHash("md5sum", md5.New, hc, args)
}

func cmdSha1sum(_ context.Context, hc interp.HandlerContext, args []string) error {
	return cmdHash("sha1sum", sha1.New, hc, args)
}

func cmdSha256sum(_ context.Context, hc interp.HandlerContext, args []string) error {
	return cmdHash("sha256sum", sha256.New, hc, args)
}

func cmdShasum(_ context.Context, hc interp.HandlerContext, args []string) error {
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
	return runHash("shasum", newHash, hc, fs.Args())
}

func cmdHash(name string, newHash func() hash.Hash, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet(name, hc.Stderr)
	fs.Bool("b", false, "")
	fs.Bool("t", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	return runHash(name, newHash, hc, fs.Args())
}

func runHash(name string, newHash func() hash.Hash, hc interp.HandlerContext, files []string) error {
	if len(files) == 0 {
		files = []string{"-"}
	}
	code := 0
	for _, f := range files {
		var r io.Reader = hc.Stdin
		label := "-"
		if f != "-" {
			fh, err := os.Open(resolve(hc.Dir, f))
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
