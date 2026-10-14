//go:build !unix

package client

import "os"

func inode(os.FileInfo) uint64 { return 0 }

// syncDir is a no-op where directories cannot be synced.
func syncDir(string) error { return nil }
