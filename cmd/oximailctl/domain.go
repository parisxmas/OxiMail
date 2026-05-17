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
		return c.misuse("oximailctl domain <add|list|delete|dkim|dkim-show> ...")
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
	case "dkim-show":
		return c.domainDKIMShow(args[1:])
	default:
		return c.misuse("oximailctl domain <add|list|delete|dkim|dkim-show> ...")
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
//
// Refuses to regenerate when a key already exists unless -force is
// given. Regenerating silently used to be the default, which made it
// far too easy to break a deployment where the previous public key was
// already published — every signed message in flight would then fail
// DKIM verification at the recipient.
func (c *cmdContext) domainDKIM(args []string) int {
	fs := flag.NewFlagSet("domain dkim", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	selector := fs.String("selector", "oximail", "DKIM selector")
	force := fs.Bool("force", false, "regenerate even if a key already exists (invalidates any previously published DNS record)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return c.misuse("oximailctl domain dkim [-selector S] [-force] <domain>")
	}
	domain := fs.Arg(0)

	d, err := c.store.GetDomain(domain)
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such domain %q — add it first with `domain add`", domain)
	} else if err != nil {
		return c.fail("%v", err)
	}
	if d.DKIMPrivateKey != "" && !*force {
		return c.fail(
			"DKIM key for %s already exists (selector %q). "+
				"Use `oximailctl domain dkim-show %s` to view the TXT record, "+
				"or pass -force to regenerate (this invalidates the previously published DNS record)",
			domain, d.DKIMSelector, domain)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return c.fail("generate key: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := c.store.SetDKIMKey(domain, *selector, string(privPEM)); err != nil {
		return c.fail("store DKIM key: %v", err)
	}

	rec, err := dkimTXTRecord(domain, *selector, &key.PublicKey)
	if err != nil {
		return c.fail("%v", err)
	}
	action := "generated"
	if *force {
		action = "regenerated (forced)"
	}
	fmt.Fprintf(c.stdout, "DKIM key %s for %s (selector %q).\n\n", action, domain, *selector)
	fmt.Fprintf(c.stdout, "Publish this DNS TXT record, then mail from %s is signed:\n\n", domain)
	fmt.Fprintf(c.stdout, "  %s\n", rec)
	return 0
}

// domainDKIMShow reads the existing DKIM key for a domain and prints
// the public-key DNS TXT record. Read-only — useful when you've lost
// the original output from `dkim` and need to re-paste the record into
// DNS without regenerating (which would invalidate the published key).
func (c *cmdContext) domainDKIMShow(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl domain dkim-show <domain>")
	}
	domain := args[0]

	d, err := c.store.GetDomain(domain)
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such domain %q", domain)
	} else if err != nil {
		return c.fail("%v", err)
	}
	if d.DKIMPrivateKey == "" {
		return c.fail("no DKIM key for %s — generate one with `oximailctl domain dkim %s`", domain, domain)
	}
	block, _ := pem.Decode([]byte(d.DKIMPrivateKey))
	if block == nil {
		return c.fail("stored DKIM key for %s is not PEM-encoded", domain)
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return c.fail("parse stored DKIM key for %s: %v", domain, err)
	}
	rec, err := dkimTXTRecord(domain, d.DKIMSelector, &priv.PublicKey)
	if err != nil {
		return c.fail("%v", err)
	}
	fmt.Fprintf(c.stdout, "%s\n", rec)
	return 0
}

// dkimTXTRecord formats the DNS TXT record line for a DKIM public key.
// Shared by `dkim` (after generation) and `dkim-show` (read-back) so
// the on-disk and printed-after-the-fact records are bit-identical.
func dkimTXTRecord(domain, selector string, pub *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	return fmt.Sprintf("%s._domainkey.%s.  IN TXT  \"v=DKIM1; k=rsa; p=%s\"",
		selector, domain, base64.StdEncoding.EncodeToString(der)), nil
}
