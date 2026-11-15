//go:build !windows

package pty

import (
	"os"
	"syscall"
)

type processGroupHandle struct {
	pgid int
}

func configureSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true,
	}
}

func attachProcessGroup(proc *os.Process) (*processGroupHandle, error) {
	if proc == nil {
		return nil, nil
	}
	pgid, err := syscall.Getpgid(proc.Pid)
	if err != nil || pgid <= 0 {
		pgid = proc.Pid
	}
	return &processGroupHandle{pgid: pgid}, nil
}

// killProcessTree sends SIGKILL to the entire process group (-pgid) as well as the leader PID.
func killProcessTree(proc *os.Process, pg *processGroupHandle) {
	if pg != nil && pg.pgid > 0 {
		_ = syscall.Kill(-pg.pgid, syscall.SIGKILL)
	}
	if proc != nil && proc.Pid > 0 {
		_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
		_ = proc.Kill()
	}
}
