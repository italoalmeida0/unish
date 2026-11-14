//go:build !windows

package unish

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
	"mvdan.cc/sh/v3/interp"
)

func openFlockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		// Fall back to read-only open (e.g. read-only file or directory).
		if rf, rerr := os.Open(path); rerr == nil {
			return rf, nil
		}
		return nil, err
	}
	return f, nil
}

func tryFlockFile(f *os.File, shared bool) (bool, error) {
	op := unix.LOCK_EX | unix.LOCK_NB
	if shared {
		op = unix.LOCK_SH | unix.LOCK_NB
	}
	err := unix.Flock(int(f.Fd()), op)
	runtime.KeepAlive(f)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return false, err
}

func unlockFlockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	runtime.KeepAlive(f)
	return err
}

func resolveFlockFD(fd int, hc interp.HandlerContext) (func(shared bool) (bool, error), func() error, error) {
	if f := stdioOSFile(fd, hc); f != nil {
		return func(shared bool) (bool, error) {
				return tryFlockFile(f, shared)
			}, func() error {
				return unlockFlockFile(f)
			}, nil
	}
	if fd < 0 {
		return nil, nil, fmt.Errorf("bad file descriptor: %d", fd)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		return nil, nil, fmt.Errorf("bad file descriptor: %d", fd)
	}
	return func(shared bool) (bool, error) {
			op := unix.LOCK_EX | unix.LOCK_NB
			if shared {
				op = unix.LOCK_SH | unix.LOCK_NB
			}
			err := unix.Flock(fd, op)
			if err == nil {
				return true, nil
			}
			if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				return false, nil
			}
			return false, err
		}, func() error {
			return unix.Flock(fd, unix.LOCK_UN)
		}, nil
}
