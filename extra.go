package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

type extraCmd struct {
	name string
	main func(ctx context.Context, hc interp.HandlerContext, args []string) error
}

var extraCommands []extraCmd

func init() {
	extraCommands = []extraCmd{
		{"cat", cmdCat},
		{"ls", cmdLs},
		{"grep", cmdGrep},
		{"head", cmdHead},
		{"tail", cmdTail},
		{"sort", cmdSort},
		{"uniq", cmdUniq},
		{"wc", cmdWc},
		{"tee", cmdTee},
		{"tr", cmdTr},
		{"seq", cmdSeq},
		{"cut", cmdCut},
		{"paste", cmdPaste},
		{"comm", cmdComm},
		{"split", cmdSplit},
		{"diff", cmdDiff},
		{"cmp", cmdCmp},
		{"hexdump", cmdHexdump},
		{"strings", cmdStrings},
		{"dirname", cmdDirname},
		{"basename", cmdBasename},
		{"realpath", cmdRealpath},
		{"readlink", cmdReadlink},
		{"ln", cmdLn},
		{"du", cmdDu},
		{"sleep", cmdSleep},
		{"kill", cmdKill},
		{"pwd", cmdPwd},
		{"true", cmdTrue},
		{"false", cmdFalse},
		{"yes", cmdYes},
		{"which", cmdWhich},
		{"printenv", cmdPrintenv},
		{"whoami", cmdWhoami},
		{"nproc", cmdNproc},
		{"clear", cmdClear},
		{"echo", cmdEcho},
		{"date", cmdDate},
		{"uname", cmdUname},
		{"hostname", cmdHostname},
		{"md5sum", cmdMd5sum},
		{"sha1sum", cmdSha1sum},
		{"sha256sum", cmdSha256sum},
		{"shasum", cmdShasum},
		{"mktemp", cmdMktemp},
		{"find", cmdFind},
	}
}

func lookupExtra(name string) *extraCmd {
	for i := range extraCommands {
		if extraCommands[i].name == name {
			return &extraCommands[i]
		}
	}
	return nil
}

func extraHandler(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}
		if args[0] == "ls" {
			args = filterLsArgs(args)
		}
		cmd := lookupExtra(args[0])
		if cmd == nil {
			return next(ctx, args)
		}
		if args[0] == "cat" && !hasCatFlag(args[1:]) {
			return next(ctx, args)
		}
		for _, a := range args[1:] {
			if a == "--version" {
				hc := interp.HandlerCtx(ctx)
				fmt.Fprintf(hc.Stdout, "%s (unish coreutils) 1.0\n", args[0])
				return nil
			}
		}
		hc := interp.HandlerCtx(ctx)
		err := cmd.main(ctx, hc, splitAttached(args[0], args))
		if err != nil {
			switch {
			case ctx.Err() == context.DeadlineExceeded:
				return interp.NewExitStatus(124)
			case ctx.Err() == context.Canceled:
				return interp.NewExitStatus(130)
			}
			if ee, ok := err.(exitError); ok {
				return interp.NewExitStatus(uint8(ee.code))
			}
			if err == flag.ErrHelp {
				return interp.NewExitStatus(2)
			}
			return err
		}
		return nil
	}
}

func filterLsArgs(args []string) []string {
	out := args[:1:1]
	for _, a := range args[1:] {
		if a == "--color" || strings.HasPrefix(a, "--color=") ||
			a == "--hyperlink" || strings.HasPrefix(a, "--hyperlink=") ||
			a == "--group-directories-first" || a == "--indicator-style=classify" {
			continue
		}
		out = append(out, a)
	}
	return out
}

func callOverride(ctx context.Context, args []string) ([]string, error) {
	if len(args) == 0 || args[0] != "kill" {
		return args, nil
	}
	hc := interp.HandlerCtx(ctx)
	if err := cmdKill(ctx, hc, splitAttached("kill", args)); err != nil {
		return []string{"false"}, nil
	}
	return []string{"true"}, nil
}

