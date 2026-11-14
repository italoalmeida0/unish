//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func killProc(proc *os.Process, sig string) error {
	s, ok := map[string]os.Signal{
		"HUP":    syscall.SIGHUP,
		"INT":    syscall.SIGINT,
		"QUIT":   syscall.SIGQUIT,
		"ILL":    syscall.SIGILL,
		"TRAP":   syscall.SIGTRAP,
		"ABRT":   syscall.SIGABRT,
		"IOT":    syscall.SIGABRT,
		"BUS":    syscall.SIGBUS,
		"FPE":    syscall.SIGFPE,
		"KILL":   syscall.SIGKILL,
		"USR1":   syscall.SIGUSR1,
		"SEGV":   syscall.SIGSEGV,
		"USR2":   syscall.SIGUSR2,
		"PIPE":   syscall.SIGPIPE,
		"ALRM":   syscall.SIGALRM,
		"TERM":   syscall.SIGTERM,
		"CHLD":   syscall.SIGCHLD,
		"CLD":    syscall.SIGCHLD,
		"CONT":   syscall.SIGCONT,
		"STOP":   syscall.SIGSTOP,
		"TSTP":   syscall.SIGTSTP,
		"TTIN":   syscall.SIGTTIN,
		"TTOU":   syscall.SIGTTOU,
		"URG":    syscall.SIGURG,
		"XCPU":   syscall.SIGXCPU,
		"XFSZ":   syscall.SIGXFSZ,
		"VTALRM": syscall.SIGVTALRM,
		"PROF":   syscall.SIGPROF,
		"WINCH":  syscall.SIGWINCH,
		"IO":     syscall.SIGIO,
		"SYS":    syscall.SIGSYS,
	}[sig]
	if !ok {
		n := 0
		for _, c := range sig {
			if c < '0' || c > '9' {
				return syscall.EINVAL
			}
			n = n*10 + int(c-'0')
		}
		s = syscall.Signal(n)
	}
	return proc.Signal(s)
}

func signalExitCode(ee *exec.ExitError) int {
	if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		return 128 + int(st.Signal())
	}
	return -1
}
