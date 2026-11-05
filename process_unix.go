//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func runManagedProcess(cmd *exec.Cmd) error {
	return runManagedProcessUntil(cmd, nil)
}

func runManagedProcessUntil(cmd *exec.Cmd, stop <-chan struct{}) error {
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		return err
	case <-stop:
		if err := killProcessGroup(cmd); err != nil {
			return err
		}
		<-wait
		return nil
	}
}
