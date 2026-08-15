package main

import "testing"

func TestServiceCommandIgnoredWhenEmpty(t *testing.T) {
	handled, code := serviceCommand(options{})
	if handled || code != 0 {
		t.Fatalf("handled=%v code=%d", handled, code)
	}
}
