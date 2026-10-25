//go:build !unix && !windows

package main

import (
	"errors"
	"syscall"
)

func detachedAttr() *syscall.SysProcAttr { return nil }

func processAlive(int) bool { return false }

func stopProcess(int) error { return errors.New("not supported on this system") }
