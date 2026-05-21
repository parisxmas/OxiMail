// TypeScript types mirroring OxiMail's webmail JSON API
// (internal/webmail).

export interface Mailbox {
  name: string;
  subscribed: boolean;
  total: number;
  unseen: number;
}

export interface MessageSummary {
  id: number;
  uid: number;
  subject: string;
  from: string;
  date: string;
  size: number;
  flags: string[];
  seen: boolean;
  // Optional short body preview, populated only when the listing was
  // requested with ?snippets=1 (computed server-side from the message
  // body, capped at ~140 chars). Absent on listings that opted out.
  snippet?: string;
}

export interface Attachment {
  filename: string;
  content_type: string;
  size: number;
}

export interface MessageDetail extends MessageSummary {
  to: string[];
  cc: string[];
  message_id: string;
  text: string;
  html: string;
  attachments: Attachment[];
}

export interface LoginResult {
  token: string;
  address: string;
}

export interface SendResult {
  delivered: number;
  queued: number;
}

// IMAP system flags used by the UI.
export const FLAG_SEEN = '\\Seen';
export const FLAG_FLAGGED = '\\Flagged';

// Thread groups a set of MessageSummary entries that we believe belong
// to the same conversation. We compute threads client-side from the
// flat /api/mailboxes/{mailbox}/messages response — no backend change.
//
// Grouping key: subject with all leading "Re:"/"Fwd:"/"FW:"/"[tag]"
// prefixes stripped, trimmed and lowercased. This is the gmail-style
// "naive subject" rule. It misses subject-rewrite cases (where a
// reply changes the subject) and over-groups two unrelated messages
// that happen to share a subject — but for a personal mailbox at the
// sizes we deal with, those edges are rare and the win on the
// common "Re: foo" chain is huge.
export interface Thread {
  // Grouping key (normalized lowercase subject). Stable id for *ngFor.
  key: string;
  // Display subject — the canonical-cased subject of the latest message
  // with Re:/Fwd: stripped. Used for the list row title.
  subject: string;
  // Messages in the thread, newest first. messages[0] is always the
  // one we render in the row and open by default.
  messages: MessageSummary[];
  // Distinct senders (as raw From strings), newest first, capped at 3.
  // Drives the comma-joined sender label on the row.
  senders: string[];
  // Number of unread messages in the thread. Drives the bold/unread
  // styling and is shown as a count badge when > 0.
  unreadCount: number;
}
