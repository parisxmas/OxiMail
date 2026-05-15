package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/parisxmas/OxiMail/internal/sieve"
	"github.com/parisxmas/OxiMail/internal/store"
)

func (c *cmdContext) sieve(args []string) int {
	if len(args) == 0 {
		return c.misuse("oximailctl sieve <get|set|clear> ...")
	}
	switch args[0] {
	case "get":
		return c.sieveGet(args[1:])
	case "set":
		return c.sieveSet(args[1:])
	case "clear":
		return c.sieveClear(args[1:])
	default:
		return c.misuse("oximailctl sieve <get|set|clear> ...")
	}
}

func (c *cmdContext) sieveGet(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl sieve get <address>")
	}
	acc, err := c.store.GetAccount(args[0])
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such account %q", args[0])
	}
	if err != nil {
		return c.fail("%v", err)
	}
	sc, err := c.store.GetSieveScript(acc.ID)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(c.stdout, "no sieve script set for %s\n", acc.Address)
		return 0
	}
	if err != nil {
		return c.fail("%v", err)
	}
	fmt.Fprint(c.stdout, sc.Source)
	if sc.Source != "" && sc.Source[len(sc.Source)-1] != '\n' {
		fmt.Fprintln(c.stdout)
	}
	return 0
}

// sieveSet reads the script from stdin and stores it. The script is
// validated by parsing it; a syntax error is reported and the previous
// script (if any) stays in place.
func (c *cmdContext) sieveSet(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl sieve set <address>  # script on stdin")
	}
	acc, err := c.store.GetAccount(args[0])
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such account %q", args[0])
	}
	if err != nil {
		return c.fail("%v", err)
	}
	src, err := io.ReadAll(c.stdin)
	if err != nil {
		return c.fail("read script: %v", err)
	}
	if _, err := sieve.Parse(string(src)); err != nil {
		return c.fail("parse sieve: %v", err)
	}
	if _, err := c.store.SetSieveScript(acc.ID, string(src)); err != nil {
		return c.fail("save sieve: %v", err)
	}
	fmt.Fprintf(c.stdout, "sieve script saved for %s (%d bytes)\n", acc.Address, len(src))
	return 0
}

func (c *cmdContext) sieveClear(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl sieve clear <address>")
	}
	acc, err := c.store.GetAccount(args[0])
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such account %q", args[0])
	}
	if err != nil {
		return c.fail("%v", err)
	}
	if err := c.store.DeleteSieveScript(acc.ID); err != nil {
		return c.fail("clear sieve: %v", err)
	}
	fmt.Fprintf(c.stdout, "sieve script cleared for %s\n", acc.Address)
	return 0
}
