//go:build !windows

package main

import "testing"

func TestServiceCommandRejectedOffWindows(t *testing.T) {
	handled, code := serviceCommand(options{service: "install"})
	if !handled || code != 1 {
		t.Fatalf("handled=%v code=%d", handled, code)
	}
}