func resolve(dir, p string) string {
	if p == "" || p == "-" || filepath.IsAbs(p) || dir == "" {
		return p
	}
	if runtime.GOOS == "windows" && len(p) > 0 && (p[0] == '/' || p[0] == '\\') {
		if w, ok := msysPath(p); ok {
			return w
		}
	}
	return filepath.Join(dir, p)
}

var msysCygpathOnce sync.Once
var msysCygpath string

func msysPath(p string) (string, bool) {
	msysCygpathOnce.Do(func() {
		if v, err := exec.LookPath("cygpath.exe"); err == nil {
			msysCygpath = v
		} else if v, err := exec.LookPath("cygpath"); err == nil {
			msysCygpath = v
		}
	})
	if msysCygpath == "" {
		return "", false
	}
	out, err := exec.Command(msysCygpath, "-w", p).Output()
	if err != nil {
		return "", false
	}
	w := strings.TrimSpace(string(out))
	if w == "" {
		return "", false
	}
	return w, true
}

func shellGetenv(hc interp.HandlerContext, key string) string {
	if v := hc.Env.Get(key); v.IsSet() {
		return v.Str
	}
	return os.Getenv(key)
}

type flagSpec struct {
	bools  string
	values string
	long   map[string]string
}

var flagSpecs = map[string]flagSpec{
	"grep": {
		bools:  "clFivnhqrRaEo",
		values: "emABCf",
		long: map[string]string{
			"count": "c", "files-with-matches": "l", "fixed-strings": "F",
			"ignore-case": "i", "invert-match": "v", "line-number": "n",
			"no-filename": "h", "quiet": "q", "silent": "q", "recursive": "r",
			"regexp": "e", "max-count": "m", "after-context": "A",
			"before-context": "B", "context": "C", "only-matching": "o",
			"extended-regexp": "E", "file": "f", "exclude": "exclude",
			"include": "include", "exclude-dir": "exclude-dir", "color": "",
		},
	},
	"head": {bools: "qv", values: "nc", long: map[string]string{
		"lines": "n", "bytes": "c", "quiet": "q", "silent": "q", "verbose": "v",
	}},
	"tail": {bools: "qvf", values: "nc", long: map[string]string{
		"lines": "n", "bytes": "c", "quiet": "q", "silent": "q",
		"verbose": "v", "follow": "f",
	}},
	"sort": {bools: "rufnc", values: "tk", long: map[string]string{
		"reverse": "r", "unique": "u", "ignore-case": "f", "numeric-sort": "n",
		"check": "c", "key": "k",
	}},
	"uniq": {bools: "cdui", values: "", long: map[string]string{
		"count": "c", "repeated": "d", "unique": "u", "ignore-case": "i",
	}},
	"wc": {bools: "lwcmL", values: "", long: map[string]string{
		"lines": "l", "words": "w", "bytes": "c", "chars": "m",
		"max-line-length": "L",
	}},
	"tee": {bools: "a", values: "", long: map[string]string{"append": "a"}},
	"tr":  {bools: "dsc", values: "", long: map[string]string{
		"delete": "d", "squeeze-repeats": "s", "complement": "c",
	}},
	"cut": {bools: "s", values: "dfc", long: map[string]string{
		"delimiter": "d", "fields": "f", "characters": "c",
		"only-delimited": "s",
	}},
	"paste": {bools: "s", values: "d", long: map[string]string{
		"delimiters": "d", "serial": "s",
	}},
	"cat": {bools: "nbEsSTA", values: "", long: map[string]string{
		"number": "n", "number-nonblank": "b", "squeeze-blank": "s",
		"show-ends": "E", "show-tabs": "T", "show-all": "A",
	}},
	"ls": {bools: "alhdRFQSrt1", values: "", long: map[string]string{
		"all": "a", "long": "l", "human-readable": "h", "directory": "d",
		"recursive": "R", "classify": "F", "quoted": "Q",
		"size": "S", "reverse": "r", "time": "t",
	}},
	"comm": {bools: "123", values: ""},
	"split": {bools: "d", values: "la", long: map[string]string{
		"lines": "l", "suffix-length": "a",
	}},
	"diff": {bools: "qu", values: "", long: map[string]string{
		"brief": "q", "report-identical-files": "s", "unified": "u",
	}},
	"cmp": {bools: "sl", values: "n", long: map[string]string{
		"silent": "s", "quiet": "s",
	}},
	"id": {bools: "ug", values: "", long: map[string]string{}},
	"du": {bools: "sh", values: "", long: map[string]string{
		"summarize": "s", "human-readable": "h",
	}},
	"df": {bools: "hkP", values: "", long: map[string]string{
		"human-readable": "h",
	}},
	"ps": {bools: "aefu", values: "", long: map[string]string{}},
	"free": {bools: "mhg", values: "", long: map[string]string{}},
	"stat": {bools: "", values: "c", long: map[string]string{}},
	"ss": {bools: "tuln", values: "", long: map[string]string{}},
	"env": {bools: "i", values: "", long: map[string]string{}},
	"nice": {bools: "", values: "n", long: map[string]string{
		"adjustment": "n",
	}},
	"cp": {bools: "rfvn", values: "", long: map[string]string{
		"recursive": "r", "force": "f", "verbose": "v", "no-clobber": "n",
	}},
	"mv": {bools: "funv", values: "", long: map[string]string{
		"force": "f", "update": "u", "no-clobber": "n", "verbose": "v",
	}},
	"rm": {bools: "rfv", values: "", long: map[string]string{
		"recursive": "r", "force": "f", "verbose": "v",
	}},
	"mkdir": {bools: "pv", values: "m", long: map[string]string{
		"parents": "p", "verbose": "v",
	}},
	"touch": {bools: "c", values: "d", long: map[string]string{
		"no-create": "c", "date": "d",
	}},
	"chmod": {bools: "Rv", values: "", long: map[string]string{
		"recursive": "R", "verbose": "v",
	}},
	"xargs": {bools: "t0", values: "nI", long: map[string]string{}},
	"base64": {bools: "d", values: "w", long: map[string]string{
		"decode": "d", "wrap": "w",
	}},
	"tar": {bools: "cxtzv", values: "fC", long: map[string]string{}},
	"gzip": {bools: "kcf", values: "", long: map[string]string{
		"keep": "k", "stdout": "c", "force": "f",
	}},
	"gunzip": {bools: "kcf", values: "", long: map[string]string{
		"keep": "k", "stdout": "c", "force": "f",
	}},
	"mktemp": {bools: "d", values: "p", long: map[string]string{
		"directory": "d", "tmpdir": "p",
	}},
	"ln": {bools: "sf", values: "", long: map[string]string{
		"symbolic": "s", "force": "f",
	}},
	"date": {bools: "u", values: "", long: map[string]string{"universal": "u"}},
	"uname": {bools: "amnrsv", values: "", long: map[string]string{
		"all": "a", "machine": "m", "nodename": "n",
		"release": "r", "sysname": "s",
	}},
	"hexdump": {bools: "C", values: "n"},
	"strings": {bools: "a", values: "n"},
	"md5sum":  {bools: "bt", values: ""},
	"sha1sum": {bools: "bt", values: ""},
	"sha256sum": {bools: "bt", values: ""},
	"readlink": {bools: "fm", values: ""},
	"realpath": {bools: "em", values: ""},
	"basename": {bools: "a", values: "s"},
	"od": {bools: "Anvxcdu", values: "tNfwjA", long: map[string]string{
		"format": "t", "address-radix": "A", "output-duplicates": "",
		"read-bytes": "N", "skip-bytes": "j", "width": "w",
		"strings": "S",
	}},
	"nl": {bools: "", values: "biwsvn", long: map[string]string{
		"body-numbering": "b", "line-increment": "i", "number-width": "w",
		"number-separator": "s", "starting-line-number": "v",
		"number-format": "n",
	}},
	"tac": {bools: "br", values: "s", long: map[string]string{
		"before": "b", "regex": "r", "separator": "s",
	}},
	"rev": {bools: "", values: ""},
	"fold": {bools: "sb", values: "w", long: map[string]string{
		"width": "w", "spaces": "s", "bytes": "b",
	}},
	"expand": {bools: "", values: "t", long: map[string]string{"tabs": "t"}},
	"unexpand": {bools: "a", values: "t", long: map[string]string{
		"tabs": "t", "all": "a", "first-only": "first-only",
	}},
	"join": {bools: "i", values: "12jateo", long: map[string]string{
		"ignore-case": "i",
	}},
}

