package main

import "fmt"

func (c *cmdContext) domain(args []string) int {
	if len(args) == 0 {
		return c.misuse("oximailctl domain <add|list> ...")
	}
	switch args[0] {
	case "add":
		return c.domainAdd(args[1:])
	case "list":
		return c.domainList(args[1:])
	default:
		return c.misuse("oximailctl domain <add|list> ...")
	}
}

func (c *cmdContext) domainAdd(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl domain add <domain>")
	}
	d, err := c.store.CreateDomain(args[0])
	if err != nil {
		return c.fail("create domain: %v", err)
	}
	fmt.Fprintf(c.stdout, "added domain %s\n", d.Domain)
	return 0
}

func (c *cmdContext) domainList(args []string) int {
	if len(args) != 0 {
		return c.misuse("oximailctl domain list")
	}
	domains, err := c.store.ListDomains()
	if err != nil {
		return c.fail("list domains: %v", err)
	}
	for _, d := range domains {
		fmt.Fprintf(c.stdout, "%s\tactive=%t\n", d.Domain, d.Active)
	}
	return 0
}
