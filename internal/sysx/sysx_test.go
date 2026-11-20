package sysx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockFileReportsBusyAcrossHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := TryLockFile(a); err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if err := TryLockFile(b); err != ErrLockBusy {
		t.Fatalf("second lock = %v, want ErrLockBusy", err)
	}
	UnlockFile(a)
	if err := TryLockFile(b); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	UnlockFile(b)
}

func TestProcessAliveSelf(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("own process should be alive")
	}
}
