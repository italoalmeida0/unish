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
		// GNU dirname preserves a leading "./" prefix; Go's
		// filepath.Dir cleans it away (Dir("./a/b") == "a").
		prefix := ""
		q := p
		if strings.Trim(q, "/") != "" {
			q = strings.TrimSuffix(p, "/")
		}
		if strings.HasPrefix(q, "./") {
			prefix = "./"
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
	// GNU realpath resolves the lexical path even when the target does
	// not exist (like `realpath -m`); only -e requires existence.
	code := 0
	for _, p := range fs.Args() {
		full := resolve(hc.Dir, p)
		if *exists {
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
				// GNU readlink on a non-symlink fails silently (exit 1).
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
			err = os.Symlink(src, target)
		} else {
			err = os.Link(resolve(hc.Dir, src), target)
		}
		if err != nil {
			// GNU wording: "ln: failed to create symbolic link 'DST': File exists".
			if os.IsExist(err) {
				kind := "hard link"
				if *sym {
					kind = "symbolic link"
				}
				fmt.Fprintf(hc.Stderr, "ln: failed to create %s '%s': File exists\n", kind, rest[len(rest)-1])
			} else {
				fmt.Fprintln(hc.Stderr, "ln:", err)
			}
			return exitError{1}
		}
	}
	return nil
}

func cmdDu(_ context.Context, hc interp.HandlerContext, args []string) error {
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
	for _, p := range paths {
		full := resolve(hc.Dir, p)
		// GNU du without -L does not follow a symlink operand: it
		// reports 0 blocks for the link itself.
		if fi, err := os.Lstat(full); err == nil && fi.Mode()&os.ModeSymlink != 0 {
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
		var entries int64
		filepath.Walk(full, func(_ string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			entries++
			if !info.IsDir() {
				total += info.Size()
			}
			return nil
		})
		blocks := (total + 4095) / 4096
		if blocks < 1 {
			blocks = 1
		}
		// +1 block per directory entry (d itself, subdirs), min like du.
		var dirs int64
		filepath.Walk(full, func(_ string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				dirs++
			}
			return nil
		})
		_ = entries
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

func cmdDate(_ context.Context, hc interp.HandlerContext, args []string) error {
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
	dryRun := fs.Bool("u", false, "")
	fs.BoolVar(dryRun, "dry-run", false, "")
	fs.Bool("t", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// GNU mktemp -t: interpret template as relative to $TMPDIR (compat).
	// The template still comes from positional args; -t only changes the
	// directory interpretation, which already defaults to os.TempDir().
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
			// A template directory of /tmp (or any absolute POSIX path
			// that does not exist, e.g. on Windows) falls back to the
			// OS temp dir, matching GNU's "create in /tmp" intent.
			cand := resolve(hc.Dir, d)
			if st, err := os.Stat(cand); err != nil || !st.IsDir() {
				if filepath.IsAbs(d) || strings.HasPrefix(d, "/") {
					cand = os.TempDir()
				}
			}
			dir = cand
			pattern = filepath.Base(pattern)
		}
	}
	prefix, suffix := splitMktempPattern(pattern)
	if *dryRun {
		name, err := os.MkdirTemp("", "")
		if err != nil {
			fmt.Fprintln(hc.Stderr, "mktemp:", err)
			return exitError{1}
		}
		os.Remove(name)
		// Derive a non-created name with same pattern in target dir.
		f, err := os.CreateTemp(dir, prefix+"*"+suffix)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "mktemp:", err)
			return exitError{1}
		}
		nm := f.Name()
		f.Close()
		os.Remove(nm)
		fmt.Fprintln(hc.Stdout, filepath.ToSlash(nm))
		return nil
	}
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

type findNode struct {
	display string
	full    string
	fi      os.FileInfo
	depth   int
	pruned  bool
}

type findPredicate func(node *findNode, hc interp.HandlerContext) bool