func splitAttached(cmd string, args []string) []string {
	spec, ok := flagSpecs[cmd]
	if !ok {
		return args
	}
	var movable, rest []string
	movable = append(movable, args[0])
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			name := a[2:]
			val := ""
			hasVal := false
			if i := strings.IndexByte(name, '='); i >= 0 {
				val, hasVal = name[i+1:], true
				name = name[:i]
			}
			short, known := spec.long[name]
			if !known {
				rest = append(rest, a)
				continue
			}
			if short == "" {
				continue
			}
			if len(short) > 1 {
				if hasVal {
					movable = append(movable, "--"+short, val)
				} else if i+1 < len(argv) {
					movable = append(movable, "--"+short, argv[i+1])
					i++
				} else {
					rest = append(rest, "--"+short)
				}
				continue
			}
			if strings.ContainsRune(spec.values, rune(short[0])) {
				if hasVal {
					movable = append(movable, "-"+short, val)
				} else if i+1 < len(argv) {
					movable = append(movable, "-"+short, argv[i+1])
					i++
				} else {
					rest = append(rest, "-"+short)
				}
			} else {
				movable = append(movable, "-"+short)
				if hasVal {
					rest = append(rest, val)
				}
			}
			continue
		}
		if len(a) > 2 && a[0] == '-' && a[1] != '-' &&
			strings.ContainsRune(spec.values, rune(a[1])) {
			movable = append(movable, "-"+string(a[1]), a[2:])
			continue
		}
		if len(a) > 2 && a[0] == '-' && a[1] != '-' && isFlagChar(cmd, a[1]) {
			if expanded, ok := cascadeBools(spec, a[1:]); ok {
				movable = append(movable, expanded...)
				continue
			}
		}
		rest = append(rest, a)
	}
	return append(movable, rest...)
}

