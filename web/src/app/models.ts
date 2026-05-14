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
