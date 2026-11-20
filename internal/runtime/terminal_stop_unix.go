//go:build !windows

package runtime

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// killProcessGroups hangs up and then kills the terminal's process groups.
func (s *workspaceTerminal) killProcessGroups() {
	fg, _ := unix.IoctlGetInt(int(s.file.Fd()), unix.TIOCGPGRP)
	// Closing the controlling PTY also hangs up the foreground process group.
	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGHUP)
	s.file.Close()
	time.Sleep(100 * time.Millisecond)
	if fg > 0 {
		syscall.Kill(-fg, syscall.SIGKILL)
	}
	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
}