func isFlagChar(cmd string, c byte) bool {
	if isASCIILetter(c) {
		return true
	}
	return c >= '0' && c <= '9' && cmd == "comm"
}

func cascadeBools(spec flagSpec, s string) ([]string, bool) {
	var out []string
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.ContainsRune(spec.bools, rune(c)) {
			out = append(out, "-"+string(c))
			continue
		}
		if strings.ContainsRune(spec.values, rune(c)) {
			out = append(out, "-"+string(c))
			if i+1 < len(s) {
				out = append(out, s[i+1:])
			}
			return out, true
		}
		return nil, false
	}
	return out, true
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func parseN(args []string, def uint64) (uint64, []string) {
	n := def
	rest := args[:0:0]
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' && a[1] >= '0' && a[1] <= '9' {
			if v, err := parseUint(a[1:]); err == nil {
				n = v
				continue
			}
		}
		rest = append(rest, a)
	}
	return n, rest
}

func parseUint(s string) (uint64, error) {
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + uint64(c-'0')
	}
	return n, nil
}

func openInputs(dir string, args []string, stdin io.Reader) (readers []io.Reader, names []string, closeAll func(), err error) {
	if len(args) == 0 {
		return []io.Reader{stdin}, []string{""}, func() {}, nil
	}
	var files []*os.File
	closeAll = func() {
		for _, f := range files {
			f.Close()
		}
	}
	for _, a := range args {
		if a == "-" {
			readers = append(readers, stdin)
			names = append(names, "")
			continue
		}
		p := resolve(dir, a)
		f, e := os.Open(p)
		if e != nil {
			closeAll()
			return nil, nil, func() {}, e
		}
		files = append(files, f)
		readers = append(readers, f)
		names = append(names, a)
	}
	return readers, names, closeAll, nil
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func hasCatFlag(args []string) bool {
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' && a != "-" {
			return true
		}
	}
	return false
}

