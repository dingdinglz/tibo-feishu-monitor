//go:build linux || darwin

package lock

import (
	"path/filepath"
	"testing"
)

func TestExclusiveAndRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "monitor.lock")
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Acquire(p); err == nil {
		second.Close()
		t.Fatal("allowed duplicate owner")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}
