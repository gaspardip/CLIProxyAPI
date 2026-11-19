package config

import (
	"context"
	"os"

	"golang.org/x/sync/semaphore"
)

var fileAccess = semaphore.NewWeighted(1)

// WithFileAccess serializes supported in-process config I/O, including load-time
// rewrites and failed-write restoration. One gate also covers symlinks and mounted
// aliases of the same inode. Independent processes and direct os writes are outside
// this contract. Callers must release it before validation or runtime callbacks;
// the operation must not call another function that acquires this gate.
func WithFileAccess(operation func() error) error {
	if err := fileAccess.Acquire(context.Background(), 1); err != nil {
		return err
	}
	defer fileAccess.Release(1)
	return operation()
}

// ReadConfigFile reads committed bytes without running application callbacks.
func ReadConfigFile(path string) (data []byte, err error) {
	err = WithFileAccess(func() error {
		data, err = os.ReadFile(path)
		return err
	})
	return data, err
}
