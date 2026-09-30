//go:build !linux

package drive

import "errors"

func diskCapacity(path string) (total, used, available int64, err error) {
	return 0, 0, 0, errors.New("filesystem capacity checking currently requires Linux")
}
