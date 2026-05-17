package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/parisxmas/OxiMail/internal/store"
)

// Backup file layout (tar, uncompressed for human-inspectability):
//
//   manifest.json    — top-level metadata: version, account address,
//                      number of mailboxes / messages, the schema
//                      version the dump was taken at.
//   account.json     — the full account document, including the
//                      bcrypt password hash so a restored account
//                      logs in with the original credentials.
//   vacation.json    — optional, the rule.
//   sieve.txt        — optional, the script source.
//   mailboxes/<name>.json
//                    — one file per mailbox: {mailbox: {...},
//                      messages: [{...}, ...]}. UIDValidity, UIDNext,
//                      HighestModSeq, and per-message UID / ModSeq /
//                      Flags are preserved verbatim.
//   blobs/<blob_key> — raw RFC 5322 bytes, one file per unique blob.

const backupFormatVersion = 1

type backupManifest struct {
	FormatVersion int    `json:"format_version"`
	Generator     string `json:"generator"`
	Address       string `json:"address"`
	Mailboxes     int    `json:"mailboxes"`
	Messages      int    `json:"messages"`
	Blobs         int    `json:"blobs"`
	GeneratedAt   string `json:"generated_at"`
}

type backupMailboxFile struct {
	Mailbox  *store.Mailbox   `json:"mailbox"`
	Messages []*store.Message `json:"messages"`
}

