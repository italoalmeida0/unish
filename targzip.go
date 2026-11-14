package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

func cmdTar(ctx context.Context, hc interp.HandlerContext, args []string) error {
	rawArgs := args[1:]
	if len(rawArgs) > 0 && !strings.HasPrefix(rawArgs[0], "-") && !strings.Contains(rawArgs[0], "=") {
		rawArgs = append([]string{"-" + rawArgs[0]}, rawArgs[1:]...)
	}
	// Always split combined shorts (-cf, -xzf) and normalize --long=val;
	// without this `tar -cf t.tar --exclude=X dir` misparses -cf and
	// treats --exclude as a positional path (real-agent failure).
	rawArgs = splitAttached("tar", append([]string{"tar"}, rawArgs...))[1:]
	fs := newFlagSet("tar", hc.Stderr)
	create := fs.Bool("c", false, "")
	extract := fs.Bool("x", false, "")
	list := fs.Bool("t", false, "")
	zip := fs.Bool("z", false, "")
	verb := fs.Bool("v", false, "")
	keepOld := fs.Bool("k", false, "")
	fs.BoolVar(keepOld, "keep-old-files", false, "")
	absNames := fs.Bool("P", false, "")
	fs.BoolVar(absNames, "absolute-names", false, "")
	excludes := []string{}
	fs.Var(stringListFlag(&excludes), "exclude", "")
	file := fs.String("f", "", "")
	dir := fs.String("C", "", "")
	if err := fs.Parse(rawArgs); err != nil {
		return err
	}
	nops := 0
	for _, b := range []bool{*create, *extract, *list} {
		if b {
			nops++
		}
	}
	if nops != 1 {
		fmt.Fprintln(hc.Stderr, "tar: need exactly one of -c, -x, -t")
		return flag.ErrHelp
	}
	if *file == "" {
		fmt.Fprintln(hc.Stderr, "tar: -f is required")
		return flag.ErrHelp
	}
	base := hc.Dir
	if *dir != "" {
		base = resolve(hc.Dir, *dir)
		fi, err := os.Stat(base)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "tar: %s: Cannot open: No such file or directory\n", *dir)
			fmt.Fprintln(hc.Stderr, "tar: Error is not recoverable: exiting now")
			return exitError{2}
		}
		if !fi.IsDir() {
			fmt.Fprintf(hc.Stderr, "tar: %s: Cannot open: Not a directory\n", *dir)
			fmt.Fprintln(hc.Stderr, "tar: Error is not recoverable: exiting now")
			return exitError{2}
		}
	}
	archive := resolve(hc.Dir, *file)
	switch {
	case *create:
		return tarCreate(ctx, hc, archive, base, filterTarExcludes(fs.Args(), excludes), excludes, *zip, *verb, *absNames)
	case *extract:
		return tarExtract(ctx, hc, archive, base, fs.Args(), excludes, *zip, *verb, *keepOld, *absNames)
	default:
		return tarList(ctx, hc, archive, fs.Args(), excludes, *zip, *verb)
	}
}

func tarOpenWriter(hc interp.HandlerContext, archive string, zip bool) (io.Closer, *gzip.Writer, *tar.Writer, error) {
	var f *os.File
	raw := hc.Stdout
	if archive != "-" {
		var err error
		f, err = os.Create(archive)
		if err != nil {
			return nil, nil, nil, err
		}
		raw = f
	}
	// tar writes 512-byte blocks; a large buffer keeps stdout and file
	// targets fast.
	bw := bufio.NewWriterSize(raw, 256*1024)
	var gz *gzip.Writer
	w := io.Writer(bw)
	if zip {
		gz = gzip.NewWriter(bw)
		w = gz
	}
	tw := tar.NewWriter(w)
	return &tarWriteCloser{bw: bw, f: f}, gz, tw, nil
}

// tarWriteCloser flushes the buffered archive and closes the underlying
// file, if any ("-" archives are stdout and must not be closed).
type tarWriteCloser struct {
	bw *bufio.Writer
	f  *os.File
}

