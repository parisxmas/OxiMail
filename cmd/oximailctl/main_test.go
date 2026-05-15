//go:build integration

// Integration test for the oximailctl admin CLI. It boots a live
// oxidb-server, points the CLI at it via the OXIMAIL_* environment, and
// drives the run() entry point directly. Gated behind the `integration`
// build tag.
package main

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/store"
)

func TestCLI(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	t.Setenv("OXIMAIL_OXIDB_HOST", host)
	t.Setenv("OXIMAIL_OXIDB_PORT", strconv.Itoa(port))

	// cli runs one oximailctl invocation and returns its combined
	// output and exit code.
	cli := func(stdin string, args ...string) (string, int) {
		var out bytes.Buffer
		code := run(args, strings.NewReader(stdin), &out, &out)
		return out.String(), code
	}

	// A direct store handle, for verifying side effects.
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	t.Run("domain add and list", func(t *testing.T) {
		if out, code := cli("", "domain", "add", "example.test"); code != 0 {
			t.Fatalf("domain add: code %d, output %q", code, out)
		}
		out, code := cli("", "domain", "list")
		if code != 0 || !strings.Contains(out, "example.test") {
			t.Fatalf("domain list: code %d, output %q", code, out)
		}
	})

	t.Run("account add provisions a usable account", func(t *testing.T) {
		out, code := cli("s3cret\n", "account", "add", "-quota", "1048576", "user@example.test")
		if code != 0 {
			t.Fatalf("account add: code %d, output %q", code, out)
		}
		// It can authenticate with the password set on the CLI.
		acc, err := st.Authenticate("user@example.test", "s3cret")
		if err != nil {
			t.Fatalf("Authenticate after CLI add: %v", err)
		}
		if acc.QuotaBytes != 1048576 {
			t.Errorf("quota = %d, want 1048576", acc.QuotaBytes)
		}
		// And it has the default mailboxes.
		boxes, err := st.ListMailboxes(acc.ID)
		if err != nil {
			t.Fatalf("list mailboxes: %v", err)
		}
		if len(boxes) != 6 {
			t.Errorf("default mailboxes = %d, want 6", len(boxes))
		}
		// It shows up in the listing.
		if out, code := cli("", "account", "list"); code != 0 || !strings.Contains(out, "user@example.test") {
			t.Fatalf("account list: code %d, output %q", code, out)
		}
	})

	t.Run("account delete removes it", func(t *testing.T) {
		if out, code := cli("", "account", "delete", "user@example.test"); code != 0 {
			t.Fatalf("account delete: code %d, output %q", code, out)
		}
		if _, err := st.GetAccount("user@example.test"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("account still present after delete: err = %v", err)
		}
	})

	t.Run("alias add, list, delete", func(t *testing.T) {
		if _, code := cli("pw\n", "account", "add", "real@example.test"); code != 0 {
			t.Fatal("setup: account add failed")
		}
		if out, code := cli("", "alias", "add", "team@example.test", "real@example.test"); code != 0 {
			t.Fatalf("alias add: code %d, output %q", code, out)
		}
		out, code := cli("", "alias", "list")
		if code != 0 || !strings.Contains(out, "team@example.test") {
			t.Fatalf("alias list: code %d, output %q", code, out)
		}
		if out, code := cli("", "alias", "delete", "team@example.test"); code != 0 {
			t.Fatalf("alias delete: code %d, output %q", code, out)
		}
		if _, err := st.GetAlias("team@example.test"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("alias still present after delete: err = %v", err)
		}
	})

	t.Run("an empty password is rejected", func(t *testing.T) {
		if out, code := cli("\n", "account", "add", "empty@example.test"); code == 0 {
			t.Errorf("empty password accepted: output %q", out)
		}
	})

	t.Run("misuse exits 2", func(t *testing.T) {
		if _, code := cli("", "frobnicate"); code != 2 {
			t.Errorf("unknown command: code %d, want 2", code)
		}
		if _, code := cli("", "account"); code != 2 {
			t.Errorf("account with no verb: code %d, want 2", code)
		}
	})

	t.Run("domain dkim generates and stores a key", func(t *testing.T) {
		if _, code := cli("", "domain", "add", "dkim.test"); code != 0 {
			t.Fatal("setup: domain add failed")
		}
		out, code := cli("", "domain", "dkim", "-selector", "s1", "dkim.test")
		if code != 0 {
			t.Fatalf("domain dkim: code %d, output %q", code, out)
		}
		if !strings.Contains(out, "s1._domainkey.dkim.test") || !strings.Contains(out, "v=DKIM1") {
			t.Errorf("dkim output is missing the DNS record: %q", out)
		}
		// The key landed on the domain.
		d, err := st.GetDomain("dkim.test")
		if err != nil {
			t.Fatalf("get domain: %v", err)
		}
		if d.DKIMSelector != "s1" || d.DKIMPrivateKey == "" {
			t.Errorf("DKIM key not stored: selector=%q key-length=%d", d.DKIMSelector, len(d.DKIMPrivateKey))
		}
	})

	t.Run("domain dkim requires the domain to exist", func(t *testing.T) {
		if out, code := cli("", "domain", "dkim", "no-such-domain.test"); code == 0 {
			t.Errorf("domain dkim on a missing domain succeeded: output %q", out)
		}
	})

	t.Run("account passwd updates the password", func(t *testing.T) {
		if _, code := cli("first\n", "account", "add", "passwd-user@example.test"); code != 0 {
			t.Fatal("setup: account add failed")
		}
		// The original password works.
		if _, err := st.Authenticate("passwd-user@example.test", "first"); err != nil {
			t.Fatalf("setup: authenticate with original password: %v", err)
		}
		// Change it.
		if out, code := cli("second\n", "account", "passwd", "passwd-user@example.test"); code != 0 {
			t.Fatalf("account passwd: code %d, output %q", code, out)
		}
		// Old password no longer works; new one does.
		if _, err := st.Authenticate("passwd-user@example.test", "first"); err == nil {
			t.Error("old password still works after passwd")
		}
		if _, err := st.Authenticate("passwd-user@example.test", "second"); err != nil {
			t.Errorf("new password does not work: %v", err)
		}
	})

	t.Run("domain delete refuses while accounts remain", func(t *testing.T) {
		// example.test has accounts created earlier in this test.
		if out, code := cli("", "domain", "delete", "example.test"); code == 0 {
			t.Errorf("domain delete with accounts succeeded: %q", out)
		}
	})

	t.Run("domain delete works after the accounts are gone", func(t *testing.T) {
		if _, code := cli("", "domain", "add", "ephemeral.test"); code != 0 {
			t.Fatal("setup: domain add failed")
		}
		if out, code := cli("", "domain", "delete", "ephemeral.test"); code != 0 {
			t.Fatalf("domain delete: code %d, output %q", code, out)
		}
		if _, err := st.GetDomain("ephemeral.test"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("domain still present after delete: err = %v", err)
		}
	})
}
