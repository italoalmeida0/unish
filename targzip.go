package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

func cmdTar(_ context.Context, hc interp.HandlerContext, args []string) error {
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
		return tarCreate(hc, archive, base, filterTarExcludes(fs.Args(), excludes), *zip, *verb)
	case *extract:
		return tarExtract(hc, archive, base, fs.Args(), *zip, *verb)
	default:
		return tarList(hc, archive, *zip, *verb)
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

func tarCreate(hc interp.HandlerContext, archive, base string, paths []string, zip, verb bool) error {
	if len(paths) == 0 {
		// GNU: excluding everything yields an empty (valid) archive, rc 0.
		f, gz, tw, err := tarOpenWriter(hc, archive, zip)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "tar:", err)
			return exitError{1}
		}
		tw.Close()
		if gz != nil {
			gz.Close()
		}
		f.Close()
		return nil
	}
	f, gz, tw, err := tarOpenWriter(hc, archive, zip)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tar:", err)
		return exitError{1}
	}
	code := 0
	// Identity lookups read /etc/passwd+group; do them once per run,
	// not once per archived file.
	_, uname, _, gname := identity()
	closeAll := func() {
		tw.Close()
		if gz != nil {
			gz.Close()
		}
		f.Close()
	}
	for _, p := range paths {
		full := filepath.Join(base, p)
		err := filepath.Walk(full, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel := path
			if base != "." {
				if r, err := filepath.Rel(base, path); err == nil {
					rel = r
				}
			}
			rel = filepath.ToSlash(rel)
			hdr, err := tar.FileInfoHeader(info, "")
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
			if !info.IsDir() {
				fh, err := os.Open(path)
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, fh)
				fh.Close()
				if err != nil {
					return err
				}
			}
			if verb {
				fmt.Fprintln(hc.Stdout, hdr.Name)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(hc.Stderr, "tar: %v\n", err)
			code = 1
		}
	}
	closeAll()
	if code != 0 {
		return exitError{code}
	}
	return nil
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
		if n, _ := io.ReadFull(f, magic[:]); n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
			zip = true
		}
		r = io.MultiReader(bytes.NewReader(magic[:]), f)
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

func tarExtract(hc interp.HandlerContext, archive, base string, only []string, zip, verb bool) error {
	f, gz, tr, err := tarOpenReader(hc, archive, zip)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tar:", err)
		return exitError{1}
	}
	defer f.Close()
	if gz != nil {
		defer gz.Close()
	}
	want := map[string]bool{}
	for _, o := range only {
		want[filepath.ToSlash(o)] = true
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(hc.Stderr, "tar:", err)
			return exitError{1}
		}
		if len(want) > 0 && !want[hdr.Name] {
			continue
		}
		// Zip-Slip defense (GNU strips leading "/" and "../"): refuse
		// entries that would escape the destination directory.
		name := filepath.FromSlash(hdr.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
			fmt.Fprintf(hc.Stderr, "tar: removing leading '/' from member names\n")
			name = strings.TrimLeft(name, "/\\")
		}
		target := filepath.Join(base, name)
		if !isWithinDir(base, target) {
			fmt.Fprintf(hc.Stderr, "tar: skipping '%s': outside destination\n", hdr.Name)
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, os.FileMode(hdr.Mode))
		default:
			os.MkdirAll(filepath.Dir(target), 0o755)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
			_, err = io.Copy(out, tr)
			out.Close()
			if err != nil {
				fmt.Fprintln(hc.Stderr, "tar:", err)
				return exitError{1}
			}
		}
		if verb {
			fmt.Fprintln(hc.Stdout, hdr.Name)
		}
	}
	return nil
}

