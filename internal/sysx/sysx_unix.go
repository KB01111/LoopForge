//go:build !windows

// Package sysx isolates the few operating-system primitives (process groups,
// advisory file locks, file identity) that differ between Unix and Windows, so
// the rest of the platform builds unchanged on both.
package sysx

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ErrLockBusy is returned by TryLockFile when another holder owns the lock.
var ErrLockBusy = errors.New("file lock is held by another process")

// SetProcessGroup runs cmd in its own process group so the whole tree can be signalled.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// HasProcessGroup reports whether SetProcessGroup was applied to cmd.
func HasProcessGroup(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid
}

// SignalGroup asks the process group led by pid to stop (force: kill it).
func SignalGroup(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pid, sig)
}

// ProcessAlive reports whether a process with this pid exists.
func ProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// TryLockFile takes an exclusive non-blocking advisory lock on f.
func TryLockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrLockBusy
	}
	return err
}

// UnlockFile releases a lock taken with TryLockFile.
func UnlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// LinkCount returns the hard-link count of the file behind info, when known.
func LinkCount(info os.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}

// FileID returns a (device, inode) identity for info, when the platform exposes one.
func FileID(info os.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

// FileOwner returns the owning uid of the file behind info, when known.
func FileOwner(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
