//go:build windows

package pty

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processGroupHandle struct {
	jobHandle windows.Handle
	pid       int
}

func configureSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_UNICODE_ENVIRONMENT,
	}
}

// attachProcessGroup creates a Windows Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and assigns the spawned process to it so all
// descendant child processes are automatically killed when the session closes.
func attachProcessGroup(proc *os.Process) (*processGroupHandle, error) {
	if proc == nil {
		return nil, nil
	}
	pg := &processGroupHandle{pid: proc.Pid}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return pg, nil
	}

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return pg, nil
	}

	pHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(proc.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return pg, nil
	}
	defer windows.CloseHandle(pHandle)

	if err := windows.AssignProcessToJobObject(job, pHandle); err != nil {
		// Note: If tether itself is already inside a non-nested Job Object on older Windows,
		// AssignProcessToJobObject may fail; taskkill /T /F fallback will still kill the tree.
		_ = windows.CloseHandle(job)
		return pg, nil
	}

	pg.jobHandle = job
	return pg, nil
}

// killProcessTree terminates the Windows Job Object (killing all child processes)
// and also runs taskkill /T /F as a belt-and-suspenders fallback.
func killProcessTree(proc *os.Process, pg *processGroupHandle) {
	if pg != nil && pg.jobHandle != 0 {
		_ = windows.TerminateJobObject(pg.jobHandle, 1)
		_ = windows.CloseHandle(pg.jobHandle)
		pg.jobHandle = 0
	}
	if proc != nil && proc.Pid > 0 {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
		_ = proc.Kill()
	}
}
