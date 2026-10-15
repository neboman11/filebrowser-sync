//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

func interrupt(p *os.Process) error { return p.Signal(os.Interrupt) }

// lockInstance takes an exclusive, non-blocking lock on path for the life of
// the process.
func lockInstance(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errAlreadyRunning
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// openPath opens a file, directory or URL with the desktop's default handler.
func openPath(target string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
