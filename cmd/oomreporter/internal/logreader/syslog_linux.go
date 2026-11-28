//go:build linux

package logreader

import (
	"time"

	"golang.org/x/sys/unix"
)

// ReadAll is the kernel's whole log, by syslog(2), which needs CAP_SYSLOG where dmesg_restrict
// is 1.
func ReadAll() (string, error) {
	size, err := unix.Klogctl(unix.SYSLOG_ACTION_SIZE_BUFFER, nil)
	if err != nil {
		return "", err
	}
	buf := make([]byte, size)
	n, err := unix.Klogctl(unix.SYSLOG_ACTION_READ_ALL, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// Now is the node's clock and CLOCK_MONOTONIC, which the log's stamps are taken against.
func Now() (time.Time, time.Duration) {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return time.Now(), time.Duration(ts.Nano())
}
