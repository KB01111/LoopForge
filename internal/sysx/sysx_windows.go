//go:build windows

// Package sysx isolates the few operating-system primitives (process groups,
// advisory file locks, file identity) that differ between Unix and Windows, so
// the rest of the platform builds unchanged on both.
package sysx

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// ErrLockBusy is returned by TryLockFile when another holder owns the lock.
var ErrLockBusy = errors.New("file lock is held by another process")

// SetProcessGroup starts cmd in a new process group and without a console window.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
}

// HasProcessGroup reports whether SetProcessGroup was applied to cmd.
func HasProcessGroup(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP != 0
}

// SignalGroup stops the process tree rooted at pid. Windows has no POSIX
// signals, so both modes terminate the whole tree with taskkill /T.
func SignalGroup(pid int, force bool) error {
	args := []string{"/T", "/PID", strconv.Itoa(pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	cmd := exec.Command("taskkill", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run()
}

// ProcessAlive reports whether a process with this pid is still running.
func ProcessAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means the process exists but belongs to someone else.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// TryLockFile takes an exclusive non-blocking lock on f.
func TryLockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrLockBusy
	}
	return err
}

// UnlockFile releases a lock taken with TryLockFile.
func UnlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// fileInfoByHandle opens path without following reparse semantics beyond the default and
// returns its BY_HANDLE_FILE_INFORMATION.
func fileInfoByHandle(path string) (windows.ByHandleFileInformation, bool) {
	var d windows.ByHandleFileInformation
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return d, false
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return d, false
	}
	defer windows.CloseHandle(h)
	if err := windows.GetFileInformationByHandle(h, &d); err != nil {
		return d, false
	}
	return d, true
}

// LinkCount returns the NTFS hard-link count of the file at path.
func LinkCount(path string, _ os.FileInfo) (uint64, bool) {
	d, ok := fileInfoByHandle(path)
	if !ok {
		return 0, false
	}
	return uint64(d.NumberOfLinks), true
}

// FileID returns (volume serial, file index) for the file at path.
func FileID(path string, _ os.FileInfo) (dev, ino uint64, ok bool) {
	d, ok := fileInfoByHandle(path)
	if !ok {
		return 0, 0, false
	}
	return uint64(d.VolumeSerialNumber), uint64(d.FileIndexHigh)<<32 | uint64(d.FileIndexLow), true
}

// FileOwner is not available through os.FileInfo on Windows.
func FileOwner(os.FileInfo) (uint32, bool) { return 0, false }

// SyncDir is a no-op on Windows: directories cannot be opened for flushing
// (FlushFileBuffers needs write access), and NTFS journals the rename itself.
func SyncDir(string) error { return nil }
