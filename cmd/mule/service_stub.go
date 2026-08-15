//go:build !windows

package main

import (
	"fmt"
	"os"
	"strings"
)

func runningAsWindowsService() bool { return false }

func runWindowsService(options) int { return 1 }

func serviceCommand(opt options) (handled bool, code int) {
	if strings.TrimSpace(opt.service) == "" {
		return false, 0
	}
	fmt.Fprintln(os.Stderr, "-service is only supported on Windows")
	return true, 1
}
