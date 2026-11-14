package unish

// hash/type bypass for mvdan/sh.
//
// mvdan/sh's `hash` prints nothing for an empty table (bash prints
// "hash: hash table empty") and never records hits in this version;
// its `type`/`command -v` answers come from a different table than the
// shell's PATH. This file implements both against a real table fed by
// the external-execution path (runExternalTracked):
//
//   - `hash` with no args prints "hash: hash table empty" when empty,
//     else a "hits command" table like bash.
//   - `hash -r` clears; `hash name...` records explicit hits.
//   - `hash -d name...` (bash extension) forgets entries.
//   - `type name...` describes: shell builtin / unish builtin /
//     function / hashed path / PATH lookup / not found (exit 1).
//   - `command -v` is left to mvdan/sh (already correct).

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// hashEntry is one remembered command location.
type hashEntry struct {
	path string
	hits uint64
}

// hashTable is the process-wide remembered-command table.
type hashTable struct {
	mu      sync.Mutex
	entries map[string]*hashEntry
}

var globalHash = &hashTable{entries: map[string]*hashEntry{}}

// hashRecord notes that name was executed from path (called by the
// external-execution path after a successful start).
func hashRecord(name, path string) {
	if name == "" || path == "" {
		return
	}
	if strings.ContainsAny(name, "/\\") {
		return // explicit paths are not hashed
	}
	globalHash.mu.Lock()
	defer globalHash.mu.Unlock()
	e, ok := globalHash.entries[name]
	if !ok {
		e = &hashEntry{}
		globalHash.entries[name] = e
	}
	e.path = path
	e.hits++
}

// cmdHashTable implements `hash [-r] [-d] [--] [name...]`.
func cmdHashTable(_ context.Context, hc interp.HandlerContext, args []string) error {
	reset := false
	forget := false
	var names []string
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "-r":
			reset = true
		case "-d":
			forget = true
		case "-rd", "-dr":
			reset = true
			forget = true
		case "--":
			names = append(names, argv[i+1:]...)
			i = len(argv)
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				fmt.Fprintf(hc.Stderr, "hash: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
				return flag.ErrHelp
			}
			names = append(names, a)
		}
	}
	globalHash.mu.Lock()
	defer globalHash.mu.Unlock()
	if reset {
		globalHash.entries = map[string]*hashEntry{}
		if len(names) == 0 {
			return nil
		}
	}
	if forget {
		if len(names) == 0 {
			fmt.Fprintln(hc.Stderr, "hash: -d: option requires an argument")
			return flag.ErrHelp
		}
		for _, n := range names {
			delete(globalHash.entries, n)
		}
		return nil
	}
	if len(names) == 0 {
		if len(globalHash.entries) == 0 {
			fmt.Fprintln(hc.Stdout, "hash: hash table empty")
			return nil
		}
		fmt.Fprintln(hc.Stdout, "hits\tcommand")
		keys := make([]string, 0, len(globalHash.entries))
		for k := range globalHash.entries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			e := globalHash.entries[k]
			fmt.Fprintf(hc.Stdout, "   %d\t%s\n", e.hits, e.path)
		}
		return nil
	}
	// `hash name...`: record current locations. Like bash, shell
	// builtins hash trivially (rc 0, no table entry needed);
	// unish builtins are NOT hashable (bash: not found).
	for _, n := range names {
		if interp.IsBuiltin(n) {
			continue
		}
		if lookupExtra(n) != nil {
			fmt.Fprintf(hc.Stderr, "hash: %s: not found\n", n)
			return exitError{1}
		}
		if p, err := findExec(hc, n); err == nil {
			e, ok := globalHash.entries[n]
			if !ok {
				e = &hashEntry{}
				globalHash.entries[n] = e
			}
			e.path = p
		} else {
			fmt.Fprintf(hc.Stderr, "hash: %s: not found\n", n)
			return exitError{1}
		}
	}
	return nil
}

// cmdTypeDesc implements `type [-afptP] [--] name...` with GNU/bash-style output.
func cmdTypeDesc(_ context.Context, hc interp.HandlerContext, args []string) error {
	mode := "" // "", "-t", "-p", "-P"
	all := false
	noFuncs := false
	var names []string
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			names = append(names, argv[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 't':
					mode = "-t"
				case 'p':
					mode = "-p"
				case 'P':
					mode = "-P"
				case 'a':
					all = true
				case 'f':
					noFuncs = true
				default:
					fmt.Fprintf(hc.Stderr, "type: -%c: invalid option\n", a[j])
					return flag.ErrHelp
				}
			}
			continue
		}
		names = append(names, a)
	}
	if len(names) == 0 {
		return nil
	}
	code := 0
	for _, n := range names {
		if mode == "-P" {
			if p, err := findExec(hc, n); err == nil {
				fmt.Fprintln(hc.Stdout, p)
			} else {
				code = 1
			}
			continue
		}
		found := false
		if def, ok := hc.LookupAlias(n); ok {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "alias")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is aliased to `%s'\n", n, def)
			}
			if !all {
				continue
			}
		}
		if syntax.IsKeyword(n) {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "keyword")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is a shell keyword\n", n)
			}
			if !all {
				continue
			}
		}
		if !noFuncs && hc.HasFunc(n) {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "function")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is a function\n", n)
			}
			if !all {
				continue
			}
		}
		if isUnishCommand(n) {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "builtin")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is a unish builtin\n", n)
			}
			if !all {
				continue
			}
		} else if interp.IsBuiltin(n) {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "builtin")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is a shell builtin\n", n)
			}
			if !all {
				continue
			}
		}
		var filePath string
		isHashed := false
		if !all {
			globalHash.mu.Lock()
			if e := globalHash.entries[n]; e != nil && e.path != "" {
				filePath = e.path
				isHashed = true
			}
			globalHash.mu.Unlock()
		}
		if filePath == "" {
			if p, err := findExec(hc, n); err == nil {
				filePath = p
			}
		}
		if filePath != "" {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "file")
			case "-p":
				fmt.Fprintln(hc.Stdout, filePath)
			default:
				if isHashed {
					fmt.Fprintf(hc.Stdout, "%s is hashed (%s)\n", n, filePath)
				} else {
					fmt.Fprintf(hc.Stdout, "%s is %s\n", n, filePath)
				}
			}
		}
		if !found {
			if mode == "" {
				fmt.Fprintf(hc.Stderr, "type: %s: not found\n", n)
			}
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

// isUnishCommand reports whether name is one of our extra commands.
func isUnishCommand(name string) bool {
	if lookupExtra(name) != nil {
		return true
	}
	base := name
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	_ = filepath.Separator
	return lookupExtra(base) != nil && strings.ContainsAny(name, "/\\")
}
