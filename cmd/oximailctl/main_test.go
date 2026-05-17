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

	t.Run("domain dkim refuses to overwrite without -force", func(t *testing.T) {
		// dkim.test had a key generated in the earlier subtest. Re-running
		// dkim must fail (without modifying the key), and the failure must
		// point the operator at -force and at dkim-show.
		before, err := st.GetDomain("dkim.test")
		if err != nil {
			t.Fatalf("get domain: %v", err)
		}
		out, code := cli("", "domain", "dkim", "dkim.test")
		if code == 0 {
			t.Fatalf("domain dkim re-run succeeded silently: %q", out)
		}
		if !strings.Contains(out, "-force") || !strings.Contains(out, "dkim-show") {
			t.Errorf("error message should mention -force and dkim-show: %q", out)
		}
		after, err := st.GetDomain("dkim.test")
		if err != nil {
			t.Fatalf("get domain (after): %v", err)
		}
		if after.DKIMPrivateKey != before.DKIMPrivateKey {
			t.Error("key changed despite the command refusing")
		}
	})

	t.Run("domain dkim -force replaces the key", func(t *testing.T) {
		before, err := st.GetDomain("dkim.test")
		if err != nil {
			t.Fatalf("get domain: %v", err)
		}
		out, code := cli("", "domain", "dkim", "-force", "-selector", "s2", "dkim.test")
		if code != 0 {
			t.Fatalf("dkim -force: code %d, output %q", code, out)
		}
		if !strings.Contains(out, "regenerated") || !strings.Contains(out, "s2._domainkey.dkim.test") {
			t.Errorf("dkim -force output is missing expected lines: %q", out)
		}
		after, err := st.GetDomain("dkim.test")
		if err != nil {
			t.Fatalf("get domain (after): %v", err)
		}
		if after.DKIMPrivateKey == before.DKIMPrivateKey {
			t.Error("key did not change after -force")
		}
		if after.DKIMSelector != "s2" {
			t.Errorf("selector did not update: got %q, want %q", after.DKIMSelector, "s2")
		}
	})

	t.Run("domain dkim-show prints the existing record (bit-identical to dkim)", func(t *testing.T) {
		// Fresh domain so we control the exact key we're comparing to.
		if _, code := cli("", "domain", "add", "show.test"); code != 0 {
			t.Fatal("setup: domain add failed")
		}
		genOut, code := cli("", "domain", "dkim", "show.test")
		if code != 0 {
			t.Fatalf("dkim: code %d, output %q", code, genOut)
		}
		// The TXT-record line is the one starting with "<selector>._domainkey".
		genTXT := findTXTLine(t, genOut, "oximail._domainkey.show.test")
		showOut, code := cli("", "domain", "dkim-show", "show.test")
		if code != 0 {
			t.Fatalf("dkim-show: code %d, output %q", code, showOut)
		}
		showTXT := strings.TrimSpace(showOut)
		if showTXT != genTXT {
			t.Errorf("dkim-show output differs from the generation-time TXT:\n  gen : %q\n  show: %q", genTXT, showTXT)
		}
	})

	t.Run("domain dkim-show errors when no key exists", func(t *testing.T) {
		if _, code := cli("", "domain", "add", "no-dkim.test"); code != 0 {
			t.Fatal("setup: domain add failed")
		}
		out, code := cli("", "domain", "dkim-show", "no-dkim.test")
		if code == 0 {
			t.Errorf("dkim-show on key-less domain succeeded: %q", out)
		}
		if !strings.Contains(out, "no DKIM key") {
			t.Errorf("error message should say 'no DKIM key': %q", out)
		}
	})

	t.Run("domain dkim-show errors on unknown domain", func(t *testing.T) {
		if out, code := cli("", "domain", "dkim-show", "no-such-domain.test"); code == 0 {
			t.Errorf("dkim-show on missing domain succeeded: %q", out)
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

	t.Run("sieve set / get / clear round-trips", func(t *testing.T) {
		if _, code := cli("pw\n", "account", "add", "filters@example.test"); code != 0 {
			t.Fatal("setup: account add failed")
		}
		script := `if header :contains "Subject" "report" { fileinto "Reports"; }` + "\n"
		// set — script comes from stdin
		out, code := cli(script, "sieve", "set", "filters@example.test")
		if code != 0 || !strings.Contains(out, "sieve script saved for filters@example.test") {
			t.Fatalf("sieve set: code=%d out=%q", code, out)
		}
		// get echoes the source back
		out, code = cli("", "sieve", "get", "filters@example.test")
		if code != 0 || !strings.Contains(out, "fileinto \"Reports\"") {
			t.Fatalf("sieve get: code=%d out=%q", code, out)
		}
		// a bad script is refused; the previous one stays in place.
		// `if {` has no test identifier — parser must reject.
		if _, code := cli("if { fileinto \"X\"; }", "sieve", "set", "filters@example.test"); code == 0 {
			t.Error("sieve set accepted a malformed script")
		}
		out, _ = cli("", "sieve", "get", "filters@example.test")
		if !strings.Contains(out, "fileinto \"Reports\"") {
			t.Errorf("a bad script replaced the working one; sieve get = %q", out)
		}
		// clear
		if out, code := cli("", "sieve", "clear", "filters@example.test"); code != 0 ||
			!strings.Contains(out, "sieve script cleared for filters@example.test") {
			t.Fatalf("sieve clear: code=%d out=%q", code, out)
		}
		if out, _ := cli("", "sieve", "get", "filters@example.test"); !strings.Contains(out, "no sieve script set for filters@example.test") {
			t.Errorf("sieve get after clear: out=%q", out)
		}
	})

	t.Run("vacation set / get / clear round-trips", func(t *testing.T) {
		if _, code := cli("pw\n", "account", "add", "ooo@example.test"); code != 0 {
			t.Fatal("setup: account add failed")
		}
		// set
		out, code := cli("", "vacation", "set", "-subject", "Away", "-body", "Back next week.",
			"ooo@example.test")
		if code != 0 || !strings.Contains(out, "vacation on for ooo@example.test") {
			t.Fatalf("vacation set: code=%d out=%q", code, out)
		}
		// get
		out, code = cli("", "vacation", "get", "ooo@example.test")
		if code != 0 || !strings.Contains(out, "vacation on for ooo@example.test") ||
			!strings.Contains(out, "subject: Away") || !strings.Contains(out, "Back next week.") {
			t.Fatalf("vacation get: code=%d out=%q", code, out)
		}
		// clear
		if out, code := cli("", "vacation", "clear", "ooo@example.test"); code != 0 ||
			!strings.Contains(out, "vacation cleared for ooo@example.test") {
			t.Fatalf("vacation clear: code=%d out=%q", code, out)
		}
		// get after clear
		out, code = cli("", "vacation", "get", "ooo@example.test")
		if code != 0 || !strings.Contains(out, "vacation disabled for ooo@example.test") {
			t.Fatalf("vacation get after clear: code=%d out=%q", code, out)
		}
	})

	t.Run("backup then restore round-trips an account", func(t *testing.T) {
		// Provision: a fresh account, INBOX seeded with one message.
		if _, code := cli("hunter2\n", "account", "add", "backup@example.test"); code != 0 {
			t.Fatal("setup: account add failed")
		}
		acc, err := st.GetAccount("backup@example.test")
		if err != nil {
			t.Fatalf("setup: get account: %v", err)
		}
		inbox, _ := st.GetMailboxByName(acc.ID, "INBOX")
		raw := []byte("From: <s@x>\r\nSubject: kept across restore\r\n\r\nbody\r\n")
		seeded, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
			Raw: raw, Subject: "kept across restore", FromAddr: "s@x",
		})
		if err != nil {
			t.Fatalf("seed inbox: %v", err)
		}
		// Add a vacation rule so the round-trip exercises that too.
		if _, code := cli("", "vacation", "set", "-subject", "Away", "-body", "Back Monday.",
			"backup@example.test"); code != 0 {
			t.Fatal("setup: vacation set failed")
		}

		// Backup.
		tarPath := t.TempDir() + "/backup@example.test.tar"
		if out, code := cli("", "backup", "backup@example.test", tarPath); code != 0 ||
			!strings.Contains(out, "backed up backup@example.test") {
			t.Fatalf("backup: code=%d out=%q", code, out)
		}

		// Delete the account, confirm it's gone.
		if _, code := cli("", "account", "delete", "backup@example.test"); code != 0 {
			t.Fatal("setup: account delete failed")
		}
		if _, err := st.GetAccount("backup@example.test"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("account still present after delete: %v", err)
		}

		// Restore.
		if out, code := cli("", "restore", tarPath); code != 0 ||
			!strings.Contains(out, "restored backup@example.test") {
			t.Fatalf("restore: code=%d out=%q", code, out)
		}

		// The account exists again and the original password works.
		if _, err := st.Authenticate("backup@example.test", "hunter2"); err != nil {
			t.Errorf("authenticate after restore: %v", err)
		}
		acc2, err := st.GetAccount("backup@example.test")
		if err != nil {
			t.Fatalf("get account after restore: %v", err)
		}
		// The seeded message is back, with its body intact.
		inbox2, _ := st.GetMailboxByName(acc2.ID, "INBOX")
		msgs, _ := st.ListMessages(inbox2.ID)
		if len(msgs) != 1 || msgs[0].Subject != "kept across restore" {
			t.Fatalf("restored INBOX = %+v, want one 'kept across restore' message", msgs)
		}
		body, err := st.FetchBody(&msgs[0])
		if err != nil {
			t.Fatalf("fetch restored body: %v", err)
		}
		if !strings.Contains(string(body), "body") {
			t.Errorf("restored body does not contain the seeded text: %q", body)
		}
		// The vacation rule is back too.
		v, err := st.GetVacation(acc2.ID)
		if err != nil || v.Subject != "Away" || v.Body != "Back Monday." {
			t.Errorf("vacation rule missing after restore: v=%+v err=%v", v, err)
		}
		// And the same backup cannot be restored on top of the now-existing account.
		if _, code := cli("", "restore", tarPath); code == 0 {
			t.Error("restore of an existing account should have failed")
		}
		_ = seeded
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

// findTXTLine returns the line in out whose first whitespace-separated
// token equals the expected owner-name (e.g. "oximail._domainkey.dkim.test").
// It fails the test if no such line is present. Used to extract the
// DKIM TXT record from multi-line CLI output for comparison against
// dkim-show.
func findTXTLine(t *testing.T, out, owner string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimSpace(line)
		// The DKIM TXT line uses an absolute owner-name (trailing dot).
		if strings.HasPrefix(s, owner+".") {
			return s
		}
	}
	t.Fatalf("no TXT line starting with %q in output:\n%s", owner, out)
	return ""
}
