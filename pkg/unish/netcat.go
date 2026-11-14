package unish

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
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
	args = splitAttached("nc", args)
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
	waitExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "w" {
			waitExplicit = true
		}
	})
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
		host := ""
		port := *localPort
		if port != "" {
			if len(rest) > 0 {
				host = rest[0]
			}
		} else if len(rest) >= 2 {
			host, port = rest[0], rest[1]
		} else if len(rest) == 1 {
			port = rest[0]
		}
		if port == "" {
			fmt.Fprintln(hc.Stderr, "nc: missing port for listen mode")
			return flag.ErrHelp
		}
		addr := net.JoinHostPort(host, port)
		if *udp {
			return ncListenUDP(ctx, hc, network, addr, timeout, waitExplicit, *verbose)
		}
		ln, err := netListen(ctx, "nc", network, addr)
		if err != nil {
			if ctx.Err() != nil && !errors.Is(err, os.ErrPermission) {
				return ctx.Err()
			}
			if *verbose || errors.Is(err, os.ErrPermission) {
				fmt.Fprintf(hc.Stderr, "nc: listen on %s failed: %v\n", addr, err)
			}
			return exitError{1}
		}
		defer ln.Close()
		stopAccept := context.AfterFunc(ctx, func() { _ = ln.Close() })
		if *verbose {
			fmt.Fprintf(hc.Stderr, "Listening on %s...\n", addr)
		}
		var stopWait *time.Timer
		if waitExplicit {
			if dl, ok := ln.(interface{ SetDeadline(time.Time) error }); ok {
				_ = dl.SetDeadline(time.Now().Add(timeout))
			} else {
				stopWait = time.AfterFunc(timeout, func() { _ = ln.Close() })
			}
		}
		conn, err := ln.Accept()
		if stopWait != nil {
			stopWait.Stop()
		}
		stopAccept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return exitError{1}
		}
		_ = ln.Close()
		defer conn.Close()
		pipeConnIdle(ctx, hc, conn, timeout)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}

	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "nc: usage: nc [-zvul] [-w sec] host port")
		return flag.ErrHelp
	}
	host, port := rest[0], rest[1]
	target := net.JoinHostPort(host, port)

	conn, err := netDial(ctx, "nc", network, target, timeout)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, os.ErrPermission) {
			return ctx.Err()
		}
		if *verbose || errors.Is(err, os.ErrPermission) {
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

	idle := timeout
	if *udp && !waitExplicit {
		idle = 200 * time.Millisecond
	}
	pipeConnIdle(ctx, hc, conn, idle)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func ncListenUDP(ctx context.Context, hc interp.HandlerContext, network, addr string, timeout time.Duration, waitExplicit, verbose bool) error {
	pc, err := netListenPacket(ctx, "nc", network, addr)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, os.ErrPermission) {
			return ctx.Err()
		}
		if verbose || errors.Is(err, os.ErrPermission) {
			fmt.Fprintf(hc.Stderr, "nc: listen on %s failed: %v\n", addr, err)
		}
		return exitError{1}
	}
	defer pc.Close()
	stop := context.AfterFunc(ctx, func() { _ = pc.Close() })
	defer stop()
	if verbose {
		fmt.Fprintf(hc.Stderr, "Listening on %s...\n", addr)
	}
	if waitExplicit {
		_ = pc.SetReadDeadline(time.Now().Add(timeout))
	}
	buf := make([]byte, 65535)
	n, peerAddr, err := pc.ReadFrom(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return exitError{1}
	}
	_ = pc.SetReadDeadline(time.Time{})
	if n > 0 && hc.Stdout != nil {
		if _, werr := hc.Stdout.Write(buf[:n]); werr != nil {
			return nil
		}
	}
	idle := 200 * time.Millisecond
	if waitExplicit {
		idle = timeout
	}
	pipeConnIdle(ctx, hc, &udpAcceptedConn{PacketConn: pc, peer: peerAddr}, idle)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

type udpAcceptedConn struct {
	net.PacketConn
	peer net.Addr
}

func (c *udpAcceptedConn) Read(b []byte) (int, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(b)
		if err != nil {
			return 0, err
		}
		if addr != nil && addr.String() == c.peer.String() {
			return n, nil
		}
	}
}

func (c *udpAcceptedConn) Write(b []byte) (int, error) {
	return c.PacketConn.WriteTo(b, c.peer)
}

func (c *udpAcceptedConn) RemoteAddr() net.Addr {
	return c.peer
}

type closeWriter interface {
	CloseWrite() error
}

func pipeConn(ctx context.Context, hc interp.HandlerContext, conn net.Conn) {
	pipeConnIdle(ctx, hc, conn, time.Second)
}

func pipeConnIdle(ctx context.Context, hc interp.HandlerContext, conn net.Conn, idleTimeout time.Duration) {
	defer conn.Close()
	stopConn := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopConn()

	stdout := hc.Stdout
	if stdout == nil {
		stdout = io.Discard
	}

	var lastRecvNano atomic.Int64
	var lastSendNano atomic.Int64

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				lastRecvNano.Store(time.Now().UnixNano())
				if _, werr := stdout.Write(buf[:n]); werr != nil {
					_ = conn.Close()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	sent := make(chan struct{})
	cw, hasCloseWrite := conn.(closeWriter)
	go func() {
		defer close(sent)
		if hc.Stdin != nil {
			buf := make([]byte, 32*1024)
			for {
				n, rerr := hc.Stdin.Read(buf)
				if n > 0 {
					lastSendNano.Store(time.Now().UnixNano())
					if _, werr := conn.Write(buf[:n]); werr != nil {
						return
					}
					lastSendNano.Store(time.Now().UnixNano())
				}
				if rerr != nil {
					break
				}
			}
		}
		if hasCloseWrite {
			_ = cw.CloseWrite()
		}
	}()

	select {
	case <-ctx.Done():
		_ = conn.Close()
		<-recvDone
	case <-sent:
		// Stdin finished (and TCP FIN sent when supported). Wait for the peer
		// to finish sending its response until EOF, bounded by an inactivity
		// timeout so half-open or connectionless sockets cannot hang forever.
		drainIdle := idleTimeout
		if !hasCloseWrite && drainIdle > 250*time.Millisecond && idleTimeout == time.Second {
			drainIdle = 200 * time.Millisecond
		}
		if drainIdle <= 0 {
			drainIdle = time.Second
		}
		waitIdleOrDone(ctx, recvDone, &lastRecvNano, drainIdle)
		_ = conn.Close()
		<-recvDone
	case <-recvDone:
		// Remote closed its write side. Drain any active stdin writes,
		// but return promptly if stdin is idle/blocked (e.g. open pipe).
		waitIdleOrDone(ctx, sent, &lastSendNano, 150*time.Millisecond)
		_ = conn.Close()
	}
}

func waitIdleOrDone(ctx context.Context, done <-chan struct{}, lastAct *atomic.Int64, idle time.Duration) {
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-timer.C:
			last := lastAct.Load()
			if last == 0 {
				return
			}
			elapsed := time.Since(time.Unix(0, last))
			if elapsed >= idle {
				return
			}
			timer.Reset(idle - elapsed)
		}
	}
}