func (c *tarWriteCloser) Close() error {
	err := c.bw.Flush()
	if c.f != nil {
		if e := c.f.Close(); err == nil {
			err = e
		}
	}
	return err
}

func closeTarWriters(tw *tar.Writer, gz *gzip.Writer, f io.Closer) error {
	err := tw.Close()
	if gz != nil {
		if e := gz.Close(); err == nil {
			err = e
		}
	}
	if e := f.Close(); err == nil {
		err = e
	}
	return err
}

func tarCreate(ctx context.Context, hc interp.HandlerContext, archive, base string, paths, excludes []string, zip, verb, absNames bool) error {
	if len(paths) == 0 {
		// GNU: excluding everything yields an empty (valid) archive, rc 0.
		f, gz, tw, err := tarOpenWriter(hc, archive, zip)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "tar:", err)
			return exitError{1}
		}
		if cerr := closeTarWriters(tw, gz, f); cerr != nil {
			if isPipeClosed(cerr) {
				return cerr
			}
			fmt.Fprintln(hc.Stderr, "tar:", cerr)
			return exitError{1}
		}
		return nil
	}
	f, gz, tw, err := tarOpenWriter(hc, archive, zip)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tar:", err)
		return exitError{1}
	}
	var archFi os.FileInfo
	if archive != "-" {
		archFi, _ = os.Stat(archive)
	}
	code := 0
	// Identity lookups read /etc/passwd+group; do them once per run,
	// not once per archived file. Same for the copy buffer: io.Copy
	// would allocate 32KiB per file.
	_, uname, _, gname := identity()
	copyBuf := make([]byte, 256*1024)
	verbOut := hc.Stdout
	if archive == "-" {
		// Never corrupt binary archive data written to stdout.
		verbOut = hc.Stderr
	}
	for _, p := range paths {
		if ctx.Err() != nil {
			_ = closeTarWriters(tw, gz, f)
			return ctx.Err()
		}
		full := resolve(base, p)
		err := filepath.Walk(full, func(path string, info os.FileInfo, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return err
			}
			// Never archive the output archive file into itself.
			if archFi != nil && os.SameFile(info, archFi) {
				return nil
			}
			rel := path
			if base != "." {
				if r, rerr := filepath.Rel(base, path); rerr == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
					rel = r
				} else if !absNames {
					rel = stripLeadingRoot(path)
				}
			}
			rel = filepath.ToSlash(rel)
			if !absNames && strings.HasPrefix(rel, "/") {
				rel = strings.TrimLeft(rel, "/")
			}
			if rel == "" {
				rel = "."
			}
			if tarPathExcluded(rel, excludes) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			linkTarget := ""
			if info.Mode()&os.ModeSymlink != 0 {
				lt, lerr := os.Readlink(path)
				if lerr != nil {
					return lerr
				}
				linkTarget = filepath.ToSlash(lt)
			}
			hdr, err := tar.FileInfoHeader(info, linkTarget)
			if err != nil {
				return err
			}
			hdr.Name = rel
			if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
				hdr.Name += "/"
			}
			// GNU tar stores user/group names; Go leaves them empty.
			// Fill from the current identity for -tv parity.
			if hdr.Uname == "" {
				hdr.Uname = uname
			}
			if hdr.Gname == "" {
				hdr.Gname = gname
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				fh, err := os.Open(path)
				if err != nil {
					return err
				}
				_, err = io.CopyBuffer(tw, fh, copyBuf)
				cerr := fh.Close()
				if err != nil {
					return err
				}
				if cerr != nil {
					return cerr
				}
			}
			if verb {
				fmt.Fprintln(verbOut, hdr.Name)
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				_ = closeTarWriters(tw, gz, f)
				return ctx.Err()
			}
			if isPipeClosed(err) {
				_ = closeTarWriters(tw, gz, f)
				return err
			}
			fmt.Fprintf(hc.Stderr, "tar: %v\n", err)
			code = 1
		}
	}
	if cerr := closeTarWriters(tw, gz, f); cerr != nil {
		if isPipeClosed(cerr) {
			return cerr
		}
		fmt.Fprintf(hc.Stderr, "tar: %v\n", cerr)
		code = 1
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func stripLeadingRoot(p string) string {
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	return strings.TrimLeft(rest, `/\`)
}

// nopReadWriteCloser adapts a plain reader (like stdin) to the handle
// shape tarOpenReader returns.
type nopReadWriteCloser struct{ io.Reader }

func (nopReadWriteCloser) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }
func (nopReadWriteCloser) Close() error                { return nil }

// tarOpenReader opens the archive for reading; "-" is stdin, like GNU
// tar. Gzip input is sniffed transparently; the sniffed bytes are
// re-prepended so no seeking/reopening is needed (works for pipes).
func tarOpenReader(hc interp.HandlerContext, archive string, zip bool) (io.ReadWriteCloser, *gzip.Reader, *tar.Reader, error) {
	var f io.ReadWriteCloser
	if archive == "-" {
		f = nopReadWriteCloser{hc.Stdin}
	} else {
		var err error
		f, err = openShellFile(archive)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	var gz *gzip.Reader
	var r io.Reader = f
	if !zip {
		var magic [2]byte
		n, _ := io.ReadFull(f, magic[:])
		if n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
			zip = true
		}
		if n > 0 {
			r = io.MultiReader(bytes.NewReader(magic[:n]), f)
		}
	}
	if zip {
		var err error
		gz, err = gzip.NewReader(r)
		if err != nil {
			f.Close()
			return nil, nil, nil, err
		}
		r = gz
	}
	return f, gz, tar.NewReader(r), nil
}

type deferredHardLink struct {
	hdrName string
	src     string
	target  string
}

func tarExtract(ctx context.Context, hc interp.HandlerContext, archive, base string, only, excludes []string, zip, verb, keepOld, absNames bool) error {
	f, gz, tr, err := tarOpenReader(hc, archive, zip)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tar:", err)
		return exitError{1}
	}
	defer f.Close()
	if gz != nil {
		defer gz.Close()
	}
	matchedOnly := make([]bool, len(only))
	var pendingLinks []deferredHardLink
	warnedLeadingSlash := false
	code := 0

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(hc.Stderr, "tar:", err)
			return exitError{1}
		}
		if len(only) > 0 && !tarMatchOnly(hdr.Name, only, matchedOnly) {
			continue
		}
		if tarPathExcluded(hdr.Name, excludes) {
			continue
		}
		target := ""
		if absNames && (filepath.IsAbs(filepath.FromSlash(hdr.Name)) || strings.HasPrefix(hdr.Name, "/")) {
			target = filepath.Clean(resolve(base, filepath.FromSlash(hdr.Name)))
		} else {
			name := filepath.FromSlash(hdr.Name)
			if filepath.IsAbs(name) || strings.HasPrefix(hdr.Name, "/") || filepath.VolumeName(name) != "" {
				if !warnedLeadingSlash {
					fmt.Fprintf(hc.Stderr, "tar: removing leading '/' from member names\n")
					warnedLeadingSlash = true
				}
				name = stripLeadingRoot(name)
			}
			if name == "" || name == "." {
				if verb {
					fmt.Fprintln(hc.Stdout, hdr.Name)
				}
				continue
			}
			target = filepath.Join(base, name)
			if !isWithinDir(base, target) || !isSafeExtractAncestors(base, target) {
				fmt.Fprintf(hc.Stderr, "tar: skipping '%s': outside destination\n", hdr.Name)
				code = 2
				continue
			}
		}

		mode := os.FileMode(hdr.Mode)
		if mode == 0 {
			if hdr.Typeflag == tar.TypeDir {
				mode = 0o755
			} else {
				mode = 0o644
			}
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode.Perm()|0o700); err != nil {
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
		case tar.TypeSymlink:
			linkRaw := filepath.FromSlash(hdr.Linkname)
			if !absNames {
				if filepath.IsAbs(linkRaw) || strings.HasPrefix(hdr.Linkname, "/") || filepath.VolumeName(linkRaw) != "" {
					fmt.Fprintf(hc.Stderr, "tar: skipping '%s': symlink target outside destination\n", hdr.Name)
					code = 2
					continue
				}
				resolvedLink := filepath.Join(filepath.Dir(target), linkRaw)
				if !isWithinDir(base, resolvedLink) {
					fmt.Fprintf(hc.Stderr, "tar: skipping '%s': symlink target outside destination\n", hdr.Name)
					code = 2
					continue
				}
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
			if lfi, lerr := os.Lstat(target); lerr == nil {
				if keepOld {
					continue
				}
				if lfi.IsDir() {
					fmt.Fprintf(hc.Stderr, "tar: %s: Cannot open: Is a directory\n", hdr.Name)
					code = 1
					continue
				}
				_ = os.Remove(target)
			}
			if err := os.Symlink(linkRaw, target); err != nil {
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
		case tar.TypeLink:
			linkName := filepath.FromSlash(hdr.Linkname)
			var linkSrc string
			if absNames && (filepath.IsAbs(linkName) || strings.HasPrefix(hdr.Linkname, "/")) {
				linkSrc = filepath.Clean(resolve(base, linkName))
			} else {
				linkName = stripLeadingRoot(linkName)
				linkSrc = filepath.Join(base, linkName)
				if !isWithinDir(base, linkSrc) || !isSafeExtractAncestors(base, linkSrc) {
					fmt.Fprintf(hc.Stderr, "tar: skipping '%s': link target outside destination\n", hdr.Name)
					code = 2
					continue
				}
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
			if lfi, lerr := os.Lstat(target); lerr == nil {
				if keepOld {
					continue
				}
				if lfi.IsDir() {
					fmt.Fprintf(hc.Stderr, "tar: %s: Cannot open: Is a directory\n", hdr.Name)
					code = 1
					continue
				}
				_ = os.Remove(target)
			}
			if _, serr := os.Lstat(linkSrc); serr != nil {
				pendingLinks = append(pendingLinks, deferredHardLink{hdrName: hdr.Name, src: linkSrc, target: target})
			} else if err := os.Link(linkSrc, target); err != nil {
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
			if !absNames && !isSafeExtractAncestors(base, target) {
				fmt.Fprintf(hc.Stderr, "tar: skipping '%s': outside destination\n", hdr.Name)
				code = 2
				continue
			}
			if lfi, lerr := os.Lstat(target); lerr == nil {
				if keepOld {
					continue
				}
				if lfi.IsDir() {
					fmt.Fprintf(hc.Stderr, "tar: %s: Cannot open: Is a directory\n", hdr.Name)
					code = 1
					continue
				}
				// Unlink existing file/symlink first so O_CREATE never writes through a symlink.
				_ = os.Remove(target)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
			if err != nil {
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
			_, err = io.Copy(out, tr)
			cerr := out.Close()
			if err != nil {
				_ = os.Remove(target)
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
			if cerr != nil {
				fmt.Fprintln(hc.Stderr, "tar:", cerr)
				return exitError{1}
			}
		}
		if verb {
			fmt.Fprintln(hc.Stdout, hdr.Name)
		}
	}

	for _, pl := range pendingLinks {
		if !absNames && (!isSafeExtractAncestors(base, pl.src) || !isSafeExtractAncestors(base, pl.target)) {
			fmt.Fprintf(hc.Stderr, "tar: skipping '%s': outside destination\n", pl.hdrName)
			code = 2
			continue
		}
		if err := os.Link(pl.src, pl.target); err != nil {
			fmt.Fprintln(hc.Stderr, "tar:", err)
			return exitError{1}
		}
	}

	for i, o := range only {
		if !matchedOnly[i] {
			fmt.Fprintf(hc.Stderr, "tar: %s: Not found in archive\n", o)
			code = 2
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func tarList(ctx context.Context, hc interp.HandlerContext, archive string, only, excludes []string, zip, verb bool) error {
	f, gz, tr, err := tarOpenReader(hc, archive, zip)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tar:", err)
		return exitError{1}
	}
	defer f.Close()
	if gz != nil {
		defer gz.Close()
	}
	matchedOnly := make([]bool, len(only))
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(hc.Stderr, "tar:", err)
			return exitError{1}
		}
		if len(only) > 0 && !tarMatchOnly(hdr.Name, only, matchedOnly) {
			continue
		}
		if tarPathExcluded(hdr.Name, excludes) {
			continue
		}
		var werr error
		if verb {
			user := hdr.Uname
			if user == "" {
				user = fmt.Sprintf("%d", hdr.Uid)
			}
			group := hdr.Gname
			if group == "" {
				group = fmt.Sprintf("%d", hdr.Gid)
			}
			_, werr = fmt.Fprintf(hc.Stdout, "%s %s/%s %9d %s %s\n",
				hdr.FileInfo().Mode(), user, group, hdr.Size,
				hdr.ModTime.Format("2006-01-02 15:04"), hdr.Name)
		} else {
			_, werr = fmt.Fprintln(hc.Stdout, hdr.Name)
		}
		if isPipeClosed(werr) {
			return werr
		}
	}
	code := 0
	for i, o := range only {
		if !matchedOnly[i] {
			fmt.Fprintf(hc.Stderr, "tar: %s: Not found in archive\n", o)
			code = 2
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cleanTarName(s string) string {
	s = filepath.ToSlash(s)
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return "."
	}
	return s
}

func tarMatchOnly(hdrName string, only []string, matched []bool) bool {
	h := cleanTarName(hdrName)
	hit := false
	for i, o := range only {
		w := cleanTarName(o)
		if h == w || w == "." || strings.HasPrefix(h, w+"/") {
			matched[i] = true
			hit = true
		}
	}
	return hit
}

func cmdGzip(_ context.Context, hc interp.HandlerContext, args []string) error {
	return gzipRun(hc, "gzip", args, false, true)
}

func cmdGunzip(_ context.Context, hc interp.HandlerContext, args []string) error {
	return gzipRun(hc, "gunzip", args, true, true)
}

func cmdGzcat(_ context.Context, hc interp.HandlerContext, args []string) error {
	return gzipRun(hc, "gzcat", args, true, false)
}

func gzipRun(hc interp.HandlerContext, name string, args []string, decode, removeOrig bool) error {
	args = expandGzipArgs(args)
	fs := newFlagSet(name, hc.Stderr)
	keep := fs.Bool("k", false, "")
	fs.BoolVar(keep, "keep", false, "")
	stdout := fs.Bool("c", false, "")
	fs.BoolVar(stdout, "stdout", false, "")
	fs.BoolVar(stdout, "to-stdout", false, "")
	force := fs.Bool("f", false, "")
	fs.BoolVar(force, "force", false, "")
	decompress := fs.Bool("d", false, "")
	fs.BoolVar(decompress, "decompress", false, "")
	fs.BoolVar(decompress, "uncompress", false, "")
	_ = fs.Bool("q", false, "")
	_ = fs.Bool("quiet", false, "")
	_ = fs.Bool("v", false, "")
	_ = fs.Bool("verbose", false, "")
	_ = fs.Bool("n", false, "")
	_ = fs.Bool("no-name", false, "")
	for d := '1'; d <= '9'; d++ {
		_ = fs.Bool(string(d), false, "")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *decompress {
		decode = true
	}
	toStdout := *stdout || name == "gzcat"
	files := fs.Args()
	if len(files) == 0 {
		if decode {
			if err := gunzipStream(hc.Stdin, hc.Stdout, *force); err != nil {
				if isPipeClosed(err) {
					return err
				}
				if isGzipFormatError(err) {
					fmt.Fprintf(hc.Stderr, "\ngzip: stdin: not in gzip format\n")
				} else {
					fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
				}
				return exitError{1}
			}
			return nil
		}
		gw := gzip.NewWriter(hc.Stdout)
		_, err := io.Copy(gw, hc.Stdin)
		cerr := gw.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			if isPipeClosed(err) {
				return err
			}
			fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
			return exitError{1}
		}
		return nil
	}
	code := 0
	for _, p := range files {
		if p == "-" {
			if decode {
				if err := gunzipStream(hc.Stdin, hc.Stdout, *force); err != nil {
					if isPipeClosed(err) {
						return err
					}
					fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
					code = 1
				}
			} else {
				gw := gzip.NewWriter(hc.Stdout)
				_, err := io.Copy(gw, hc.Stdin)
				if cerr := gw.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					if isPipeClosed(err) {
						return err
					}
					fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
					code = 1
				}
			}
			continue
		}
		full := resolve(hc.Dir, p)
		sfi, serr := os.Stat(full)
		if serr != nil {
			fmt.Fprintf(hc.Stderr, "%s: %v\n", name, serr)
			code = 1
			continue
		}
		if sfi.IsDir() {
			fmt.Fprintf(hc.Stderr, "%s: %s is a directory -- ignored\n", name, p)
			code = 1
			continue
		}
		if decode {
			out := strings.TrimSuffix(p, ".gz")
			if out == p {
				out = p + ".out"
			}
			dst := resolve(hc.Dir, out)
			if !toStdout && !*force {
				if _, lerr := os.Lstat(dst); lerr == nil {
					fmt.Fprintf(hc.Stderr, "%s: %s already exists; not overwritten\n", name, out)
					code = 1
					continue
				}
			}
			if err := gunzipFile(hc, full, dst, toStdout, *force); err != nil {
				if isPipeClosed(err) {
					return err
				}
				// GNU wording: "gzip: bad.gz: not in gzip format".
				if isGzipFormatError(err) {
					// GNU emits a leading blank line before this diagnostic.
					fmt.Fprintf(hc.Stderr, "\ngzip: %s: not in gzip format\n", p)
				} else {
					fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
				}
				code = 1
				continue
			}
			if removeOrig && !*keep && !toStdout {
				if rerr := os.Remove(full); rerr != nil {
					fmt.Fprintf(hc.Stderr, "%s: %v\n", name, rerr)
					code = 1
				}
			}
		} else {
			outDisp := p + ".gz"
			out := full + ".gz"
			if !toStdout && !*force {
				if _, lerr := os.Lstat(out); lerr == nil {
					fmt.Fprintf(hc.Stderr, "%s: %s already exists; not overwritten\n", name, outDisp)
					code = 1
					continue
				}
			}
			if err := gzipFile(hc, full, out, toStdout, *force); err != nil {
				if isPipeClosed(err) {
					return err
				}
				fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
				code = 1
				continue
			}
			if removeOrig && !*keep && !toStdout {
				if rerr := os.Remove(full); rerr != nil {
					fmt.Fprintf(hc.Stderr, "%s: %v\n", name, rerr)
					code = 1
				}
			}
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

var errNotGzipFormat = errors.New("not in gzip format")

func expandGzipArgs(args []string) []string {
	if len(args) <= 1 {
		return args
	}
	const shorts = "kcdfqvn123456789"
	var flags, rest []string
	flags = append(flags, args[0])
	endFlags := false
	for _, a := range args[1:] {
		if endFlags {
			rest = append(rest, a)
			continue
		}
		if a == "--" {
			endFlags = true
			rest = append(rest, a)
			continue
		}
		if len(a) > 2 && a[0] == '-' && a[1] != '-' {
			allKnown := true
			for i := 1; i < len(a); i++ {
				if !strings.ContainsRune(shorts, rune(a[i])) {
					allKnown = false
					break
				}
			}
			if allKnown {
				for i := 1; i < len(a); i++ {
					flags = append(flags, "-"+string(a[i]))
				}
				continue
			}
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func gunzipStream(r io.Reader, w io.Writer, forcePassthrough bool) error {
	var magic [2]byte
	n, rerr := io.ReadFull(r, magic[:])
	if n == 0 && (rerr == io.EOF || rerr == io.ErrUnexpectedEOF) {
		if forcePassthrough {
			return nil
		}
		return errNotGzipFormat
	}
	combined := io.MultiReader(bytes.NewReader(magic[:n]), r)
	if forcePassthrough && (n < 2 || magic[0] != 0x1f || magic[1] != 0x8b) {
		_, err := io.Copy(w, combined)
		return err
	}
	gr, err := gzip.NewReader(combined)
	if err != nil {
		return errNotGzipFormat
	}
	defer gr.Close()
	if _, err := io.Copy(w, gr); err != nil {
		return err
	}
	return gr.Close()
}

func gzipFile(hc interp.HandlerContext, src, dst string, toStdout, force bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if toStdout {
		gw := gzip.NewWriter(hc.Stdout)
		_, err = io.Copy(gw, in)
		if cerr := gw.Close(); err == nil {
			err = cerr
		}
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".unish-gzip-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	gw := gzip.NewWriter(tmp)
	_, err = io.Copy(gw, in)
	gwErr := gw.Close()
	cerr := tmp.Close()
	if err == nil {
		err = gwErr
	}
	if err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	_ = in.Close()
	if force {
		_ = os.Chmod(dst, 0o666)
		_ = os.Remove(dst)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func gunzipFile(hc interp.HandlerContext, src, dst string, toStdout, force bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if toStdout {
		return gunzipStream(in, hc.Stdout, force)
	}
	gr, err := gzip.NewReader(in)
	if err != nil {
		return errNotGzipFormat
	}
	defer gr.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".unish-gunzip-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, err = io.Copy(tmp, gr)
	grErr := gr.Close()
	cerr := tmp.Close()
	if err == nil {
		err = grErr
	}
	if err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	_ = in.Close()
	if force {
		_ = os.Chmod(dst, 0o666)
		_ = os.Remove(dst)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// isGzipFormatError reports whether err is a gzip magic-header error
// (corrupt/non-gzip input), which GNU reports as "not in gzip format".
func isGzipFormatError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errNotGzipFormat) || err == io.EOF {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid header")
}

// isWithinDir reports whether target resides inside base (after cleaning).
func isWithinDir(base, target string) bool {
	absBase, err1 := filepath.Abs(base)
	absTarget, err2 := filepath.Abs(target)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isSafeExtractAncestors verifies that no existing directory/symlink ancestor
// between base and filepath.Dir(target) resolves outside base's real path.
func isSafeExtractAncestors(base, target string) bool {
	absBase, err1 := filepath.Abs(base)
	absTarget, err2 := filepath.Abs(target)
	if err1 != nil || err2 != nil {
		return false
	}
	realBase := absBase
	if rb, err := filepath.EvalSymlinks(absBase); err == nil {
		realBase = rb
	}
	rel, err := filepath.Rel(absBase, filepath.Dir(absTarget))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if rel == "." {
		return true
	}
	cur := absBase
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		lfi, lerr := os.Lstat(cur)
		if lerr != nil {
			// Remaining deeper components do not exist on disk yet.
			break
		}
		realCur, rerr := filepath.EvalSymlinks(cur)
		if rerr != nil {
			return false
		}
		if !isWithinDir(realBase, realCur) {
			return false
		}
		_ = lfi
	}
	return true
}

func tarPathExcluded(rel string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	clean := cleanTarName(rel)
	if clean == "." {
		return false
	}
	parts := strings.Split(clean, "/")
	baseName := parts[len(parts)-1]
	for _, raw := range excludes {
		e := strings.Trim(raw, "'\"")
		e = strings.TrimSuffix(filepath.ToSlash(e), "/")
		e = strings.TrimPrefix(e, "./")
		if e == "" {
			continue
		}
		if clean == e || strings.HasPrefix(clean, e+"/") || baseName == e {
			return true
		}
		if ok, _ := filepath.Match(e, baseName); ok {
			return true
		}
		if ok, _ := filepath.Match(e, clean); ok {
			return true
		}
		for _, seg := range parts {
			if seg == e {
				return true
			}
			if ok, _ := filepath.Match(e, seg); ok {
				return true
			}
		}
	}
	return false
}

func filterTarExcludes(paths, excludes []string) []string {
	if len(excludes) == 0 {
		return paths
	}
	var out []string
	for _, p := range paths {
		if !tarPathExcluded(p, excludes) {
			out = append(out, p)
		}
	}
	return out
}
