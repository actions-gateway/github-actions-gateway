// Command hog waits 5 seconds, then takes memory, 16 MiB at a time, until it holds as many MiB as
// its argument or a limit kills it. It is the workload of the oom-reporter's kind e2e lane; the wait
// lets the agent read the pod before its kill, which it would otherwise answer as Unknown.
//
//	hog <MiB>
//	hog <MiB> <name>...  first moves itself into a cgroup of 32 MiB at the path the names make,
//	                     below its own, as a job that names its cgroups to forge the kernel's
//	                     line would; it needs a privileged container
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const step = 16 << 20

// cgroupRoot is the container's own cgroup, in its cgroup namespace.
var cgroupRoot = "/sys/fs/cgroup"

func main() {
	mib, err := parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "hog:", err)
		os.Exit(2)
	}
	if names := os.Args[2:]; len(names) > 0 {
		if err := nest(names); err != nil {
			fmt.Fprintln(os.Stderr, "hog:", err)
			os.Exit(1)
		}
	}
	time.Sleep(5 * time.Second)
	var held [][]byte
	for len(held)*step < mib<<20 {
		b := make([]byte, step)
		// A page counts once it is written.
		for i := 0; i < len(b); i += os.Getpagesize() {
			b[i] = 1
		}
		held = append(held, b)
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("hog: holding %d MiB, not killed\n", len(held)*step>>20)
	for {
		time.Sleep(time.Hour)
		_ = held
	}
}

func parse(args []string) (mib int, err error) {
	if len(args) == 0 {
		return 0, errors.New("usage: hog <MiB> [<cgroup name>...]")
	}
	if mib, err = strconv.Atoi(args[0]); err != nil || mib <= 0 {
		return 0, errors.New("want a positive number of MiB")
	}
	return mib, nil
}

// nest moves the hog into a cgroup with a memory limit at the path names make. A cgroup v2 with
// processes in it can enable no controller for its children, so it leaves its own first.
func nest(names []string) error {
	write := func(file, data string) error { return os.WriteFile(file, []byte(data), 0o600) }
	pid := strconv.Itoa(os.Getpid())
	if err := os.Mkdir(filepath.Join(cgroupRoot, "hog"), 0o750); err != nil {
		return err
	}
	if err := write(filepath.Join(cgroupRoot, "hog", "cgroup.procs"), pid); err != nil {
		return err
	}
	dir := cgroupRoot
	for _, name := range names {
		if err := write(filepath.Join(dir, "cgroup.subtree_control"), "+memory"); err != nil {
			return err
		}
		dir = filepath.Join(dir, name)
		if err := os.Mkdir(dir, 0o750); err != nil {
			return err
		}
	}
	if err := write(filepath.Join(dir, "memory.max"), strconv.Itoa(32<<20)); err != nil {
		return err
	}
	// The kernel then prints the group it kills whole, as it does for a container.
	if err := write(filepath.Join(dir, "memory.oom.group"), "1"); err != nil {
		return err
	}
	return write(filepath.Join(dir, "cgroup.procs"), pid)
}
