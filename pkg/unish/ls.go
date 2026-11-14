package unish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"

	"golang.org/x/term"
)

func cmdLs(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		full := resolveHostPath(hc.Dir, name)
		fi, err := fsLstat(ctx, hc.Dir, "ls", name)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(hc.Stderr, "ls: cannot access '%s': No such file or directory\n", name)
			} else {
				fmt.Fprintf(hc.Stderr, "ls: cannot access '%s': %v\n", name, err)
			}
			code = 2
			continue
		}
		if !*dirOnly && fi.Mode()&os.ModeSymlink != 0 && (strings.HasSuffix(name, "/") || (!*long && !*classify)) {
			if sfi, serr := fsStat(ctx, hc.Dir, "ls", name); serr == nil && sfi.IsDir() {
				fi = sfi
			} else if serr != nil && strings.HasSuffix(name, "/") && errors.Is(serr, os.ErrPermission) {
				fmt.Fprintf(hc.Stderr, "ls: cannot access '%s': %v\n", name, serr)
				code = 2
				continue
			}
		}
		if !fi.IsDir() || *dirOnly {
			if werr := printLsEntryInode(ctx, hc, hc.Dir, name, name, full, fi, *long, *human, *classify, *quoted, *inode, *timeStyle); isPipeClosed(werr) {
				return werr
			}
			continue
		}
		if multi {
			if ni > 0 {
				if _, werr := fmt.Fprintln(hc.Stdout); isPipeClosed(werr) {
					return werr
				}
			}
			if _, werr := fmt.Fprintf(hc.Stdout, "%s:\n", name); isPipeClosed(werr) {
				return werr
			}
		}
		if *recurse {
			first := true
			if err := lsRecursive(ctx, hc, name, full, fi, lsOpts{*all, *long, *human, *classify, *quoted, *sortSize, *reverse, *sortTime, *inode, *timeStyle}, &first, []os.FileInfo{fi}); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if isPipeClosed(err) {
					return err
				}
				code = 2
			}
			continue
		}
		entries, err := readDirSorted(ctx, hc.Dir, name, full, fi, *all, *sortSize, *reverse, *sortTime)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "ls: cannot open directory '%s': %v\n", name, err)
			code = 2
			continue
		}
		if *comma {
			var names2 []string
			for _, e := range entries {
				names2 = append(names2, lsDisplayName(e.Name(), filepath.Join(full, e.Name()), mustStat(ctx, hc.Dir, name, full, e), *classify, *quoted))
			}
			if _, werr := fmt.Fprintln(hc.Stdout, strings.Join(names2, ", ")); isPipeClosed(werr) {
				return werr
			}
		} else if *cols || *across {
			var names2 []string
			for _, e := range entries {
				names2 = append(names2, lsDisplayName(e.Name(), filepath.Join(full, e.Name()), mustStat(ctx, hc.Dir, name, full, e), *classify, *quoted))
			}
			if _, werr := fmt.Fprintln(hc.Stdout, strings.Join(names2, "  ")); isPipeClosed(werr) {
				return werr
			}
		} else {
			for _, e := range entries {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				entryRaw := lsChildRaw(name, e.Name())
				if werr := printLsEntryInode(ctx, hc, hc.Dir, entryRaw, e.Name(), filepath.Join(full, e.Name()), mustStat(ctx, hc.Dir, name, full, e), *long, *human, *classify, *quoted, *inode, *timeStyle); isPipeClosed(werr) {
					return werr
				}
			}
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func lsChildRaw(dirRaw, elem string) string {
	if elem == "." {
		return dirRaw
	}
	if elem == ".." {
		return filepath.Join(dirRaw, "..")
	}
	return filepath.Join(dirRaw, elem)
}

type lsOpts struct {
	all, long, human, classify, quoted, sortSize, reverse, sortTime bool
	inode                                                           bool
	timeStyle                                                       string
}

func lsRecursive(ctx context.Context, hc interp.HandlerContext, display, full string, dirFi os.FileInfo, o lsOpts, first *bool, ancestors []os.FileInfo) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	entries, err := readDirSorted(ctx, hc.Dir, display, full, dirFi, o.all, o.sortSize, o.reverse, o.sortTime)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "ls: cannot open directory '%s': %v\n", display, err)
		return exitError{2}
	}
	if !*first {
		if _, werr := fmt.Fprintln(hc.Stdout); isPipeClosed(werr) {
			return werr
		}
	}
	*first = false
	if _, werr := fmt.Fprintf(hc.Stdout, "%s:\n", display); isPipeClosed(werr) {
		return werr
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		entryRaw := lsChildRaw(display, e.Name())
		if werr := printLsEntryInode(ctx, hc, hc.Dir, entryRaw, e.Name(), filepath.Join(full, e.Name()), mustStat(ctx, hc.Dir, display, full, e), o.long, o.human, o.classify, o.quoted, o.inode, o.timeStyle); isPipeClosed(werr) {
			return werr
		}
	}
	hadErr := false
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Never descend into "." or ".." (infinite recursion); GNU
		// ls -R skips them for descent while still DISPLAYING them
		// under -a.
		if e.Name() == "." || e.Name() == ".." {
			continue
		}
		if e.IsDir() {
			subFull := filepath.Join(full, e.Name())
			subDisp := strings.TrimSuffix(display, "/") + "/" + e.Name()
			var subAnc []os.FileInfo
			var subFi os.FileInfo
			if sfi, serr := fsStat(ctx, hc.Dir, "ls", subDisp); serr == nil {
				subFi = sfi
				loop := false
				for _, anc := range ancestors {
					if anc != nil && os.SameFile(anc, sfi) {
						loop = true
						break
					}
				}
				if loop {
					fmt.Fprintf(hc.Stderr, "ls: %s: not listing already-listed directory\n", subDisp)
					hadErr = true
					continue
				}
				subAnc = append(ancestors[:len(ancestors):len(ancestors)], sfi)
			} else if errors.Is(serr, os.ErrPermission) {
				fmt.Fprintf(hc.Stderr, "ls: cannot open directory '%s': %v\n", subDisp, serr)
				hadErr = true
				continue
			} else {
				subAnc = ancestors
			}
			if err := lsRecursive(ctx, hc, subDisp, subFull, subFi, o, first, subAnc); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if isPipeClosed(err) {
					return err
				}
				hadErr = true
			}
		}
	}
	if hadErr {
		return exitError{2}
	}
	return nil
}

