package main

import (
	"archive/tar"
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
	fs := newFlagSet("tar", hc.Stderr)
	create := fs.Bool("c", false, "")
	extract := fs.Bool("x", false, "")
	list := fs.Bool("t", false, "")
	zip := fs.Bool("z", false, "")
	verb := fs.Bool("v", false, "")
	file := fs.String("f", "", "")
	dir := fs.String("C", "", "")
	if err := fs.Parse(args[1:]); err != nil {
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
	}
	archive := resolve(hc.Dir, *file)
	switch {
	case *create:
		return tarCreate(hc, archive, base, fs.Args(), *zip, *verb)
	case *extract:
		return tarExtract(hc, archive, base, fs.Args(), *zip, *verb)
	default:
		return tarList(hc, archive, *zip, *verb)
	}
}

func tarOpenWriter(hc interp.HandlerContext, archive string, zip bool) (io.WriteCloser, *gzip.Writer, *tar.Writer, error) {
	f, err := os.Create(archive)
	if err != nil {
		return nil, nil, nil, err
	}
	var gz *gzip.Writer
	w := io.Writer(f)
	_ = hc
	if zip {
		gz = gzip.NewWriter(f)
		w = gz
	}
	tw := tar.NewWriter(w)
	return f, gz, tw, nil
}

func tarCreate(hc interp.HandlerContext, archive, base string, paths []string, zip, verb bool) error {
	if len(paths) == 0 {
		fmt.Fprintln(hc.Stderr, "tar: missing operand")
		return flag.ErrHelp
	}
	f, gz, tw, err := tarOpenWriter(hc, archive, zip)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tar:", err)
		return exitError{1}
	}
	code := 0
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
			rel, err := filepath.Rel(base, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = rel
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
				fmt.Fprintln(hc.Stdout, rel)
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

func tarOpenReader(archive string, zip bool) (io.ReadCloser, *gzip.Reader, *tar.Reader, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, nil, nil, err
	}
	var gz *gzip.Reader
	r := io.Reader(f)
	if zip {
		gz, err = gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, nil, err
		}
		r = gz
	}
	return f, gz, tar.NewReader(r), nil
}

func tarExtract(hc interp.HandlerContext, archive, base string, only []string, zip, verb bool) error {
	f, gz, tr, err := tarOpenReader(archive, zip)
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
		target := filepath.Join(base, filepath.FromSlash(hdr.Name))
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
	f, gz, tr, err := tarOpenReader(archive, zip)
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
			fmt.Fprintf(hc.Stdout, "%s %8d %s\n", hdr.FileInfo().Mode(), hdr.Size, hdr.Name)
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
					fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
					code = 1
				}
				continue
			}
			if removeOrig && !*keep && !*stdout && name != "gzcat" {
				os.Remove(full)
			}
		} else {
			out := full + ".gz"
			if err := gzipFile(full, out, *stdout); err != nil {
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

func gzipFile(src, dst string, toStdout bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if toStdout {
		gw := gzip.NewWriter(os.Stdout)
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