func cmdCat(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("cat", hc.Stderr)
	numAll := fs.Bool("n", false, "")
	numNonBlank := fs.Bool("b", false, "")
	squeeze := fs.Bool("s", false, "")
	showEnds := fs.Bool("E", false, "")
	showTabs := fs.Bool("T", false, "")
	showAll := fs.Bool("A", false, "")
	fs.BoolVar(numAll, "number", false, "")
	fs.BoolVar(numNonBlank, "number-nonblank", false, "")
	fs.BoolVar(squeeze, "squeeze-blank", false, "")
	fs.BoolVar(showEnds, "show-ends", false, "")
	fs.BoolVar(showTabs, "show-tabs", false, "")
	if *showAll {
		*showEnds, *showTabs = true, true
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cat:", err)
		return exitError{1}
	}
	defer closeAll()
	ln := 0
	prevBlank := false
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			blank := line == ""
			if *squeeze && blank && prevBlank {
				continue
			}
			prevBlank = blank
			out := line
			if *showTabs {
				out = strings.ReplaceAll(out, "\t", "^I")
			}
			if *showEnds {
				out += "$"
			}
			number := *numAll || (*numNonBlank && !blank)
			if number {
				ln++
				fmt.Fprintf(hc.Stdout, "%6d\t%s\n", ln, out)
			} else {
				fmt.Fprintln(hc.Stdout, out)
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "cat:", err)
			return exitError{1}
		}
	}
	return nil
}

func cmdPwd(_ context.Context, hc interp.HandlerContext, args []string) error {
	_ = args
	fmt.Fprintln(hc.Stdout, hc.Dir)
	return nil
}

func cmdTrue(_ context.Context, _ interp.HandlerContext, _ []string) error {
	return nil
}

func cmdFalse(_ context.Context, _ interp.HandlerContext, _ []string) error {
	return exitError{1}
}

func cmdYes(ctx context.Context, hc interp.HandlerContext, args []string) error {
	s := "y"
	if len(args) > 1 {
		s = strings.Join(args[1:], " ")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if _, err := fmt.Fprintln(hc.Stdout, s); err != nil {
			return nil
		}
	}
}

func cmdEcho(_ context.Context, hc interp.HandlerContext, args []string) error {
	rest := args[1:]
	newline := true
	if len(rest) > 0 && rest[0] == "-n" {
		newline = false
		rest = rest[1:]
	}
	if newline {
		fmt.Fprintln(hc.Stdout, strings.Join(rest, " "))
	} else {
		fmt.Fprint(hc.Stdout, strings.Join(rest, " "))
	}
	return nil
}

func cmdClear(_ context.Context, hc interp.HandlerContext, _ []string) error {
	fmt.Fprint(hc.Stdout, "\033[H\033[2J")
	return nil
}

