package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
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
	stdout, _, err := runUnishScript(t, "pgrep -l .")
	if err != nil {
		t.Fatalf("pgrep -l . failed: %v", err)
	}
	if len(strings.TrimSpace(stdout)) == 0 {
		t.Errorf("expected non-empty output for pgrep -l .")
	}
}
