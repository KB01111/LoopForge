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

func TestLinkCountAndFileID(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(a)
	if n, ok := LinkCount(a, info); !ok || n != 1 {
		t.Fatalf("LinkCount = %d,%v want 1,true", n, ok)
	}
	b := filepath.Join(dir, "b")
	if err := os.Link(a, b); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	info, _ = os.Stat(a)
	if n, ok := LinkCount(a, info); !ok || n != 2 {
		t.Fatalf("LinkCount after link = %d,%v want 2,true", n, ok)
	}
	d1, i1, ok1 := FileID(a, info)
	infoB, _ := os.Stat(b)
	d2, i2, ok2 := FileID(b, infoB)
	if !ok1 || !ok2 || d1 != d2 || i1 != i2 {
		t.Fatalf("hard links should share an identity: %v/%v %v/%v", d1, i1, d2, i2)
	}
}
