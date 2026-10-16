//go:build !windows

package main

import (
	"os"
	"syscall"
	"time"
)

// killSelf delivers SIGKILL to this process and does not return.
func killSelf() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	for {
		time.Sleep(time.Second)
	}
}
