package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSourceProcessLockChild(t *testing.T) {
	if os.Getenv("SRHOP_LOCK_TEST_CHILD") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	release, err := acquireSourceProcessLock(ctx)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected exclusion and cancellation, got %v", err)
	}
}

func TestSourceProcessLockExcludesOtherProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	release, err := acquireSourceProcessLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	child := exec.Command(os.Args[0], "-test.run=^TestSourceProcessLockChild$")
	child.Env = append(os.Environ(), "SRHOP_LOCK_TEST_CHILD=1")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, out)
	}
}

func TestSourceProcessLockRejectsSymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.MkdirAll(appSupportDir(), 0700)
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte("keep"), 0600)
	if err := os.Symlink(target, filepath.Join(appSupportDir(), "source-generation.lock")); err != nil {
		t.Fatal(err)
	}
	if release, err := acquireSourceProcessLock(context.Background()); err == nil {
		release()
		t.Fatal("accepted symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "keep" {
		t.Fatal("modified target")
	}
}

func TestSourceLockDescriptorNotInherited(t *testing.T) {
	if os.Getenv("SRHOP_LOCK_FD_CHILD") == "1" {
		expected, err := os.Stat(filepath.Join(appSupportDir(), "source-generation.lock"))
		if err != nil {
			t.Fatal(err)
		}
		for fd := 3; fd < 256; fd++ {
			var st syscall.Stat_t
			if syscall.Fstat(fd, &st) == nil {
				if original, ok := expected.Sys().(*syscall.Stat_t); ok && st.Dev == original.Dev && st.Ino == original.Ino {
					t.Fatal("lock descriptor inherited")
				}
			}
		}
		return
	}
	t.Setenv("HOME", t.TempDir())
	release, err := acquireSourceProcessLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	child := exec.Command(os.Args[0], "-test.run=^TestSourceLockDescriptorNotInherited$")
	child.Env = append(os.Environ(), "SRHOP_LOCK_FD_CHILD=1")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child %v: %s", err, out)
	}
}
