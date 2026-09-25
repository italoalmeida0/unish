package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"

	"golang.org/x/term"
)

func cmdLs(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("ls", hc.Stderr)
	all := fs.Bool("a", false, "")
	long := fs.Bool("l", false, "")
	human := fs.Bool("h", false, "")
	dirOnly := fs.Bool("d", false, "")
	recurse := fs.Bool("R", false, "")
	classify := fs.Bool("F", false, "")
	quoted := fs.Bool("Q", false, "")
	sortSize := fs.Bool("S", false, "")
	reverse := fs.Bool("r", false, "")
	sortTime := fs.Bool("t", false, "")
	oneCol := fs.Bool("1", false, "")
	comma := fs.Bool("m", false, "")
	cols := fs.Bool("C", false, "")
	across := fs.Bool("x", false, "")
	inode := fs.Bool("i", false, "")
	timeStyle := fs.String("time-style", "", "")
	fullTime := fs.Bool("full-time", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *fullTime {
		*timeStyle = "full-iso"
	}
	names := fs.Args()
	if len(names) == 0 {
		names = []string{"."}
	}
	_ = oneCol
	_ = human
	_ = comma
	_ = cols
	_ = across
	_ = inode

	code := 0
	multi := len(names) > 1
	for ni, name := range names {
		full := resolve(hc.Dir, name)
		fi, err := os.Lstat(full)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "ls: cannot access '%s': No such file or directory\n", name)
			code = 2
			continue
		}
		if !fi.IsDir() || *dirOnly {
			printLsEntryInode(hc, name, full, fi, *long, *human, *classify, *quoted, *inode, *timeStyle)
			continue
		}
		if multi {
			if ni > 0 {
				fmt.Fprintln(hc.Stdout)
			}
			fmt.Fprintf(hc.Stdout, "%s:\n", name)
		}
		if *recurse {
			first := true
			if err := lsRecursive(hc, name, full, lsOpts{*all, *long, *human, *classify, *quoted, *sortSize, *reverse, *sortTime, *inode, *timeStyle}, &first); err != nil {
				code = 2
			}
			continue
		}
		entries, err := readDirSorted(full, *all, *sortSize, *reverse, *sortTime)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "ls: cannot open directory '%s': %v\n", name, err)
			code = 2
			continue
		}
		if *comma {
			var names2 []string
			for _, e := range entries {
				names2 = append(names2, lsDisplayName(e.Name(), filepath.Join(full, e.Name()), mustStat(full, e), *classify, *quoted))
			}
			fmt.Fprintln(hc.Stdout, strings.Join(names2, ", "))
		} else if *cols || *across {
			var names2 []string
			for _, e := range entries {
				names2 = append(names2, lsDisplayName(e.Name(), filepath.Join(full, e.Name()), mustStat(full, e), *classify, *quoted))
			}
			fmt.Fprintln(hc.Stdout, strings.Join(names2, "  "))
		} else {
			for _, e := range entries {
				printLsEntryInode(hc, e.Name(), filepath.Join(full, e.Name()), mustStat(full, e), *long, *human, *classify, *quoted, *inode, *timeStyle)
			}
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

type lsOpts struct {
	all, long, human, classify, quoted, sortSize, reverse, sortTime bool
	inode                                                           bool
	timeStyle                                                       string
}

func lsRecursive(hc interp.HandlerContext, display, full string, o lsOpts, first *bool) error {
	entries, err := readDirSorted(full, o.all, o.sortSize, o.reverse, o.sortTime)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "ls: cannot open directory '%s': %v\n", display, err)
		return err
	}
	if !*first {
		fmt.Fprintln(hc.Stdout)
	}
	*first = false
	fmt.Fprintf(hc.Stdout, "%s:\n", display)
	for _, e := range entries {
		printLsEntryInode(hc, e.Name(), filepath.Join(full, e.Name()), mustStat(full, e), o.long, o.human, o.classify, o.quoted, o.inode, o.timeStyle)
	}
	for _, e := range entries {
		// Never descend into "." or ".." (infinite recursion); GNU
		// ls -R skips them for descent while still DISPLAYING them
		// under -a.
		if e.Name() == "." || e.Name() == ".." {
			continue
		}
		if e.IsDir() {
			if err := lsRecursive(hc, display+"/"+e.Name(), filepath.Join(full, e.Name()), o, first); err != nil {
				return err
			}
		}
	}
	return nil
}

