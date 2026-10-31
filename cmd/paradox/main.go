// Command paradox is the PARADOX scheduler and job queue.
package main

import (
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Rumeasiyan/paradox"
	"github.com/Rumeasiyan/paradox/sign"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

const usage = `usage:
  paradox -version            print the version
  paradox keygen -out FILE    create a key pair: private half to FILE, public half printed
  paradox pubkey [-in FILE]   print the public half of a private key (default: PARADOX_PRIVATE_KEY)
  paradox -healthcheck        exit 0 if the running service and its database are healthy
  paradox testsystem          run the test system that answers /jobs/<name>
`

// run is main without the process exit, so tests can drive it.
func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) > 0 {
		switch args[0] {
		case "keygen":
			return report(stderr, keygen(args[1:], stdout, stderr))
		case "pubkey":
			return report(stderr, pubkey(args[1:], stdout, stderr, getenv))
		case "testsystem":
			return report(stderr, runTestSystem(args[1:], stdout, stderr, getenv))
		}
	}
	fs := flag.NewFlagSet("paradox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	showVersion := fs.Bool("version", false, "print the version and exit")
	healthcheck := fs.Bool("healthcheck", false, "exit 0 if the running service and its database are healthy")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, paradox.Version)
		return 0
	}
	if *healthcheck {
		return report(stderr, runHealthcheck(getenv))
	}
	fs.Usage()
	return 2
}

var errUsage = errors.New("usage")

func report(stderr io.Writer, err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprint(stderr, usage)
		return 2
	default:
		fmt.Fprintln(stderr, "paradox:", err)
		return 1
	}
}

// keygen writes a new private key to a file only its owner can read and
// prints the public half (PX-CON-04). The private key never goes to the
// terminal, where it would end up in scrollback and logs.
func keygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "file to write the private key to; must not exist")
	if err := fs.Parse(args); err != nil || *out == "" || fs.NArg() > 0 {
		return errUsage
	}
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite a key", *out)
		}
		return err
	}
	if _, err := fmt.Fprintln(f, priv); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintln(stdout, pub)
	fmt.Fprintf(stderr, "private key written to %s (readable by you only); keep it secret\n", *out)
	return nil
}

// pubkey prints the public half of a private key, so PARADOX's operator can
// hand systems the key they verify PARADOX with.
func pubkey(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("pubkey", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "file holding the private key (default: PARADOX_PRIVATE_KEY)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errUsage
	}
	text := getenv("PARADOX_PRIVATE_KEY")
	source := "PARADOX_PRIVATE_KEY"
	if *in != "" {
		b, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		text, source = string(b), *in
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("no private key: %s is empty", source)
	}
	priv, err := sign.ParsePrivateKey(text)
	if err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
	fmt.Fprintln(stdout, sign.FormatPublicKey(priv.Public().(ed25519.PublicKey)))
	return nil
}
