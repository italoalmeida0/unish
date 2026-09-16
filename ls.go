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
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	names := fs.Args()
	if len(names) == 0 {
		names = []string{"."}
	}
	_ = oneCol
	_ = human

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
			printLsEntry(hc, name, full, fi, *long, *human, *classify, *quoted)
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
			if err := lsRecursive(hc, name, full, lsOpts{*all, *long, *human, *classify, *quoted, *sortSize, *reverse, *sortTime}, &first); err != nil {
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
		for _, e := range entries {
			printLsEntry(hc, e.Name(), filepath.Join(full, e.Name()), mustStat(full, e), *long, *human, *classify, *quoted)
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

type lsOpts struct {
	all, long, human, classify, quoted, sortSize, reverse, sortTime bool
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
		printLsEntry(hc, e.Name(), filepath.Join(full, e.Name()), mustStat(full, e), o.long, o.human, o.classify, o.quoted)
	}
	for _, e := range entries {
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

func printLsEntry(hc interp.HandlerContext, display, full string, fi os.FileInfo, long, human, classify, quoted bool) {
	name := display
	if quoted {
		name = fmt.Sprintf("%q", display)
	}
	if classify && fi != nil {
		name += lsIndicator(fi)
	}
	if !long || fi == nil {
		fmt.Fprintln(hc.Stdout, name)
		return
	}
	fmt.Fprintf(hc.Stdout, "%s %d %s %s %s %s %s\n",
		lsPerm(fi), 1, lsUser(), lsGroup(), lsSize(fi, human),
		fi.ModTime().Format("Jan _2 15:04"), name)
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
