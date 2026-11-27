// Command hog waits 5 seconds, then takes memory, 16 MiB at a time, until it holds as many MiB as
// its argument or a limit kills it. It is the workload of the oom-reporter's kind e2e lane; the wait
// lets the agent read the pod before its kill, which it would otherwise answer as Unknown.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const step = 16 << 20

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hog <MiB>")
		os.Exit(2)
	}
	mib, err := strconv.Atoi(os.Args[1])
	if err != nil || mib <= 0 {
		fmt.Fprintln(os.Stderr, "hog: want a positive number of MiB")
		os.Exit(2)
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
