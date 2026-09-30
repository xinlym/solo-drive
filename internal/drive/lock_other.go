//go:build !unix

package drive

import (
	"errors"
	"os"
)

func acquireLock(path string) (*os.File, error) {
	return nil, errors.New("use Linux or run SoloDrive in a Linux container")
}
func releaseLock(f *os.File) error { return f.Close() }
