package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func runUnishScript(t *testing.T, src string) (string, string, error) {
	t.Helper()
	prog, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	r, err := interp.New(
		interp.StdIO(nil, &stdout, &stderr),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(extraHandler),
	)
	if err != nil {
		t.Fatalf("interpreter creation error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = r.Run(ctx, prog)
	return stdout.String(), stderr.String(), err
}

func TestSsCommand(t *testing.T) {
	// Test ss -tulnp
	stdout, stderr, err := runUnishScript(t, "ss -tulnp")
	if err != nil {
		t.Fatalf("ss -tulnp failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Local Address:Port") {
		t.Errorf("expected Local Address:Port header, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Process") {
		t.Errorf("expected Process header with -p flag, got:\n%s", stdout)
	}

	// Test ss -tln
	stdout, stderr, err = runUnishScript(t, "ss -tln")
	if err != nil {
		t.Fatalf("ss -tln failed: %v\nstderr: %s", err, stderr)
	}
	if strings.Contains(stdout, "Process") {
		t.Errorf("did not expect Process header without -p flag, got:\n%s", stdout)
	}
}

func TestNetcatScanning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// Test open port
	cmd := fmt.Sprintf("nc -z -v 127.0.0.1 %d", port)
	stdout, stderr, err := runUnishScript(t, cmd)
	if err != nil {
		t.Fatalf("nc -z -v open port failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stderr, "succeeded") && !strings.Contains(stdout, "succeeded") {
		t.Errorf("expected succeeded in output, got stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestPgrep(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("pgrep parity targets procps; no procfs on macOS")
	}
	stdout, _, err := runUnishScript(t, "pgrep -l .")
	if err != nil {
		t.Fatalf("pgrep -l . failed: %v", err)
	}
	if len(strings.TrimSpace(stdout)) == 0 {
		t.Errorf("expected non-empty output for pgrep -l .")
	}
}

func TestFindExpressions(t *testing.T) {
	// Test find with -name and -not -path
	stdout, stderr, err := runUnishScript(t, `find . -maxdepth 1 -name "main.go" -not -path "/.git/"`)
	if err != nil {
		t.Fatalf("find failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "main.go") {
		t.Errorf("expected find to locate main.go, got:\n%s", stdout)
	}

	// Test find -not matching
	stdout, _, err = runUnishScript(t, `find . -maxdepth 1 -name "main.go" -not -name "main.go"`)
	if err != nil {
		t.Fatalf("find failed: %v", err)
	}
	if strings.Contains(stdout, "main.go") {
		t.Errorf("expected empty output with negation, got:\n%s", stdout)
	}
}

func TestGrepNonHanging(t *testing.T) {
	// Grep with recursive and an include filter that matches nothing should exit without hanging
	_, _, err := runUnishScript(t, `grep -rl "system-prompt" --include=".cs" --include=".json" .`)
	if err == nil {
		t.Errorf("expected grep to return exit code 1 when no files match, got nil error")
	}
}

// TestListProcsParentLinkage locks the ppid column of the process table on
// EVERY platform. The macOS reader previously parsed e_ppid at a hand-rolled
// offset that was actually the start of eproc, so every row reported ppid 0
// — invisible to ps(1)'s default columns and to pgrep/pkill, which is why
// the existing suite (which skips pgrep on darwin) never caught it.
func TestListProcsParentLinkage(t *testing.T) {
	procs, err := listProcs()
	if err != nil {
		t.Fatalf("listProcs: %v", err)
	}
	self := os.Getpid()
	parent := os.Getppid()
	var found bool
	for _, p := range procs {
		if p.pid == self {
			found = true
			if p.ppid != parent {
				t.Errorf("self ppid = %d, want %d (the process table must link parentage)", p.ppid, parent)
			}
		}
	}
	if !found {
		t.Fatalf("process table does not contain the calling process (pid %d)", self)
	}
	// The parent (the test runner) must be present and have a live pid.
	for _, p := range procs {
		if p.pid == parent {
			if p.ppid < 0 {
				t.Errorf("parent row has negative ppid: %+v", p)
			}
			return
		}
	}
	t.Errorf("process table does not contain the parent (pid %d)", parent)
}
