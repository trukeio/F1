//go:build !unix

package repodir

import (
	"errors"
	"os"
)

func lockFile(*os.File) error {
	return errors.New("repodir: repository locking is only supported on Unix systems")
}

func unlockFile(*os.File) error { return nil }
