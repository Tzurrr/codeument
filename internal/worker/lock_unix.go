//go:build !windows

package worker

import (
	"os"
	"syscall"
)

// Lock is a single-instance file lock.
type Lock struct{ f *os.File }

// TryLock acquires the lock or returns ok=false when another worker holds it.
func TryLock(path string) (*Lock, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &Lock{f: f}, true, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