func cmdFind(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var paths, expr []string
	seenExpr := false
	for _, a := range args[1:] {
		if !seenExpr && !strings.HasPrefix(a, "-") && a != "!" && a != "(" {
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
	hasAction := false

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
			sub := parseOr()
			if peek() == ")" {
				next()
			}
			return sub
		}
		switch tok {
		case "-name":
			pat := next()
			return func(n *findNode, _ interp.HandlerContext) bool {
				// GNU matches -name against the basename of the path
				// as given ("." for the root), not the resolved name.
				base := n.fi.Name()
				if b := pathBase(n.display); b != "" {
					base = b
				}
				if ok, _ := filepath.Match(pat, base); ok {
					return true
				}
				if strings.HasPrefix(pat, ".") && !strings.ContainsAny(pat, "*?[") {
					return strings.HasSuffix(base, pat)
				}
				return false
			}
		case "-iname":
			pat := strings.ToLower(next())
			return func(n *findNode, _ interp.HandlerContext) bool {
				base := n.fi.Name()
				if b := pathBase(n.display); b != "" {
					base = b
				}
				name := strings.ToLower(base)
				if ok, _ := filepath.Match(pat, name); ok {
					return true
				}
				if strings.HasPrefix(pat, ".") && !strings.ContainsAny(pat, "*?[") {
					return strings.HasSuffix(name, pat)
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
				isDir := n.fi.IsDir()
				switch t {
				case "d":
					return isDir
				case "f":
					return !isDir && (n.fi.Mode()&os.ModeType == 0)
				case "l":
					return n.fi.Mode()&os.ModeSymlink != 0
				case "s":
					return n.fi.Mode()&os.ModeSocket != 0
				case "p":
					return n.fi.Mode()&os.ModeNamedPipe != 0
				default:
					return false
				}
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
				if n.fi.IsDir() {
					n.pruned = true
				}
				return true
			}
		case "-empty":
			return func(n *findNode, _ interp.HandlerContext) bool {
				if n.fi.IsDir() {
					entries, err := os.ReadDir(n.full)
					return err == nil && len(entries) == 0
				}
				return n.fi.Size() == 0
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
				sz := n.fi.Size()
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
				ageDays := int(now.Sub(n.fi.ModTime()).Hours() / 24)
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
				ageMins := int(now.Sub(n.fi.ModTime()).Minutes())
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
				fmt.Fprintln(hc.Stdout, filepath.ToSlash(n.display))
				return true
			}
		case "-print0":
			hasAction = true
			return func(n *findNode, hc interp.HandlerContext) bool {
				fmt.Fprintf(hc.Stdout, "%s\x00", filepath.ToSlash(n.display))
				return true
			}
		case "-delete":
			hasAction = true
			return func(n *findNode, _ interp.HandlerContext) bool {
				_ = os.Remove(n.full)
				return true
			}
		case "-exec":
			hasAction = true
			var execArgs []string
			for pos < len(expr) {
				a := next()
				if a == ";" || a == "+" {
					break
				}
				execArgs = append(execArgs, a)
			}
			return func(n *findNode, hc interp.HandlerContext) bool {
				var cmdLine []string
				for _, a := range execArgs {
					if a == "{}" {
						cmdLine = append(cmdLine, n.display)
					} else {
						cmdLine = append(cmdLine, a)
					}
				}
				if len(cmdLine) > 0 {
					_ = runSubcommand(ctx, hc, cmdLine)
				}
				return true
			}
		case "-perm":
			modeStr := next()
			return func(n *findNode, _ interp.HandlerContext) bool {
				return matchPerm(n, modeStr)
			}
		case "-newer", "-anewer", "-cnewer":
			ref := next()
			refFull := resolve(hc.Dir, ref)
			fi, err := os.Stat(refFull)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "find: '%s': No such file or directory\n", ref)
				return func(*findNode, interp.HandlerContext) bool { return false }
			}
			refTime := fi.ModTime()
			return func(n *findNode, _ interp.HandlerContext) bool {
				return n.fi.ModTime().After(refTime)
			}
		case "-true":
			return func(*findNode, interp.HandlerContext) bool { return true }
		case "-false":
			return func(*findNode, interp.HandlerContext) bool { return false }
		default:
			if strings.HasPrefix(tok, "-") {
				fmt.Fprintf(hc.Stderr, "find: unknown predicate `%s`\n", tok)
			}
			return func(*findNode, interp.HandlerContext) bool { return true }
		}
	}

	var rootPred findPredicate
	if len(expr) > 0 {
		rootPred = parseOr()
	} else {
		rootPred = func(*findNode, interp.HandlerContext) bool { return true }
	}

	code := 0
	for _, p := range paths {
		full := resolve(hc.Dir, p)
		walkFind(ctx, hc, p, full, 0, maxdepth, mindepth, rootPred, hasAction, &code)
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func walkFind(ctx context.Context, hc interp.HandlerContext, display, full string, depth, maxdepth, mindepth int, pred findPredicate, hasAction bool, code *int) {
	if ctx.Err() != nil {
		return
	}
	fi, err := os.Lstat(full)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "find: '%s': No such file or directory\n", display)
		*code = 1
		return
	}
	node := findNode{
		display: display,
		full:    full,
		fi:      fi,
		depth:   depth,
	}
	match := pred(&node, hc)
	if match && !hasAction && depth >= mindepth {
		fmt.Fprintln(hc.Stdout, filepath.ToSlash(display))
	}
	if !fi.IsDir() || node.pruned {
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
		subDisplay := display
		if subDisplay == "." {
			subDisplay = "./" + e.Name()
		} else if strings.HasSuffix(subDisplay, "/") {
			subDisplay = subDisplay + e.Name()
		} else {
			subDisplay = subDisplay + "/" + e.Name()
		}
		walkFind(ctx, hc, subDisplay, filepath.Join(full, e.Name()), depth+1, maxdepth, mindepth, pred, hasAction, code)
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
	fs.Bool("binary", false, "")
	fs.Bool("t", false, "")
	fs.Bool("text", false, "")
	check := fs.Bool("c", false, "")
	fs.BoolVar(check, "check", false, "")
	quiet := fs.Bool("quiet", false, "")
	status := fs.Bool("status", false, "")
	strict := fs.Bool("strict", false, "")
	fs.Bool("tag", false, "")
	warn := fs.Bool("w", false, "")
	fs.BoolVar(warn, "warn", false, "")
	_ = quiet
	_ = status
	_ = strict
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *check {
		return runHashCheck(name, newHash, hc, fs.Args(), *quiet, *status)
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
			fh, err := openShellFile(resolve(hc.Dir, f))
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
func runHashCheck(name string, newHash func() hash.Hash, hc interp.HandlerContext, files []string, quiet, status bool) error {
	if len(files) == 0 {
		files = []string{"-"}
	}
	code := 0
	checked := 0
	failed := 0
	badFmt := 0
	for _, f := range files {
		var data []byte
		var err error
		if f == "-" || f == "/dev/stdin" {
			data, err = io.ReadAll(hc.Stdin)
		} else {
			data, err = readShellFile(resolve(hc.Dir, f))
		}
		if err != nil {
			fmt.Fprintln(hc.Stderr, name+":", err)
			code = 1
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			sum, path, ok := parseHashLine(line)
			if !ok {
				badFmt++
				continue
			}
			checked++
			raw, err := readShellFile(resolve(hc.Dir, path))
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
	_ = failed
	_ = badFmt
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
	if v >= 1 && v <= 31 {
		return true
	}
	if v >= 34 && v <= 64 {
		return true
	}
	return false
}

// matchPerm implements find -perm MODE: exact match (644), all-bits
// (-mode), any-bit (/mode). Symbolic specs (u+x) match when all listed
// bits are present.
func matchPerm(n *findNode, spec string) bool {
	perm := n.fi.Mode().Perm()
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