func mustStat(ctx context.Context, baseDir, rawDir, _ string, e os.DirEntry) os.FileInfo {
	if de, ok := e.(*dotDirEntry); ok {
		fi, _ := de.Info()
		return fi
	}
	if CapabilityHookFromContext(ctx) == nil {
		if fi, err := e.Info(); err == nil {
			return fi
		}
	}
	if fi, err := fsLstat(ctx, baseDir, "ls", filepath.Join(rawDir, e.Name())); err == nil {
		return fi
	}
	if CapabilityHookFromContext(ctx) != nil {
		return nil
	}
	if fi, err := e.Info(); err == nil {
		return fi
	}
	return nil
}

type dotDirEntry struct {
	ctx      context.Context
	name     string
	real     string
	fallback os.FileInfo
}

type fallbackDirInfo struct{ name string }

func (d fallbackDirInfo) Name() string       { return d.name }
func (d fallbackDirInfo) Size() int64        { return 0 }
func (d fallbackDirInfo) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (d fallbackDirInfo) ModTime() time.Time { return time.Time{} }
func (d fallbackDirInfo) IsDir() bool        { return true }
func (d fallbackDirInfo) Sys() any           { return nil }

func (d *dotDirEntry) Name() string      { return d.name }
func (d *dotDirEntry) IsDir() bool       { return true }
func (d *dotDirEntry) Type() os.FileMode { return os.ModeDir }
func (d *dotDirEntry) Info() (os.FileInfo, error) {
	fi, err := fsStat(d.ctx, "", "ls", d.real)
	if err != nil {
		if d.fallback != nil {
			return d.fallback, nil
		}
		return fallbackDirInfo{name: d.name}, nil
	}
	return fi, nil
}

