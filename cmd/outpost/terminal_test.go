package main

import (
	"os"
	"testing"
)

// /dev/null is a character device, which the old check took for a terminal,
// so `install </dev/null` asked a question nobody could answer.
func TestDevNullIsNotATerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Errorf("%s reported as a terminal", os.DevNull)
	}
}
