package main

import (
	"fmt"
	"sort"

	"github.com/parisxmas/OxiMail/internal/store"
)

func (c *cmdContext) mailbox(args []string) int {
	if len(args) == 0 {
		return c.misuse("oximailctl mailbox <list|dedupe> ...")
	}
	switch args[0] {
	case "list":
		return c.mailboxList(args[1:])
	case "dedupe":
		return c.mailboxDedupe(args[1:])
	default:
		return c.misuse("oximailctl mailbox <list|dedupe> ...")
	}
}

// mailboxList prints every mailbox belonging to one account, with its
// id, name, total message count, and unseen count. Useful for spotting
// duplicates by hand.
func (c *cmdContext) mailboxList(args []string) int {
	if len(args) != 1 {
		return c.misuse("oximailctl mailbox list <address>")
	}
	acc, err := c.store.GetAccount(args[0])
	if err != nil {
		return c.fail("get account %q: %v", args[0], err)
	}
	boxes, err := c.store.ListMailboxes(acc.ID)
	if err != nil {
		return c.fail("list mailboxes: %v", err)
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].ID < boxes[j].ID })
	for _, mb := range boxes {
		stats, _ := c.store.Stats(acc.ID, mb.ID)
		fmt.Fprintf(c.stdout, "  id=%d\tname=%q\ttotal=%d\tunseen=%d\n",
			mb.ID, mb.Name, stats.Total, stats.Unseen)
	}
	return 0
}

// mailboxDedupe collapses duplicate-by-name mailboxes for one account
// (or every account when called with no argument). For each (account,
// name) pair the SURVIVING mailbox is the one with the lowest _id; the
// other duplicates have their messages re-pointed at the survivor and
// are then deleted.
//
// This is a one-shot cleanup for the bug introduced when an aborted
// MigratePerAccount cycle (the OxiDB DropCollection "Not a directory"
// crash, since fixed) re-ran the legacy→per-account insert on every
// boot. Each retry created another copy of every default folder, so
// after ~25 restart-loop iterations every account had 25 INBOXes, 25
// Sent folders, etc.
//
// Safe to run on a clean install (no duplicates, nothing happens) and
// safe to re-run on a half-deduped state (the survivor is always the
// oldest by id, deterministically picked).
func (c *cmdContext) mailboxDedupe(args []string) int {
	if len(args) > 1 {
		return c.misuse("oximailctl mailbox dedupe [<address>]")
	}
	var accounts []store.Account
	if len(args) == 1 {
		acc, err := c.store.GetAccount(args[0])
		if err != nil {
			return c.fail("get account %q: %v", args[0], err)
		}
		accounts = []store.Account{*acc}
	} else {
		all, err := c.store.ListAccounts("")
		if err != nil {
			return c.fail("list accounts: %v", err)
		}
		accounts = all
	}

	total := 0
	for _, acc := range accounts {
		n, err := c.dedupOneAccount(&acc)
		if err != nil {
			return c.fail("%v", err)
		}
		total += n
	}
	fmt.Fprintf(c.stdout, "dedupe complete — removed %d duplicate mailbox(es) across %d account(s)\n", total, len(accounts))
	return 0
}

// dedupOneAccount is the per-account worker: groups mailboxes by name,
// keeps the lowest-id one per group, re-points every duplicate's
// messages at the survivor, and deletes the duplicate mailbox docs.
// Returns the number of mailbox docs deleted.
func (c *cmdContext) dedupOneAccount(acc *store.Account) (int, error) {
	boxes, err := c.store.ListMailboxes(acc.ID)
	if err != nil {
		return 0, fmt.Errorf("list mailboxes for %s: %w", acc.Address, err)
	}
	if len(boxes) == 0 {
		return 0, nil
	}

	// Group by mailbox NAME (case-sensitive — IMAP names are exact).
	byName := make(map[string][]store.Mailbox)
	for _, mb := range boxes {
		byName[mb.Name] = append(byName[mb.Name], mb)
	}

	removed := 0
	for name, group := range byName {
		if len(group) < 2 {
			continue
		}
		// Survivor = lowest _id (oldest).
		sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		keeper := group[0]
		dupes := group[1:]
		fmt.Fprintf(c.stdout, "  %s / %q: %d total, keeping id=%d, removing %d\n",
			acc.Address, name, len(group), keeper.ID, len(dupes))
		for _, dupe := range dupes {
			// Re-point this duplicate's messages at the survivor.
			// Allocate a fresh UID on the destination for each
			// message so the IMAP UIDValidity contract holds (the
			// keeper retains its own UID space; the merged
			// messages get new UIDs at the high end).
			msgs, err := c.store.ListMessages(acc.ID, dupe.ID)
			if err != nil {
				return removed, fmt.Errorf("list messages in mailbox %d: %w", dupe.ID, err)
			}
			for _, m := range msgs {
				if _, err := c.store.MoveMessage(acc.ID, m.ID, keeper.ID); err != nil {
					return removed, fmt.Errorf("move message %d %s→%s: %w",
						m.ID, name, name, err)
				}
			}
			// Now the duplicate mailbox is empty — drop it.
			if err := c.store.DeleteMailbox(acc.ID, dupe.ID); err != nil {
				return removed, fmt.Errorf("delete mailbox %d: %w", dupe.ID, err)
			}
			removed++
		}
	}
	return removed, nil
}
