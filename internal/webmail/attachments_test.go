package webmail

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestBlockedAttachmentExtension(t *testing.T) {
	cases := []struct {
		name        string
		filename    string
		wantBlocked bool
		wantExt     string
	}{
		// Allowed shapes.
		{"plain pdf", "report.pdf", false, ""},
		{"docx (non-macro)", "notes.docx", false, ""},
		{"zip is allowed (legit common case)", "archive.zip", false, ""},
		{"image", "screenshot.png", false, ""},
		{"no extension at all", "Makefile", false, ""},
		{"trailing dot only", "weird.", false, ""},

		// The blocklist hits.
		{"exe", "installer.exe", true, ".exe"},
		{"bat", "run.bat", true, ".bat"},
		{"vbs", "macro.vbs", true, ".vbs"},
		{"hta", "form.hta", true, ".hta"},
		{"jar", "tool.jar", true, ".jar"},
		{"iso (MotW evasion)", "build.iso", true, ".iso"},
		{"macro doc", "salary.docm", true, ".docm"},
		{"macro excel", "form.xlsm", true, ".xlsm"},

		// Case-insensitive — `.EXE` is still a Windows executable on
		// receipt; we MUST match it.
		{"uppercase EXE", "Setup.EXE", true, ".exe"},
		{"mixed case Bat", "Run.Bat", true, ".bat"},

		// Double-extension trickery: Windows binds the handler to the
		// last extension, so that's what we evaluate.
		{"double extension blocks on last", "report.pdf.exe", true, ".exe"},
		{"reverse double extension passes", "report.exe.pdf", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ext, blocked := blockedAttachmentExtension(c.filename)
			if blocked != c.wantBlocked {
				t.Errorf("filename=%q: blocked=%v, want %v", c.filename, blocked, c.wantBlocked)
			}
			if blocked && ext != c.wantExt {
				t.Errorf("filename=%q: ext=%q, want %q", c.filename, ext, c.wantExt)
			}
		})
	}
}

func TestDecodeAttachmentsRejectsBlockedExtension(t *testing.T) {
	// Validate the wiring: a .exe attachment going through the same
	// path the HTTP handler uses gets rejected with the dedicated
	// "blocked" message, not the size or base64 message. The error
	// is what handleSend / handleSaveDraft surface as the 400 body,
	// so the user sees a concrete why.
	in := []attachmentInput{
		{Filename: "installer.exe", ContentType: "application/octet-stream", Data: base64.StdEncoding.EncodeToString([]byte("MZ\x90\x00"))},
	}
	_, err := decodeAttachments(in)
	if err == nil {
		t.Fatal("decodeAttachments accepted a .exe; must reject")
	}
	if !strings.Contains(err.Error(), ".exe") || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error %q should name the blocked extension and say 'blocked'", err)
	}
}

func TestDecodeAttachmentsAllowsCleanFiles(t *testing.T) {
	// Sanity: the blocklist mustn't accidentally fire on innocent
	// filenames. A pdf + a png + a zip all go through with no error.
	in := []attachmentInput{
		{Filename: "report.pdf", ContentType: "application/pdf", Data: base64.StdEncoding.EncodeToString([]byte("%PDF-1.4"))},
		{Filename: "shot.png", ContentType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("\x89PNG"))},
		{Filename: "data.zip", ContentType: "application/zip", Data: base64.StdEncoding.EncodeToString([]byte("PK\x03\x04"))},
	}
	out, err := decodeAttachments(in)
	if err != nil {
		t.Fatalf("decodeAttachments rejected clean files: %v", err)
	}
	if len(out) != 3 {
		t.Errorf("got %d attachments, want 3", len(out))
	}
}