func mustStat(dir string, e os.DirEntry) os.FileInfo {
	if de, ok := e.(*dotDirEntry); ok {
		fi, _ := os.Stat(de.real)
		return fi
	}
	if fi, err := e.Info(); err == nil {
		return fi
	}
	if fi, err := os.Lstat(filepath.Join(dir, e.Name())); err == nil {
		return fi
	}
	return nil
}

type dotDirEntry struct {
	name string
	real string
}

func (d *dotDirEntry) Name() string               { return d.name }
func (d *dotDirEntry) IsDir() bool                { return true }
func (d *dotDirEntry) Type() os.FileMode          { return os.ModeDir }
func (d *dotDirEntry) Info() (os.FileInfo, error) { return os.Stat(d.real) }

func readDirSorted(dir string, all, sortSize, reverse, sortTime bool) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dots []os.DirEntry
	if all {
		dots = []os.DirEntry{
			&dotDirEntry{name: ".", real: dir},
			&dotDirEntry{name: "..", real: filepath.Dir(dir)},
		}
	} else {
		filtered := entries[:0:0]
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	entries = append(dots, entries...)
	statCache := map[string]os.FileInfo{}
	stat := func(e os.DirEntry) os.FileInfo {
		if fi, ok := statCache[e.Name()]; ok {
			return fi
		}
		fi, _ := e.Info()
		statCache[e.Name()] = fi
		return fi
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		var less bool
		switch {
		case sortSize:
			var sa, sb int64
			if fi := stat(a); fi != nil {
				sa = fi.Size()
			}
			if fi := stat(b); fi != nil {
				sb = fi.Size()
			}
			if sa != sb {
				less = sa > sb
			} else {
				less = a.Name() < b.Name()
			}
		case sortTime:
			var ta, tb time.Time
			if fi := stat(a); fi != nil {
				ta = fi.ModTime()
			}
			if fi := stat(b); fi != nil {
				tb = fi.ModTime()
			}
			if !ta.Equal(tb) {
				less = ta.After(tb)
			} else {
				less = a.Name() < b.Name()
			}
		default:
			less = a.Name() < b.Name()
		}
		if reverse {
			return !less
		}
		return less
	})
	return entries, nil
}

// lsTimeFormat renders mtime per --time-style / --full-time (GNU names):
// full-iso, long-iso, iso, locale, +FORMAT (strftime-ish, only the
// verbs agents use: %Y %m %d %H %M %S %N %T %F). Default: "Jan _2 15:04".
func lsTimeFormat(mt time.Time, style string) string {
	switch style {
	case "full-iso":
		return mt.Format("2006-01-02 15:04:05.000000000 -0700")
	case "long-iso":
		return mt.Format("2006-01-02 15:04")
	case "iso":
		return mt.Format("01-02 15:04")
	case "locale":
		return mt.Format("Jan _2 15:04")
	}
	if strings.HasPrefix(style, "+") {
		f := style[1:]
		r := strings.NewReplacer(
			"%Y", "2006", "%m", "01", "%d", "02",
			"%H", "15", "%M", "04", "%S", "05",
			"%N", "000000000", "%T", "15:04:05", "%F", "2006-01-02",
		)
		return mt.Format(r.Replace(f))
	}
	return mt.Format("Jan _2 15:04")
}

func printLsEntry(hc interp.HandlerContext, display, full string, fi os.FileInfo, long, human, classify, quoted bool, timeStyle string) {
	name := display
	if quoted {
		name = fmt.Sprintf("%q", display)
	}
	if classify && fi != nil {
		name += lsIndicator(fi)
	}
	wrap := func(s string) string {
		if code := lsColorCode(fi, display); lsColorWanted() && code != "" {
			return "\x1b[" + code + "m" + s + "\x1b[0m"
		}
		return s
	}
	if !long || fi == nil {
		fmt.Fprintln(hc.Stdout, wrap(name))
		return
	}
	// GNU `ls -l` renders symlinks as "name -> target".
	if fi.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(full); err == nil {
			name += " -> " + target
		}
	}
	fmt.Fprintf(hc.Stdout, "%s %d %s %s %s %s %s\n",
		lsPerm(fi), 1, lsUser(), lsGroup(), lsSize(fi, human),
		lsTimeFormat(fi.ModTime(), timeStyle), wrap(name))
}

