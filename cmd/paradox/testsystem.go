package main

import (
	"errors"
	"io"
)

// runTestSystem serves the test system (backlog story 15).
func runTestSystem(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	return errors.New("testsystem is not built yet (backlog story 15)")
}