func readDirSorted(ctx context.Context, baseDir, rawDir, fullDir string, dirFi os.FileInfo, all, sortSize, reverse, sortTime bool) ([]os.DirEntry, error) {
	entries, err := fsReadDir(ctx, baseDir, "ls", rawDir)
	if err != nil {
		return nil, err
	}
	hookActive := CapabilityHookFromContext(ctx) != nil
	var dots []os.DirEntry
	if all {
		dots = []os.DirEntry{
			&dotDirEntry{ctx: ctx, name: ".", real: fullDir, fallback: dirFi},
			&dotDirEntry{ctx: ctx, name: "..", real: filepath.Dir(fullDir), fallback: dirFi},
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
	if hookActive {
		allowed := entries[:0:0]
		for _, e := range entries {
			if _, lerr := checkFSNoFollowLeaf(ctx, baseDir, "ls", filepath.Join(rawDir, e.Name()), OpStat); lerr == nil {
				allowed = append(allowed, e)
			}
		}
		entries = allowed
	}
	entries = append(dots, entries...)
	statCache := map[string]os.FileInfo{}
	stat := func(e os.DirEntry) os.FileInfo {
		if fi, ok := statCache[e.Name()]; ok {
			return fi
		}
		var fi os.FileInfo
		if de, ok := e.(*dotDirEntry); ok {
			fi, _ = de.Info()
		} else if !hookActive {
			fi, _ = e.Info()
		} else {
			fi, _ = fsLstat(ctx, baseDir, "ls", filepath.Join(rawDir, e.Name()))
		}
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

func printLsEntry(ctx context.Context, hc interp.HandlerContext, baseDir, rawEntry, display, _ string, fi os.FileInfo, long, human, classify, quoted bool, timeStyle string) error {
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
		_, err := fmt.Fprintln(hc.Stdout, wrap(name))
		return err
	}
	// GNU `ls -l` renders symlinks as "name -> target".
	if fi.Mode()&os.ModeSymlink != 0 {
		if target, err := fsReadlink(ctx, baseDir, "ls", rawEntry); err == nil {
			name += " -> " + target
		}
	}
	_, err := fmt.Fprintf(hc.Stdout, "%s %d %s %s %s %s %s\n",
		lsPerm(fi), 1, lsUser(), lsGroup(), lsSize(fi, human),
		lsTimeFormat(fi.ModTime(), timeStyle), wrap(name))
	return err
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

func printLsEntryInode(ctx context.Context, hc interp.HandlerContext, baseDir, rawEntry, display, full string, fi os.FileInfo, long, human, classify, quoted, inode bool, timeStyle string) error {
	lookupIno := func() string {
		if display == "." || display == ".." {
			if _, err := fsStat(ctx, "", "ls", full); err == nil {
				return fmt.Sprintf("%d", inodeOf(full))
			}
			return "?"
		}
		if _, err := fsLstat(ctx, baseDir, "ls", rawEntry); err == nil {
			return fmt.Sprintf("%d", inodeOf(full))
		}
		return "?"
	}
	if inode && !long {
		_, err := fmt.Fprintf(hc.Stdout, "%s %s\n", lookupIno(), lsDisplayName(display, full, fi, classify, quoted))
		return err
	}
	if inode && long {
		if _, err := fmt.Fprintf(hc.Stdout, "%s ", lookupIno()); err != nil {
			return err
		}
		return printLsEntry(ctx, hc, baseDir, rawEntry, display, full, fi, long, human, classify, quoted, timeStyle)
	}
	return printLsEntry(ctx, hc, baseDir, rawEntry, display, full, fi, long, human, classify, quoted, timeStyle)
}