func lsIndicator(fi os.FileInfo) string {
	if fi.IsDir() {
		return "/"
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "@"
	}
	if fi.Mode()&os.ModeNamedPipe != 0 {
		return "|"
	}
	if fi.Mode()&os.ModeSocket != 0 {
		return "="
	}
	if fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
		return "*"
	}
	return ""
}

func lsPerm(fi os.FileInfo) string {
	m := fi.Mode()
	var sb strings.Builder
	switch {
	case m.IsDir():
		sb.WriteByte('d')
	case m&os.ModeSymlink != 0:
		sb.WriteByte('l')
	default:
		sb.WriteByte('-')
	}
	for _, b := range []struct {
		bit  os.FileMode
		char byte
	}{
		{0o400, 'r'}, {0o200, 'w'}, {0o100, 'x'},
		{0o040, 'r'}, {0o020, 'w'}, {0o010, 'x'},
		{0o004, 'r'}, {0o002, 'w'}, {0o001, 'x'},
	} {
		if m&b.bit != 0 {
			sb.WriteByte(b.char)
		} else {
			sb.WriteByte('-')
		}
	}
	return sb.String()
}

func lsUser() string {
	if u, err := userCurrent(); err == nil && u != "" {
		if i := strings.LastIndexAny(u, `\/`); i >= 0 {
			u = u[i+1:]
		}
		return u
	}
	return "user"
}

func lsGroup() string { return "group" }

func lsSize(fi os.FileInfo, human bool) string {
	if human {
		return humanSize(fi.Size())
	}
	return fmt.Sprintf("%d", fi.Size())
}

// lsColorWanted reports whether ANSI colors should be emitted.
func lsColorWanted() bool {
	switch lsColorMode {
	case "always":
		return true
	case "auto":
		return term.IsTerminal(1)
	}
	return false
}

// lsColorCode is the GNU ls color for an entry (dir blue, symlink cyan,
// executable green, fifo/socket/device yellow, archives red).
func lsColorCode(fi os.FileInfo, name string) string {
	if fi == nil {
		return ""
	}
	mode := fi.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return "36"
	case mode.IsDir():
		return "34"
	case mode&os.ModeNamedPipe != 0, mode&os.ModeSocket != 0, mode&os.ModeDevice != 0:
		return "33"
	case mode&0o111 != 0:
		return "32"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".tar", ".gz", ".tgz", ".zip", ".bz2", ".xz", ".7z", ".rar":
		return "31"
	}
	return ""
}

func lsDisplayName(display, full string, fi os.FileInfo, classify, quoted bool) string {
	name := display
	if quoted {
		name = fmt.Sprintf("%q", display)
	}
	if classify && fi != nil {
		name += lsIndicator(fi)
	}
	if code := lsColorCode(fi, display); lsColorWanted() && code != "" {
		return "\x1b[" + code + "m" + name + "\x1b[0m"
	}
	return name
}

func printLsEntryInode(hc interp.HandlerContext, display, full string, fi os.FileInfo, long, human, classify, quoted, inode bool, timeStyle string) {
	if inode && !long {
		var ino string
		if _, err := os.Lstat(full); err == nil {
			ino = fmt.Sprintf("%d", inodeOf(full))
		} else {
			ino = "?"
		}
		fmt.Fprintf(hc.Stdout, "%s %s\n", ino, lsDisplayName(display, full, fi, classify, quoted))
		return
	}
	if inode && long {
		var ino string
		if _, err := os.Lstat(full); err == nil {
			ino = fmt.Sprintf("%d", inodeOf(full))
		} else {
			ino = "?"
		}
		// GNU puts inode first; keep long format after it.
		old := hc.Stdout
		_ = old
		fmt.Fprintf(hc.Stdout, "%s ", ino)
		printLsEntry(hc, display, full, fi, long, human, classify, quoted, timeStyle)
		return
	}
	printLsEntry(hc, display, full, fi, long, human, classify, quoted, timeStyle)
}
