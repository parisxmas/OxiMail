// Command oximailctl is OxiMail's administration CLI: it provisions
// domains, accounts, and aliases directly against the OxiDB store. It
// connects using the same OXIMAIL_* configuration as the server.
//
// Usage:
//
//	oximailctl domain  add <domain>
//	oximailctl domain  list
//	oximailctl domain  delete <domain>               refuses if any accounts remain
//	oximailctl domain  dkim [-selector S] [-force] <domain>  generate a signing key
//	oximailctl domain  dkim-show <domain>            print the existing key's TXT record
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
//	oximailctl sieve    get <address>
//	oximailctl sieve    set <address>           script on stdin
//	oximailctl sieve    clear <address>
//	oximailctl backup  <address> <path.tar>     mailboxes + messages + blobs
//	oximailctl restore <path.tar>               refuses if account exists
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

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
	case "sieve":
		return c.sieve(args[1:])
	case "backup":
		return c.backup(args[1:])
	case "restore":
		return c.restore(args[1:])
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

// readPassword prompts on stderr and reads one password from stdin.
// When stdin is a real terminal the input is masked via x/term so
// the password does not appear on screen. When stdin is a pipe or a
// file (e.g. tests, automation), the function falls back to a plain
// line read — masking is meaningless there and would just confuse a
// caller that piped a password in.
func (c *cmdContext) readPassword() (string, error) {
	fmt.Fprint(c.stderr, "Password: ")
	pw, err := readPasswordFrom(c.stdin)
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	if pw == "" {
		return "", fmt.Errorf("password must not be empty")
	}
	return pw, nil
}

// readPasswordFrom reads one password from r. If r is *os.File and
// refers to a terminal, term.ReadPassword is used (echo off + line
// terminated by Enter); otherwise a plain ReadString is used. The
// helper sits at package scope so tests can drive it with a
// non-terminal Reader without going through cmdContext.
func readPasswordFrom(r io.Reader) (string, error) {
	if f, ok := r.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		if err != nil {
			return "", err
		}
		// term.ReadPassword strips the newline; print one ourselves
		// so the next prompt appears on its own line.
		fmt.Fprintln(os.Stderr)
		return string(b), nil
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, `oximailctl — OxiMail administration

usage:
  oximailctl domain  add <domain>
  oximailctl domain  list
  oximailctl domain  delete <domain>
  oximailctl domain  dkim [-selector S] [-force] <domain>
  oximailctl domain  dkim-show <domain>             # read-only: print existing TXT record
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
  oximailctl sieve    get   <address>
  oximailctl sieve    set   <address>            # script on stdin
  oximailctl sieve    clear <address>
  oximailctl backup  <address> <path.tar>        # tar of metadata + blobs
  oximailctl restore <path.tar>                  # refuses if account exists

It connects to OxiDB with the same OXIMAIL_* environment variables as
the server — OXIMAIL_OXIDB_HOST, OXIMAIL_OXIDB_PORT.
`)
}
