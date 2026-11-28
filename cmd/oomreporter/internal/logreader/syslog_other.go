//go:build !linux

package logreader

import (
	"errors"
	"time"
)

var errNotLinux = errors.New("the kernel's log is read on Linux alone")

// ReadAll is the kernel's whole log, on Linux.
func ReadAll() (string, error) { return "", errNotLinux }

// Now is the node's clock and, on Linux, the time since its boot.
func Now() (time.Time, time.Duration) { return time.Now(), 0 }
