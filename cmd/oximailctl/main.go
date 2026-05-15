// Command oximailctl is OxiMail's administration CLI: it provisions
// domains, accounts, and aliases directly against the OxiDB store. It
// connects using the same OXIMAIL_* configuration as the server.
//
// Usage:
//
//	oximailctl domain  add <domain>
//	oximailctl domain  list
//	oximailctl domain  delete <domain>               refuses if any accounts remain
//	oximailctl domain  dkim [-selector S] <domain>   generate a signing key
//	oximailctl account add [-quota N] <address>      password read from stdin
//	oximailctl account list [-domain <domain>]
//	oximailctl account delete <address>
//	oximailctl account passwd <address>              new password read from stdin
//	oximailctl alias    add <address> <dest>[,<dest>...]
//	oximailctl alias    list
//	oximailctl alias    delete <address>
//	oximailctl vacation get <address>
//	oximailctl vacation set <address> -subject S -body B [-suppress-days N]
//	oximailctl vacation clear <address>
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/parisxmas/OxiMail/internal/config"
	"github.com/parisxmas/OxiMail/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// cmdContext carries the open store and the IO streams through the
// command handlers, so they stay testable without touching os.*.
type cmdContext struct {
	store  *store.Store
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// run is the testable entry point: it dispatches one command and
// returns a process exit code (0 ok, 1 error, 2 misuse).
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return 0
	}

	cfg := config.Load()
	st, err := store.Open(cfg.OxiDBHost, cfg.OxiDBPort)
	if err != nil {
		fmt.Fprintf(stderr, "oximailctl: %v\n", err)
		return 1
	}
	defer st.Close()
	if err := store.EnsureSchema(st); err != nil {
		fmt.Fprintf(stderr, "oximailctl: schema: %v\n", err)
		return 1
	}

	c := &cmdContext{store: st, stdin: stdin, stdout: stdout, stderr: stderr}
	switch args[0] {
	case "domain":
		return c.domain(args[1:])
	case "account":
		return c.account(args[1:])
	case "alias":
		return c.alias(args[1:])
	case "vacation":
		return c.vacation(args[1:])
	default:
		fmt.Fprintf(stderr, "oximailctl: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

// fail prints an error to stderr and returns exit code 1.
func (c *cmdContext) fail(format string, args ...any) int {
	fmt.Fprintf(c.stderr, "oximailctl: "+format+"\n", args...)
	return 1
}

// misuse prints a usage line to stderr and returns exit code 2.
func (c *cmdContext) misuse(line string) int {
	fmt.Fprintln(c.stderr, "usage: "+line)
	return 2
}

// readPassword prompts on stderr and reads a single line from stdin.
//
// TODO: turn off terminal echo (golang.org/x/term) for interactive use.
func (c *cmdContext) readPassword() (string, error) {
	fmt.Fprint(c.stderr, "Password: ")
	line, err := bufio.NewReader(c.stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading password: %w", err)
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", fmt.Errorf("password must not be empty")
	}
	return pw, nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, `oximailctl — OxiMail administration

usage:
  oximailctl domain  add <domain>
  oximailctl domain  list
  oximailctl domain  delete <domain>
  oximailctl domain  dkim [-selector S] <domain>
  oximailctl account add [-quota N] <address>      password read from stdin
  oximailctl account list [-domain <domain>]
  oximailctl account delete <address>
  oximailctl account passwd <address>              new password read from stdin
  oximailctl alias    add <address> <dest>[,<dest>...]
  oximailctl alias    list
  oximailctl alias    delete <address>
  oximailctl vacation get   <address>
  oximailctl vacation set   <address> -subject S -body B [-suppress-days N]
  oximailctl vacation clear <address>

It connects to OxiDB with the same OXIMAIL_* environment variables as
the server — OXIMAIL_OXIDB_HOST, OXIMAIL_OXIDB_PORT.
`)
}
