package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Independent from operation/daemon locks: stop never waits for this lock.
// Never unlink it; every process must flock the same stable inode.
func acquireSourceProcessLock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(filepath.Join(appSupportDir(), "source-generation.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "source-generation.lock")
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, fmt.Errorf("unsafe source generation lock")
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(fd, syscall.LOCK_UN); file.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func withSourceProcessLock(ctx context.Context, body func() error) error {
	release, err := acquireSourceProcessLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return body()
}

func runGeneratorExclusive() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return withSourceProcessLock(ctx, func() error { return runOffice91GeneratorContext(ctx) })
}
