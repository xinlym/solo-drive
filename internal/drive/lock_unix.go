//go:build unix

package drive

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("data directory is already in use: %w", err)
	}
	return f, nil
}
func releaseLock(f *os.File) error { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); return f.Close() }
