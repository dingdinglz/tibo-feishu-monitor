//go:build linux || darwin

// Package lock provides crash-safe single-process ownership on supported hosts.
package lock

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type Lock struct{ file *os.File }

func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, errors.New("cannot create lock directory")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("cannot open process lock")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another monitor/probe owns this database lock")
	}
	return &Lock{file: f}, nil
}

func (l *Lock) Close() error {
	// Never unlink: replacing an inode would let a second process bypass flock.
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}
