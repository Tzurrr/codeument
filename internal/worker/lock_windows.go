//go:build windows

package worker

import "os"

// Lock is a single-instance file lock (best effort on Windows: exclusive
// create of a lock file).
type Lock struct{ path string }

// TryLock acquires the lock or returns ok=false when another worker holds it.
func TryLock(path string) (*Lock, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	_ = f.Close()
	return &Lock{path: path}, true, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() {
	if l != nil {
		_ = os.Remove(l.path)
	}
}
