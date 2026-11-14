package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

type awkInitAssign struct {
	name string
	val  awkVal
}

func cmdAwk(ctx context.Context, hc interp.HandlerContext, args []string) error {
	cmdName := "awk"
	if len(args) > 0 && args[0] != "" {
		cmdName = args[0]
	}

	var initAssigns []awkInitAssign
	var progSources []string
	hasExplicitProg := false

	i := 1
	for i < len(args) {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if a == "-" || len(a) == 0 || a[0] != '-' {
			break
		}

		// Long options
		if strings.HasPrefix(a, "--") {
			switch {
			case a == "--help":
				fmt.Fprintf(hc.Stdout, "Usage: %s [POSIX or GNU style options] [-f progfile] [--] 'program' [file ...]\n", cmdName)
				return nil
			case a == "--posix" || a == "--traditional" || a == "--compat" ||
				a == "--re-interval" || a == "--characters-as-bytes" || a == "--non-decimal-data":
				i++
				continue
			case a == "--field-separator":
				if i+1 >= len(args) {
					fmt.Fprintf(hc.Stderr, "%s: option '--field-separator' requires an argument\n", cmdName)
					return exitError{code: 2}
				}
				initAssigns = append(initAssigns, awkInitAssign{name: "FS", val: awkStrVal(awkUnescapeC(args[i+1]))})
				i += 2
				continue
			case strings.HasPrefix(a, "--field-separator="):
				val := strings.TrimPrefix(a, "--field-separator=")
				initAssigns = append(initAssigns, awkInitAssign{name: "FS", val: awkStrVal(awkUnescapeC(val))})
				i++
				continue
			case a == "--assign":
				if i+1 >= len(args) {
					fmt.Fprintf(hc.Stderr, "%s: option '--assign' requires an argument\n", cmdName)
					return exitError{code: 2}
				}
				asgn, err := parseAwkFlagAssign(args[i+1])
				if err != nil {
					fmt.Fprintf(hc.Stderr, "%s: %v\n", cmdName, err)
					return exitError{code: 2}
				}
				initAssigns = append(initAssigns, asgn)
				i += 2
				continue
			case strings.HasPrefix(a, "--assign="):
				asgn, err := parseAwkFlagAssign(strings.TrimPrefix(a, "--assign="))
				if err != nil {
					fmt.Fprintf(hc.Stderr, "%s: %v\n", cmdName, err)
					return exitError{code: 2}
				}
				initAssigns = append(initAssigns, asgn)
				i++
				continue
			case a == "--file":
				if i+1 >= len(args) {
					fmt.Fprintf(hc.Stderr, "%s: option '--file' requires an argument\n", cmdName)
					return exitError{code: 2}
				}
				src, err := loadAwkProgFile(hc, args[i+1])
				if err != nil {
					fmt.Fprintf(hc.Stderr, "%s: can't open file %s: %v\n", cmdName, args[i+1], err)
					return exitError{code: 2}
				}
				progSources = append(progSources, src)
				hasExplicitProg = true
				i += 2
				continue
			case strings.HasPrefix(a, "--file="):
				fname := strings.TrimPrefix(a, "--file=")
				src, err := loadAwkProgFile(hc, fname)
				if err != nil {
					fmt.Fprintf(hc.Stderr, "%s: can't open file %s: %v\n", cmdName, fname, err)
					return exitError{code: 2}
				}
				progSources = append(progSources, src)
				hasExplicitProg = true
				i++
				continue
			case a == "--source":
				if i+1 >= len(args) {
					fmt.Fprintf(hc.Stderr, "%s: option '--source' requires an argument\n", cmdName)
					return exitError{code: 2}
				}
				progSources = append(progSources, args[i+1])
				hasExplicitProg = true
				i += 2
				continue
			case strings.HasPrefix(a, "--source="):
				progSources = append(progSources, strings.TrimPrefix(a, "--source="))
				hasExplicitProg = true
				i++
				continue
			default:
				fmt.Fprintf(hc.Stderr, "%s: unrecognized option '%s'\n", cmdName, a)
				return exitError{code: 2}
			}
		}

		// Short options (-F, -v, -f, -e, -W, -b, etc., possibly clustered or attached)
		j := 1
		for j < len(a) {
			ch := a[j]
			switch ch {
			case 'b', 'c', 'P', 'S':
				// gawk compatibility boolean flags (-b bytes, -c traditional, -P posix, -S sandbox)
				j++
			case 'F':
				var fsArg string
				if j+1 < len(a) {
					fsArg = a[j+1:]
				} else {
					if i+1 >= len(args) {
						fmt.Fprintf(hc.Stderr, "%s: option requires an argument -- 'F'\n", cmdName)
						return exitError{code: 2}
					}
					i++
					fsArg = args[i]
				}
				initAssigns = append(initAssigns, awkInitAssign{name: "FS", val: awkStrVal(awkUnescapeC(fsArg))})
				j = len(a)
			case 'v':
				var vArg string
				if j+1 < len(a) {
					vArg = a[j+1:]
				} else {
					if i+1 >= len(args) {
						fmt.Fprintf(hc.Stderr, "%s: option requires an argument -- 'v'\n", cmdName)
						return exitError{code: 2}
					}
					i++
					vArg = args[i]
				}
				asgn, err := parseAwkFlagAssign(vArg)
				if err != nil {
					fmt.Fprintf(hc.Stderr, "%s: %v\n", cmdName, err)
					return exitError{code: 2}
				}
				initAssigns = append(initAssigns, asgn)
				j = len(a)
			case 'f':
				var fArg string
				if j+1 < len(a) {
					fArg = a[j+1:]
				} else {
					if i+1 >= len(args) {
						fmt.Fprintf(hc.Stderr, "%s: option requires an argument -- 'f'\n", cmdName)
						return exitError{code: 2}
					}
					i++
					fArg = args[i]
				}
				src, err := loadAwkProgFile(hc, fArg)
				if err != nil {
					fmt.Fprintf(hc.Stderr, "%s: can't open file %s: %v\n", cmdName, fArg, err)
					return exitError{code: 2}
				}
				progSources = append(progSources, src)
				hasExplicitProg = true
				j = len(a)
			case 'e':
				var eArg string
				if j+1 < len(a) {
					eArg = a[j+1:]
				} else {
					if i+1 >= len(args) {
						fmt.Fprintf(hc.Stderr, "%s: option requires an argument -- 'e'\n", cmdName)
						return exitError{code: 2}
					}
					i++
					eArg = args[i]
				}
				progSources = append(progSources, eArg)
				hasExplicitProg = true
				j = len(a)
			case 'W':
				var wArg string
				if j+1 < len(a) {
					wArg = a[j+1:]
				} else {
					if i+1 >= len(args) {
						fmt.Fprintf(hc.Stderr, "%s: option requires an argument -- 'W'\n", cmdName)
						return exitError{code: 2}
					}
					i++
					wArg = args[i]
				}
				if wArg == "version" {
					fmt.Fprintf(hc.Stdout, "%s (unish coreutils) 1.0\n", cmdName)
					return nil
				}
				j = len(a)
			default:
				fmt.Fprintf(hc.Stderr, "%s: invalid option -- '%c'\n", cmdName, ch)
				return exitError{code: 2}
			}
		}
		i++
	}

	if !hasExplicitProg {
		if i >= len(args) {
			fmt.Fprintf(hc.Stderr, "Usage: %s [options] 'program' [file ...]\n", cmdName)
			return exitError{code: 1}
		}
		progSources = append(progSources, args[i])
		i++
	}

	fileArgs := args[i:]
	fullSrc := strings.Join(progSources, "\n")

	prog, err := parseAwkProgram(fullSrc)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", cmdName, err)
		return exitError{code: 1}
	}

	vm := newAwkVM(ctx, hc, prog, cmdName, fileArgs)
	for _, asgn := range initAssigns {
		vm.setVarByName(asgn.name, asgn.val)
		if vm.ctrl == ctrlError {
			fmt.Fprintf(hc.Stderr, "%s: %v\n", cmdName, vm.err)
			return exitError{code: 2}
		}
	}

	if err := vm.run(); err != nil {
		if ctx.Err() != nil || isPipeClosed(err) {
			return err
		}
		if ee, ok := err.(exitError); ok {
			return ee
		}
		fmt.Fprintf(hc.Stderr, "%s: fatal: %v\n", cmdName, err)
		return exitError{code: 2}
	}
	return nil
}

func parseAwkFlagAssign(spec string) (awkInitAssign, error) {
	eq := strings.IndexByte(spec, '=')
	if eq <= 0 || !isValidAwkIdent(spec[:eq]) {
		return awkInitAssign{}, fmt.Errorf("improperly formatted variable assignment: %q", spec)
	}
	name := spec[:eq]
	rawVal := awkUnescapeC(spec[eq+1:])
	return awkInitAssign{
		name: name,
		val:  awkStrNumVal(rawVal),
	}, nil
}

func loadAwkProgFile(hc interp.HandlerContext, path string) (string, error) {
	if path == "-" || path == "/dev/stdin" {
		if hc.Stdin == nil {
			return "", nil
		}
		data, err := io.ReadAll(hc.Stdin)
		return string(data), err
	}
	p := resolve(hc.Dir, path)
	data, err := readShellFile(p)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
