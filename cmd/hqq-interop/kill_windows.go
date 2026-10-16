//go:build windows

package main

import (
	"os"
	"time"
)

// killSelf terminates this process (TerminateProcess) and does not return.
func killSelf() {
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Kill()
	}
	for {
		time.Sleep(time.Second)
	}
}