func tarList(hc interp.HandlerContext, archive string, zip, verb bool) error {
	f, gz, tr, err := tarOpenReader(hc, archive, zip)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tar:", err)
		return exitError{1}
	}
	defer f.Close()
	if gz != nil {
		defer gz.Close()
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(hc.Stderr, "tar:", err)
			return exitError{1}
		}
		if verb {
			user := hdr.Uname
			if user == "" {
				user = fmt.Sprintf("%d", hdr.Uid)
			}
			group := hdr.Gname
			if group == "" {
				group = fmt.Sprintf("%d", hdr.Gid)
			}
			fmt.Fprintf(hc.Stdout, "%s %s/%s %9d %s %s\n",
				hdr.FileInfo().Mode(), user, group, hdr.Size,
				hdr.ModTime.Format("2006-01-02 15:04"), hdr.Name)
		} else {
			fmt.Fprintln(hc.Stdout, hdr.Name)
		}
	}
	return nil
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
	fs := newFlagSet(name, hc.Stderr)
	keep := fs.Bool("k", false, "")
	fs.BoolVar(keep, "keep", false, "")
	stdout := fs.Bool("c", false, "")
	fs.BoolVar(stdout, "stdout", false, "")
	force := fs.Bool("f", false, "")
	fs.BoolVar(force, "force", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) == 0 {
		if decode {
			gr, err := gzip.NewReader(hc.Stdin)
			if err != nil {
				fmt.Fprintln(hc.Stderr, name+":", err)
				return exitError{1}
			}
			defer gr.Close()
			_, err = io.Copy(hc.Stdout, gr)
			return err
		}
		gw := gzip.NewWriter(hc.Stdout)
		_, err := io.Copy(gw, hc.Stdin)
		gw.Close()
		return err
	}
	code := 0
	for _, p := range files {
		full := resolve(hc.Dir, p)
		if decode {
			out := strings.TrimSuffix(p, ".gz")
			if out == p {
				out = p + ".out"
			}
			if err := gunzipFile(hc, full, resolve(hc.Dir, out), *stdout || name == "gzcat"); err != nil {
				if !*force {
					// GNU wording: "gzip: bad.gz: not in gzip format".
					if isGzipFormatError(err) {
						// GNU emits a leading blank line before this diagnostic.
						fmt.Fprintf(hc.Stderr, "\ngzip: %s: not in gzip format\n", p)
					} else {
						fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
					}
					code = 1
				}
				continue
			}
			if removeOrig && !*keep && !*stdout && name != "gzcat" {
				os.Remove(full)
			}
		} else {
			out := full + ".gz"
			if err := gzipFile(hc, full, out, *stdout); err != nil {
				fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
				code = 1
				continue
			}
			if removeOrig && !*keep && !*stdout {
				os.Remove(full)
			}
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func gzipFile(hc interp.HandlerContext, src, dst string, toStdout bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if toStdout {
		gw := gzip.NewWriter(hc.Stdout)
		_, err = io.Copy(gw, in)
		gw.Close()
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	gw := gzip.NewWriter(out)
	_, err = io.Copy(gw, in)
	gw.Close()
	cerr := out.Close()
	if err != nil {
		return err
	}
	return cerr
}

func gunzipFile(hc interp.HandlerContext, src, dst string, toStdout bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	gr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer gr.Close()
	if toStdout {
		_, err = io.Copy(hc.Stdout, gr)
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, gr)
	cerr := out.Close()
	if err != nil {
		return err
	}
	return cerr
}

// isGzipFormatError reports whether err is a gzip magic-header error
// (corrupt/non-gzip input), which GNU reports as "not in gzip format".
func isGzipFormatError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid header") ||
		strings.Contains(msg, "unexpected EOF") ||
		strings.Contains(msg, "unexpected end")
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

func filterTarExcludes(paths, excludes []string) []string {
	if len(excludes) == 0 {
		return paths
	}
	var out []string
	for _, p := range paths {
		skip := false
		for _, e := range excludes {
			e = strings.Trim(e, "'\"")
			if e == "" {
				continue
			}
			if p == e || strings.HasPrefix(p, strings.TrimSuffix(e, "/")+"/") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, p)
		}
	}
	return out
}
