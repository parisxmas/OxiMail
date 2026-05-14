package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/parisxmas/OxiMail/internal/store"
)

func (c *cmdContext) alias(args []string) int {
	if len(args) == 0 {
		return c.misuse("oximailctl alias <add|list|delete> ...")
	}
	switch args[0] {
	case "add":
		return c.aliasAdd(args[1:])
	case "list":
		return c.aliasList(args[1:])
	case "delete":
		return c.aliasDelete(args[1:])
	default:
		return c.misuse("oximailctl alias <add|list|delete> ...")
	}
}

func (c *cmdContext) aliasAdd(args []string) int {
	if len(args) != 2 {
		return c.misuse("oximailctl alias add <address> <dest>[,<dest>...]")
	}
	var dests []string
	for _, d := range strings.Split(args[1], ",") {
		if d = strings.TrimSpace(d); d != "" {
			dests = append(dests, d)
		}
	}
	if len(dests) == 0 {
		return c.misuse("oximailctl alias add <address> <dest>[,<dest>...]")
	}
	al, err := c.store.CreateAlias(args[0], dests)
	if err != nil {
		return c.fail("create alias: %v", err)
	}
	fmt.Fprintf(c.stdout, "added alias %s -> %s\n", al.Address, strings.Join(al.Destinations, ", "))
	return 0
}

func (c *cmdContext) aliasList(args []string) int {
	if len(args) != 0 {
		return c.misuse("oximailctl alias list")
	}
	aliases, err := c.store.ListAliases()
	if err != nil {
		return c.fail("list aliases: %v", err)
	}
	for _, a := range aliases {
		fmt.Fprintf(c.stdout, "%s\t%s\n", a.Address, strings.Join(a.Destinations, ", "))
	}
	return 0
}

func (c *cmdContext) aliasDelete(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl alias delete <address>")
	}
	if _, err := c.store.GetAlias(args[0]); errors.Is(err, store.ErrNotFound) {
		return c.fail("no such alias %q", args[0])
	} else if err != nil {
		return c.fail("%v", err)
	}
	if err := c.store.DeleteAlias(args[0]); err != nil {
		return c.fail("delete alias: %v", err)
	}
	fmt.Fprintf(c.stdout, "deleted alias %s\n", args[0])
	return 0
}
