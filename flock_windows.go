//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/windows"
	"mvdan.cc/sh/v3/interp"
)

const (
	winFlockOffsetLow  = 0xffffffff
	winFlockOffsetHigh = 0x7ffffffe
	winErrNotLocked    = windows.Errno(158) // ERROR_NOT_LOCKED
)

func openFlockFile(path string) (*os.File, error) {
	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	h, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		share,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		// Retry read-only (handles read-only files or directories with backup semantics).
		h2, err2 := windows.CreateFile(
			pathp,
			windows.GENERIC_READ,
			share,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS,
			0,
		)
		if err2 != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		h = h2
	}
	return os.NewFile(uintptr(h), path), nil
}

func tryFlockFile(f *os.File, shared bool) (bool, error) {
	ol := windows.Overlapped{
		Offset:     winFlockOffsetLow,
		OffsetHigh: winFlockOffsetHigh,
	}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ol)
	runtime.KeepAlive(f)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, err
}

func unlockFlockFile(f *os.File) error {
	ol := windows.Overlapped{
		Offset:     winFlockOffsetLow,
		OffsetHigh: winFlockOffsetHigh,
	}
	err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
	runtime.KeepAlive(f)
	if errors.Is(err, winErrNotLocked) {
		return nil
	}
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
	return nil, nil, fmt.Errorf("bad file descriptor: %d", fd)
}
