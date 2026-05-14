package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/parisxmas/OxiMail/internal/store"
)

func (c *cmdContext) account(args []string) int {
	if len(args) == 0 {
		return c.misuse("oximailctl account <add|list|delete> ...")
	}
	switch args[0] {
	case "add":
		return c.accountAdd(args[1:])
	case "list":
		return c.accountList(args[1:])
	case "delete":
		return c.accountDelete(args[1:])
	default:
		return c.misuse("oximailctl account <add|list|delete> ...")
	}
}

func (c *cmdContext) accountAdd(args []string) int {
	fs := flag.NewFlagSet("account add", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	quota := fs.Int64("quota", 0, "mailbox quota in bytes (0 = unlimited)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return c.misuse("oximailctl account add [-quota N] <address>")
	}
	address := fs.Arg(0)

	password, err := c.readPassword()
	if err != nil {
		return c.fail("%v", err)
	}
	hash, err := store.HashPassword(password)
	if err != nil {
		return c.fail("%v", err)
	}
	acc, err := c.store.CreateAccount(address, hash, *quota)
	if err != nil {
		return c.fail("create account: %v", err)
	}
	if err := c.store.EnsureDefaultMailboxes(acc.ID); err != nil {
		return c.fail("create default mailboxes: %v", err)
	}
	fmt.Fprintf(c.stdout, "added account %s (id %d)\n", acc.Address, acc.ID)
	return 0
}

func (c *cmdContext) accountList(args []string) int {
	fs := flag.NewFlagSet("account list", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	domain := fs.String("domain", "", "list only accounts in this domain")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return c.misuse("oximailctl account list [-domain <domain>]")
	}
	accounts, err := c.store.ListAccounts(*domain)
	if err != nil {
		return c.fail("list accounts: %v", err)
	}
	for _, a := range accounts {
		fmt.Fprintf(c.stdout, "%s\tquota=%d\tused=%d\tactive=%t\n",
			a.Address, a.QuotaBytes, a.UsedBytes, a.Active)
	}
	return 0
}

func (c *cmdContext) accountDelete(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl account delete <address>")
	}
	acc, err := c.store.GetAccount(args[0])
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such account %q", args[0])
	}
	if err != nil {
		return c.fail("%v", err)
	}
	if err := c.store.DeleteAccount(acc.ID); err != nil {
		return c.fail("delete account: %v", err)
	}
	fmt.Fprintf(c.stdout, "deleted account %s and all of its mail\n", acc.Address)
	return 0
}
