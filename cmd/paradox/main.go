// Command paradox is the PARADOX scheduler and job queue.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Rumeasiyan/paradox"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process exit, so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("paradox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, paradox.Version)
		return 0
	}
	fs.Usage()
	return 2
}