func (c *cmdContext) backup(args []string) int {
	if len(args) != 2 {
		return c.misuse("oximailctl backup <address> <path.tar>")
	}
	address, dest := args[0], args[1]
	acc, err := c.store.GetAccount(address)
	if errors.Is(err, store.ErrNotFound) {
		return c.fail("no such account %q", address)
	}
	if err != nil {
		return c.fail("%v", err)
	}

	f, err := os.Create(dest)
	if err != nil {
		return c.fail("create %q: %v", dest, err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()

	mailboxes, err := c.store.ListMailboxes(acc.ID)
	if err != nil {
		return c.fail("list mailboxes: %v", err)
	}

	manifest := backupManifest{
		FormatVersion: backupFormatVersion,
		Generator:     "oximailctl",
		Address:       acc.Address,
		Mailboxes:     len(mailboxes),
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	// account.json
	if err := tarJSON(tw, "account.json", acc); err != nil {
		return c.fail("%v", err)
	}

	// vacation.json (optional)
	if v, err := c.store.GetVacation(acc.ID); err == nil {
		if err := tarJSON(tw, "vacation.json", v); err != nil {
			return c.fail("%v", err)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return c.fail("%v", err)
	}

	// sieve.txt (optional)
	if sc, err := c.store.GetSieveScript(acc.ID); err == nil {
		if err := tarFile(tw, "sieve.txt", []byte(sc.Source)); err != nil {
			return c.fail("%v", err)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return c.fail("%v", err)
	}

	// mailboxes/<name>.json — and collect the unique blob set.
	seenBlobs := map[string]bool{}
	for i := range mailboxes {
		mb := &mailboxes[i]
		msgs, err := c.store.ListMessages(acc.ID, mb.ID)
		if err != nil {
			return c.fail("list messages in %q: %v", mb.Name, err)
		}
		msgPtrs := make([]*store.Message, len(msgs))
		for j := range msgs {
			msgPtrs[j] = &msgs[j]
		}
		if err := tarJSON(tw, "mailboxes/"+sanitizeName(mb.Name)+".json", backupMailboxFile{
			Mailbox:  mb,
			Messages: msgPtrs,
		}); err != nil {
			return c.fail("%v", err)
		}
		manifest.Messages += len(msgs)
		for j := range msgs {
			if msgs[j].BodyBlob != "" {
				seenBlobs[msgs[j].BodyBlob] = true
			}
		}
	}

	// blobs/<key>
	for key := range seenBlobs {
		body, err := c.store.FetchBody(&store.Message{BodyBlob: key})
		if err != nil {
			fmt.Fprintf(c.stderr, "oximailctl: skip missing blob %q: %v\n", key, err)
			continue
		}
		if err := tarFile(tw, "blobs/"+key, body); err != nil {
			return c.fail("%v", err)
		}
		manifest.Blobs++
	}

	if err := tarJSON(tw, "manifest.json", manifest); err != nil {
		return c.fail("%v", err)
	}
	fmt.Fprintf(c.stdout, "backed up %s → %s (%d mailboxes, %d messages, %d blobs)\n",
		acc.Address, dest, manifest.Mailboxes, manifest.Messages, manifest.Blobs)
	return 0
}

func (c *cmdContext) restore(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl restore <path.tar>")
	}
	src := args[0]
	f, err := os.Open(src)
	if err != nil {
		return c.fail("open %q: %v", src, err)
	}
	defer f.Close()

	// First pass: load everything into memory. Backup files are
	// per-account; even a fat mailbox is small enough to buffer.
	var (
		account      *store.Account
		vacation     *store.Vacation
		sieveSource  string
		mailboxFiles []backupMailboxFile
		blobs        = map[string][]byte{}
		manifest     backupManifest
	)

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return c.fail("read %s: %v", src, err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return c.fail("read %s entry %s: %v", src, hdr.Name, err)
		}
		switch {
		case hdr.Name == "manifest.json":
			if err := json.Unmarshal(body, &manifest); err != nil {
				return c.fail("parse manifest.json: %v", err)
			}
		case hdr.Name == "account.json":
			var acc store.Account
			if err := json.Unmarshal(body, &acc); err != nil {
				return c.fail("parse account.json: %v", err)
			}
			account = &acc
		case hdr.Name == "vacation.json":
			var v store.Vacation
			if err := json.Unmarshal(body, &v); err != nil {
				return c.fail("parse vacation.json: %v", err)
			}
			vacation = &v
		case hdr.Name == "sieve.txt":
			sieveSource = string(body)
		case strings.HasPrefix(hdr.Name, "mailboxes/"):
			var mf backupMailboxFile
			if err := json.Unmarshal(body, &mf); err != nil {
				return c.fail("parse %s: %v", hdr.Name, err)
			}
			mailboxFiles = append(mailboxFiles, mf)
		case strings.HasPrefix(hdr.Name, "blobs/"):
			key := strings.TrimPrefix(hdr.Name, "blobs/")
			blobs[key] = body
		}
	}

	if account == nil {
		return c.fail("backup is missing account.json")
	}
	if manifest.FormatVersion != 0 && manifest.FormatVersion != backupFormatVersion {
		return c.fail("backup format v%d, this oximailctl speaks v%d", manifest.FormatVersion, backupFormatVersion)
	}

	if _, err := c.store.GetAccount(account.Address); err == nil {
		return c.fail("account %q already exists; refusing to overwrite", account.Address)
	} else if !errors.Is(err, store.ErrNotFound) {
		return c.fail("%v", err)
	}

	newAcc, err := c.store.CreateAccount(account.Address, account.PasswordHash, account.QuotaBytes)
	if err != nil {
		return c.fail("create account: %v", err)
	}

	// Mailboxes — preserve UIDValidity / UIDNext / HighestModSeq.
	mbIDByName := map[string]uint64{}
	for i := range mailboxFiles {
		src := mailboxFiles[i].Mailbox
		src.AccountID = newAcc.ID
		mb, err := c.store.RestoreMailbox(src)
		if err != nil {
			return c.fail("restore mailbox %q: %v", src.Name, err)
		}
		mbIDByName[mb.Name] = mb.ID
	}

	// Messages — for each unique blob_key, the first occurrence
	// writes the bytes; subsequent occurrences just bump the
	// refcount. Track seenKeys across the whole restore.
	seenKeys := map[string]bool{}
	for i := range mailboxFiles {
		mf := mailboxFiles[i]
		newMboxID, ok := mbIDByName[mf.Mailbox.Name]
		if !ok {
			continue
		}
		for _, msg := range mf.Messages {
			msg.AccountID = newAcc.ID
			msg.MailboxID = newMboxID
			body, ok := blobs[msg.BodyBlob]
			if !ok {
				fmt.Fprintf(c.stderr, "oximailctl: blob %q for message UID=%d missing from backup; skipping\n", msg.BodyBlob, msg.UID)
				continue
			}
			newBlob := !seenKeys[msg.BodyBlob]
			if _, err := c.store.RestoreMessage(msg, body, newBlob); err != nil {
				return c.fail("restore message UID=%d in %q: %v", msg.UID, mf.Mailbox.Name, err)
			}
			seenKeys[msg.BodyBlob] = true
		}
	}

	// Vacation + sieve.
	if vacation != nil {
		if _, err := c.store.SetVacation(newAcc.ID, vacation.Enabled, vacation.Subject, vacation.Body, vacation.SuppressDays); err != nil {
			return c.fail("restore vacation: %v", err)
		}
	}
	if sieveSource != "" {
		if _, err := c.store.SetSieveScript(newAcc.ID, sieveSource); err != nil {
			return c.fail("restore sieve: %v", err)
		}
	}

	fmt.Fprintf(c.stdout, "restored %s from %s (%d mailboxes, %d blobs)\n",
		newAcc.Address, src, len(mailboxFiles), len(seenKeys))
	return 0
}

// tarJSON writes one JSON entry into the tar archive.
func tarJSON(tw *tar.Writer, name string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	return tarFile(tw, name, body)
}

// tarFile writes one raw entry.
func tarFile(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(body)),
		ModTime: time.Now(),
	}); err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("tar body %s: %w", name, err)
	}
	return nil
}

// sanitizeName replaces filesystem-unfriendly characters in a mailbox
// name so the tar entry path stays clean. "/" is the IMAP hierarchy
// delimiter — replace it with "%2F" (urlencoded) so the backup is
// flat under mailboxes/.
func sanitizeName(name string) string {
	return strings.ReplaceAll(name, "/", "%2F")
}
