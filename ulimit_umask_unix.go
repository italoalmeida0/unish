//go:build !windows

package main

import "syscall"

func applyUmask(m int) {
	syscall.Umask(m)
}

func ulimitNofile() (uint64, error) {
	var l syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &l); err != nil {
		return 0, err
	}
	return uint64(l.Cur), nil
}

func ulimitSetNofile(n uint64) error {
	var l syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &l); err != nil {
		return err
	}
	l.Cur = n
	return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &l)
}
