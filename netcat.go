package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	extraCommands = append(extraCommands,
		extraCmd{"nc", cmdNc},
		extraCmd{"netcat", cmdNc},
	)
}

func cmdNc(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("nc", hc.Stderr)
	zeroIO := fs.Bool("z", false, "")
	verbose := fs.Bool("v", false, "")
	udp := fs.Bool("u", false, "")
	listen := fs.Bool("l", false, "")
	localPort := fs.String("p", "", "")
	waitSec := fs.Int("w", 5, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	timeout := time.Duration(*waitSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	network := "tcp"
	if *udp {
		network = "udp"
	}

	rest := fs.Args()
	if *listen {
		port := *localPort
		if port == "" && len(rest) > 0 {
			port = rest[0]
		}
		if port == "" {
			fmt.Fprintln(hc.Stderr, "nc: missing port for listen mode")
			return flag.ErrHelp
		}
		addr := ":" + port
		ln, err := net.Listen(network, addr)
		if err != nil {
			if *verbose {
				fmt.Fprintf(hc.Stderr, "nc: listen on %s failed: %v\n", addr, err)
			}
			return exitError{1}
		}
		defer ln.Close()
		if *verbose {
			fmt.Fprintf(hc.Stderr, "Listening on %s...\n", addr)
		}
		conn, err := ln.Accept()
		if err != nil {
			return exitError{1}
		}
		defer conn.Close()
		pipeConn(ctx, hc, conn)
		return nil
	}

	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "nc: usage: nc [-zvul] [-w sec] host port")
		return flag.ErrHelp
	}
	host, port := rest[0], rest[1]
	target := net.JoinHostPort(host, port)

	conn, err := net.DialTimeout(network, target, timeout)
	if err != nil {
		if *verbose {
			fmt.Fprintf(hc.Stderr, "nc: connect to %s port %s (%s) failed: %v\n", host, port, network, err)
		}
		return exitError{1}
	}
	defer conn.Close()

	if *zeroIO {
		if *verbose {
			fmt.Fprintf(hc.Stderr, "Connection to %s %s port [%s/*] succeeded!\n", host, port, network)
		}
		return nil
	}

	pipeConn(ctx, hc, conn)
	return nil
}

func pipeConn(ctx context.Context, hc interp.HandlerContext, conn net.Conn) {
	// Wait for BOTH directions, not just the first. Returning when either
	// finished closed the connection before the stdin copier had sent the
	// data (the peer often stops reading first, so `printf x | nc host port`
	// delivered nothing).
	defer conn.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, hc.Stdin)
		// Half-close so the peer sees EOF and can finish.
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(hc.Stdout, conn)
		done <- struct{}{}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return
		case <-done:
		}
	}
}
