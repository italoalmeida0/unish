package main

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

// cmdHashTable implements `hash [-r] [name...]`.
func cmdHashTable(_ context.Context, hc interp.HandlerContext, args []string) error {
	reset := false
	forget := false
	var names []string
	for _, a := range args[1:] {
		switch a {
		case "-r":
			reset = true
		case "-d":
			forget = true
		case "--":
			names = append(names, args[1:]...)
		default:
			if strings.HasPrefix(a, "-") {
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
		return nil
	}
	if forget {
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

// cmdTypeDesc implements `type name...` with GNU/bash-style output.
func cmdTypeDesc(_ context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) < 2 {
		return flag.ErrHelp
	}
	code := 0
	for _, n := range args[1:] {
		switch {
		case isUnishCommand(n):
			fmt.Fprintf(hc.Stdout, "%s is a unish builtin\n", n)
		case interp.IsBuiltin(n):
			fmt.Fprintf(hc.Stdout, "%s is a shell builtin\n", n)
		default:
			if p, err := findExec(hc, n); err == nil {
				fmt.Fprintf(hc.Stdout, "%s is %s\n", n, p)
			} else {
				fmt.Fprintf(hc.Stderr, "type: %s: not found\n", n)
				code = 1
			}
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
