package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/parisxmas/OxiMail/internal/store"
)

func (c *cmdContext) vacation(args []string) int {
	if len(args) == 0 {
		return c.misuse("oximailctl vacation <get|set|clear> ...")
	}
	switch args[0] {
	case "get":
		return c.vacationGet(args[1:])
	case "set":
		return c.vacationSet(args[1:])
	case "clear":
		return c.vacationClear(args[1:])
	default:
		return c.misuse("oximailctl vacation <get|set|clear> ...")
	}
}

func (c *cmdContext) vacationGet(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl vacation get <address>")
	}
	acc, err := c.store.GetAccount(args[0])
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such account %q", args[0])
	}
	if err != nil {
		return c.fail("%v", err)
	}
	v, err := c.store.GetVacation(acc.ID)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(c.stdout, "vacation disabled for %s\n", acc.Address)
		return 0
	}
	if err != nil {
		return c.fail("%v", err)
	}
	state := "off"
	if v.Enabled {
		state = "on"
	}
	fmt.Fprintf(c.stdout, "vacation %s for %s (updated %s)\n", state, acc.Address, v.UpdatedAt)
	if v.Subject != "" {
		fmt.Fprintf(c.stdout, "  subject: %s\n", v.Subject)
	}
	fmt.Fprintf(c.stdout, "  body: %s\n", v.Body)
	if v.SuppressDays > 0 {
		fmt.Fprintf(c.stdout, "  suppress-days: %d\n", v.SuppressDays)
	}
	return 0
}

func (c *cmdContext) vacationSet(args []string) int {
	fs := flag.NewFlagSet("vacation set", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	subject := fs.String("subject", "", "auto-reply subject")
	body := fs.String("body", "", "auto-reply body (required)")
	suppress := fs.Int("suppress-days", 7, "do not re-reply to the same sender within this many days")
	disable := fs.Bool("disable", false, "store the rule but leave the responder off")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return c.misuse("oximailctl vacation set <address> -body \"...\" [-subject \"...\"] [-suppress-days N] [-disable]")
	}
	if *body == "" {
		return c.misuse("oximailctl vacation set: -body is required")
	}
	address := fs.Arg(0)
	acc, err := c.store.GetAccount(address)
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such account %q", address)
	}
	if err != nil {
		return c.fail("%v", err)
	}
	v, err := c.store.SetVacation(acc.ID, !*disable, *subject, *body, *suppress)
	if err != nil {
		return c.fail("set vacation: %v", err)
	}
	state := "off"
	if v.Enabled {
		state = "on"
	}
	fmt.Fprintf(c.stdout, "vacation %s for %s\n", state, acc.Address)
	return 0
}

func (c *cmdContext) vacationClear(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl vacation clear <address>")
	}
	acc, err := c.store.GetAccount(args[0])
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such account %q", args[0])
	}
	if err != nil {
		return c.fail("%v", err)
	}
	if err := c.store.DeleteVacation(acc.ID); err != nil {
		return c.fail("clear vacation: %v", err)
	}
	fmt.Fprintf(c.stdout, "vacation cleared for %s\n", acc.Address)
	return 0
}
