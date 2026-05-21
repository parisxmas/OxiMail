package av

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// eicar is the canonical EICAR antivirus test string. Its sha256 lives
// in builtin.sigdb so a fresh New() recognises it without operator
// input; the tests below pin both ends of that contract.
var eicar = []byte("X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*")

func TestNewLoadsBuiltinEicar(t *testing.T) {
	c, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n := c.SignatureCount(); n < 1 {
		t.Errorf("SignatureCount = %d, want >= 1 (builtin sigdb)", n)
	}
	v, err := c.Scan(context.Background(), eicar)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v.OK {
		t.Error("EICAR scan returned OK; builtin signature should match")
	}
	if v.Threat != "EICAR-Test-File" {
		t.Errorf("Threat = %q, want EICAR-Test-File", v.Threat)
	}
}

func TestScanCleanIsOK(t *testing.T) {
	c, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := c.Scan(context.Background(), []byte("hello, world — clean as a whistle"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !v.OK {
		t.Errorf("clean payload returned OK=false (threat=%q)", v.Threat)
	}
}

func TestNilClientIsClean(t *testing.T) {
	// nil *Client is the "AV disabled" idiom — every scan clean,
	// no panic. Handlers consume this branchless.
	var c *Client
	v, err := c.Scan(context.Background(), eicar)
	if err != nil {
		t.Fatalf("nil.Scan: %v", err)
	}
	if !v.OK {
		t.Error("nil scanner should always return OK")
	}
}

func TestLoadExtraSignatures(t *testing.T) {
	// Operator-provided sigdb at OXIMAIL_AV_SIGDB is loaded on top
	// of the builtin. Add a fabricated hash and confirm Scan finds
	// it under the operator's chosen name.
	dir := t.TempDir()
	path := filepath.Join(dir, "extra.sigdb")
	// Hash of the string "totally-not-malware" — a known sha256.
	custom := "totally-not-malware"
	// Trying to keep the test data self-describing: we declare what
	// we're matching against, then bake the hash inline.
	knownHash := sha256OfString(custom)
	if err := os.WriteFile(path,
		[]byte("# test extra\n"+knownHash+":TEST-CUSTOM-HIT\n"), 0o644); err != nil {
		t.Fatalf("write extra sigdb: %v", err)
	}
	c, err := New(path)
	if err != nil {
		t.Fatalf("New(extra): %v", err)
	}
	if n := c.SignatureCount(); n < 2 {
		t.Errorf("SignatureCount = %d, want >= 2 (builtin + extra)", n)
	}
	v, err := c.Scan(context.Background(), []byte(custom))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v.OK || v.Threat != "TEST-CUSTOM-HIT" {
		t.Errorf("custom hash didn't match: %+v", v)
	}
	// Builtin EICAR still works with the extra DB loaded — the
	// override semantics are additive, not replace.
	v2, _ := c.Scan(context.Background(), eicar)
	if v2.OK {
		t.Error("EICAR no longer matches after loading extra sigdb")
	}
}

func TestLoadMissingExtraErrors(t *testing.T) {
	// A non-existent extra path is a real error — operator
	// misconfigured the env var. Better to fail loudly than
	// silently skip the file they thought they were loading.
	if _, err := New("/no/such/file.sigdb"); err == nil {
		t.Fatal("expected error for missing extra sigdb")
	}
}

func TestLoadRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.sigdb")
	if err := os.WriteFile(path, []byte("nothex:Name\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := New(path)
	if err == nil {
		t.Fatal("expected parse error for non-hex sha256")
	}
	if !strings.Contains(err.Error(), "bad hex") {
		t.Errorf("error %q should mention 'bad hex'", err)
	}
}

func TestLoadIgnoresCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tidy.sigdb")
	content := `
# header comment
   # indented comment

` + sha256OfString("payload-x") + `:Trojan.X

# trailing
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, _ := c.Scan(context.Background(), []byte("payload-x"))
	if v.OK || v.Threat != "Trojan.X" {
		t.Errorf("verdict = %+v, want hit on Trojan.X", v)
	}
}

func TestScanCtxCancelled(t *testing.T) {
	c, _ := New("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Scan(ctx, eicar); err == nil {
		t.Error("expected cancelled-context error")
	}
}

// sha256OfString returns the lowercase-hex sha256 of s. Inline helper
// so the test data above stays "what we're matching against" rather
// than a free-floating hex blob.
func sha256OfString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
