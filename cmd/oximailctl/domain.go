package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"

	"github.com/parisxmas/OxiMail/internal/store"
)

func (c *cmdContext) domain(args []string) int {
	if len(args) == 0 {
		return c.misuse("oximailctl domain <add|list|delete|dkim> ...")
	}
	switch args[0] {
	case "add":
		return c.domainAdd(args[1:])
	case "list":
		return c.domainList(args[1:])
	case "delete":
		return c.domainDelete(args[1:])
	case "dkim":
		return c.domainDKIM(args[1:])
	default:
		return c.misuse("oximailctl domain <add|list|delete|dkim> ...")
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
		dkim := "no"
		if d.DKIMPrivateKey != "" {
			dkim = d.DKIMSelector
		}
		fmt.Fprintf(c.stdout, "%s\tactive=%t\tdkim=%s\n", d.Domain, d.Active, dkim)
	}
	return 0
}

// domainDelete removes a hosted domain. The store refuses to delete a
// domain that still has accounts in it.
func (c *cmdContext) domainDelete(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl domain delete <domain>")
	}
	if _, err := c.store.GetDomain(args[0]); errors.Is(err, store.ErrNotFound) {
		return c.fail("no such domain %q", args[0])
	} else if err != nil {
		return c.fail("%v", err)
	}
	if err := c.store.DeleteDomain(args[0]); err != nil {
		return c.fail("%v", err)
	}
	fmt.Fprintf(c.stdout, "deleted domain %s\n", args[0])
	return 0
}

// domainDKIM generates an RSA signing key for a domain, stores it, and
// prints the public-key DNS TXT record the operator must publish.
func (c *cmdContext) domainDKIM(args []string) int {
	fs := flag.NewFlagSet("domain dkim", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	selector := fs.String("selector", "oximail", "DKIM selector")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return c.misuse("oximailctl domain dkim [-selector S] <domain>")
	}
	domain := fs.Arg(0)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return c.fail("generate key: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := c.store.SetDKIMKey(domain, *selector, string(privPEM)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return c.fail("no such domain %q — add it first with `domain add`", domain)
		}
		return c.fail("store DKIM key: %v", err)
	}

	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return c.fail("marshal public key: %v", err)
	}
	fmt.Fprintf(c.stdout, "DKIM key generated for %s (selector %q).\n\n", domain, *selector)
	fmt.Fprintf(c.stdout, "Publish this DNS TXT record, then mail from %s is signed:\n\n", domain)
	fmt.Fprintf(c.stdout, "  %s._domainkey.%s.  IN TXT  \"v=DKIM1; k=rsa; p=%s\"\n",
		*selector, domain, base64.StdEncoding.EncodeToString(pub))
	return 0
}