func cmdWhich(_ context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) < 2 {
		return flag.ErrHelp
	}
	code := 0
	for _, name := range args[1:] {
		if p, err := findExec(hc, name); err == nil {
			fmt.Fprintln(hc.Stdout, p)
			continue
		}
		if lookupExtra(name) != nil || isKnownCoreutil(name) || interp.IsBuiltin(name) {
			fmt.Fprintf(hc.Stdout, "%s: unish builtin\n", name)
			continue
		}
		code = 1
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func isKnownCoreutil(name string) bool {
	switch name {
		case "ls", "cat", "cp", "mv", "rm", "mkdir", "touch",
			"chmod", "find", "mktemp", "xargs", "gzip", "gunzip",
			"gzcat", "tar", "base64", "shasum":
			return true
	}
	return false
}

func findExec(hc interp.HandlerContext, name string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		p := resolve(hc.Dir, name)
		fi, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if fi.IsDir() {
			return "", fmt.Errorf("is a directory")
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	path := shellGetenv(hc, "PATH")
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		for _, cand := range execCandidates(dir, name) {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("%s not found", name)
}

func execCandidates(dir, name string) []string {
	if runtime.GOOS == "windows" {
		out := []string{filepath.Join(dir, name)}
		for _, e := range strings.Split(os.Getenv("PATHEXT"), ";") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, filepath.Join(dir, name+e))
			}
		}
		return out
	}
	return []string{filepath.Join(dir, name)}
}

func cmdPrintenv(_ context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) == 1 {
		hc.Env.Each(func(name string, v expand.Variable) bool {
			if v.IsSet() && v.Exported {
				fmt.Fprintf(hc.Stdout, "%s=%s\n", name, v.Str)
			}
			return true
		})
		return nil
	}
	code := 0
	for _, k := range args[1:] {
		if v := hc.Env.Get(k); v.IsSet() {
			fmt.Fprintln(hc.Stdout, v.Str)
		} else if ev, ok := os.LookupEnv(k); ok {
			fmt.Fprintln(hc.Stdout, ev)
		} else {
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdWhoami(_ context.Context, hc interp.HandlerContext, _ []string) error {
	for _, k := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := shellGetenv(hc, k); v != "" {
			if i := strings.LastIndexAny(v, `\/ `); i >= 0 && k == "USERNAME" {
				v = v[i+1:]
				if v == "" {
					continue
				}
			}
			fmt.Fprintln(hc.Stdout, v)
			return nil
		}
	}
	if u, err := userCurrent(); err == nil && u != "" {
		if i := strings.LastIndexAny(u, `\/`); i >= 0 {
			u = u[i+1:]
		}
		fmt.Fprintln(hc.Stdout, u)
		return nil
	}
	return fmt.Errorf("whoami: cannot find name for user")
}

func userCurrent() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

func cmdNproc(_ context.Context, hc interp.HandlerContext, _ []string) error {
	fmt.Fprintln(hc.Stdout, runtime.NumCPU())
	return nil
}

func cmdHostname(_ context.Context, hc interp.HandlerContext, _ []string) error {
	h, err := os.Hostname()
	if err != nil {
		return err
	}
	fmt.Fprintln(hc.Stdout, h)
	return nil
}

func cmdUname(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("uname", hc.Stderr)
	all := fs.Bool("a", false, "")
	machine := fs.Bool("m", false, "")
	node := fs.Bool("n", false, "")
	release := fs.Bool("r", false, "")
	sysname := fs.Bool("s", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	name := map[string]string{"windows": "Windows", "linux": "Linux", "darwin": "Darwin"}[runtime.GOOS]
	if name == "" {
		name = runtime.GOOS
	}
	if *all {
		host, _ := os.Hostname()
		fmt.Fprintf(hc.Stdout, "%s %s unish 1.0 %s\n", name, host, runtime.GOARCH)
		return nil
	}
	var parts []string
	if *sysname || (!*machine && !*node && !*release) {
		parts = append(parts, name)
	}
	if *node {
		host, _ := os.Hostname()
		parts = append(parts, host)
	}
	if *release {
		parts = append(parts, "unish")
	}
	if *machine {
		parts = append(parts, runtime.GOARCH)
	}
	fmt.Fprintln(hc.Stdout, strings.Join(parts, " "))
	return nil
}
