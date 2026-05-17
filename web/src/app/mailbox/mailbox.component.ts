import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { Router, RouterLink } from '@angular/router';
import {
  LucideAngularModule,
  Archive,
  ChevronLeft,
  Edit3,
  Folder,
  Forward,
  Inbox,
  LogOut,
  Mail,
  MailOpen,
  Reply,
  ReplyAll,
  Search,
  Send,
  Settings,
  ShieldAlert,
  Star,
  Trash2,
} from 'lucide-angular';

import { ApiService } from '../api.service';
import { ComposeComponent, ComposeSeed } from '../compose/compose.component';
import {
  FLAG_FLAGGED,
  FLAG_SEEN,
  Mailbox,
  MessageDetail,
  MessageSummary,
} from '../models';

// Auto-refresh cadence for the open mailbox + folder counts. 10s strikes
// a balance between feeling live and the load it puts on the server
// (one /mailboxes + one /messages call per tab per cadence). A future
// upgrade would push these from the server via SSE/WebSocket keyed off
// the IMAP tracker's mailbox-update events instead of polling.
const REFRESH_INTERVAL_MS = 10_000;

@Component({
  selector: 'oximail-mailbox',
  imports: [DatePipe, ComposeComponent, RouterLink, LucideAngularModule],
  template: `
    <div class="app" [attr.data-view]="view()">
      <!-- Folder sidebar -->
      <aside class="folders">
        <div class="me" [title]="api.address()">{{ api.address() }}</div>
        <button class="compose-btn primary icon-text" (click)="openCompose()">
          <i-lucide [img]="icons.Edit3" [size]="16"></i-lucide>
          Compose
        </button>
        @for (mb of mailboxes(); track mb.name) {
          <button
            class="folder icon-text"
            [class.active]="mb.name === selected()"
            (click)="selectMailbox(mb.name)"
          >
            <i-lucide [img]="folderIcon(mb.name)" [size]="16"></i-lucide>
            <span class="folder-name">{{ mb.name }}</span>
            @if (mb.unseen > 0) {
              <span class="badge">{{ mb.unseen }}</span>
            }
          </button>
        }
        <a class="settings icon-text" routerLink="/settings">
          <i-lucide [img]="icons.Settings" [size]="16"></i-lucide>
          Settings
        </a>
        <button class="logout icon-text" (click)="logout()">
          <i-lucide [img]="icons.LogOut" [size]="16"></i-lucide>
          Sign out
        </button>
      </aside>

      <!-- Message list -->
      <section class="list">
        <header>
          <button
            class="back mobile-only icon-btn"
            type="button"
            (click)="view.set('folders')"
            aria-label="Back to folders"
          >
            <i-lucide [img]="icons.ChevronLeft" [size]="20"></i-lucide>
          </button>
          <span class="title icon-text">
            <i-lucide [img]="folderIcon(selected())" [size]="16"></i-lucide>
            {{ selected() }}
          </span>
          <div class="search-wrap">
            <i-lucide class="search-icon" [img]="icons.Search" [size]="14"></i-lucide>
            <input
              class="search"
              type="search"
              placeholder="Search…"
              [value]="query()"
              (input)="onSearchInput($event)"
            />
          </div>
        </header>
        @if (loadingList()) {
          <p class="hint">Loading…</p>
        } @else if (messages().length === 0) {
          <p class="hint">No messages.</p>
        } @else {
          @for (m of messages(); track m.id) {
            <button
              class="row"
              [class.unread]="!m.seen"
              [class.active]="m.id === openMessage()?.id"
              (click)="open(m.id)"
            >
              <span class="avatar" [style.background]="avatarColor(m.from)">{{ initials(m.from) }}</span>
              <div class="row-main">
                <div class="row-top">
                  <span class="row-from">{{ senderName(m.from) }}</span>
                  <span class="row-date">{{ m.date | date: 'shortDate' }}</span>
                </div>
                <div class="row-subject">{{ m.subject || '(no subject)' }}</div>
                @if (m.snippet) {
                  <div class="row-snippet">{{ m.snippet }}</div>
                }
              </div>
            </button>
          }
        }
      </section>

      <!-- Reader -->
      <section class="reader">
        @if (openMessage(); as msg) {
          <div class="reader-head">
            <button
              class="back mobile-only icon-btn"
              type="button"
              (click)="view.set('list')"
              aria-label="Back to list"
            >
              <i-lucide [img]="icons.ChevronLeft" [size]="20"></i-lucide>
            </button>
            <h2>{{ msg.subject || '(no subject)' }}</h2>
            <div class="reader-meta">
              <span class="avatar large" [style.background]="avatarColor(msg.from)">{{ initials(msg.from) }}</span>
              <div class="meta">
                <div class="meta-from">{{ msg.from }}</div>
                @if (msg.to?.length) {
                  <div class="meta-line">to {{ msg.to.join(', ') }}</div>
                }
                @if (msg.cc?.length) {
                  <div class="meta-line">cc {{ msg.cc.join(', ') }}</div>
                }
                <div class="meta-line date">{{ msg.date | date: 'medium' }}</div>
              </div>
            </div>
            <div class="actions">
              <button class="icon-btn" type="button" (click)="reply(msg, false)" title="Reply" aria-label="Reply">
                <i-lucide [img]="icons.Reply" [size]="18"></i-lucide>
              </button>
              <button class="icon-btn" type="button" (click)="reply(msg, true)" title="Reply all" aria-label="Reply all">
                <i-lucide [img]="icons.ReplyAll" [size]="18"></i-lucide>
              </button>
              <button class="icon-btn" type="button" (click)="forward(msg)" title="Forward" aria-label="Forward">
                <i-lucide [img]="icons.Forward" [size]="18"></i-lucide>
              </button>
              <span class="divider"></span>
              <button
                class="icon-btn"
                type="button"
                (click)="toggleFlagged(msg)"
                [title]="isFlagged(msg) ? 'Unflag' : 'Flag'"
                [attr.aria-label]="isFlagged(msg) ? 'Unflag' : 'Flag'"
                [class.flagged]="isFlagged(msg)"
              >
                <i-lucide [img]="icons.Star" [size]="18"></i-lucide>
              </button>
              <button class="icon-btn" type="button" (click)="markUnread(msg)" title="Mark unread" aria-label="Mark unread">
                <i-lucide [img]="icons.MailOpen" [size]="18"></i-lucide>
              </button>
              <button class="icon-btn" type="button" (click)="moveToTrash(msg)" title="Move to Trash" aria-label="Move to Trash">
                <i-lucide [img]="icons.Archive" [size]="18"></i-lucide>
              </button>
              <button class="icon-btn danger" type="button" (click)="remove(msg)" title="Delete" aria-label="Delete">
                <i-lucide [img]="icons.Trash2" [size]="18"></i-lucide>
              </button>
            </div>
          </div>
          <div class="reader-body">
            @if (msg.html) {
              <!-- Angular sanitizes [innerHTML]; a production client
                   should render email HTML in a sandboxed iframe with a
                   strict CSP. -->
              <div class="html-body" [innerHTML]="msg.html"></div>
            } @else if (msg.text) {
              <pre class="text-body">{{ msg.text }}</pre>
            } @else {
              <p class="hint">(empty message)</p>
            }
          </div>
          @if (msg.attachments.length) {
            <div class="attachments">
              <strong>Attachments</strong>
              @for (a of msg.attachments; track $index) {
                <button
                  type="button"
                  class="chip"
                  (click)="download(msg, $index, a.filename)"
                  [disabled]="downloading() === $index"
                >
                  {{ a.filename || '(unnamed)' }} · {{ a.size }} bytes
                </button>
              }
            </div>
          }
        } @else {
          <p class="hint center">Select a message to read it.</p>
        }
      </section>
    </div>

    @if (composing()) {
      <oximail-compose
        [seed]="composeSeed()"
        (close)="closeCompose()"
        (sent)="onSent()"
      />
    }
  `,
  styles: `
    .app {
      display: grid;
      grid-template-columns: 200px 320px 1fr;
      height: 100%;
    }
    .mobile-only { display: none; }
    /* Below ~720px we collapse to a single column and show only the
       column that matches the current view signal. Back buttons in
       the list header and reader header navigate between them. */
    @media (max-width: 720px) {
      .app {
        grid-template-columns: 1fr;
      }
      .app > * { display: none; }
      .app[data-view='folders'] .folders { display: flex; }
      .app[data-view='list'] .list { display: flex; flex-direction: column; }
      .app[data-view='reader'] .reader { display: flex; }
      .mobile-only.icon-btn {
        display: inline-flex;
        align-items: center;
        justify-content: center;
        width: 32px;
        height: 32px;
        background: transparent;
        border: none;
        color: var(--text-muted);
        margin-right: 4px;
      }
    }
    .folders,
    .list {
      border-right: 1px solid var(--border);
      overflow-y: auto;
    }
    .folders {
      display: flex;
      flex-direction: column;
      gap: 2px;
      padding: 14px 10px;
      background: var(--bg-muted);
    }
    .me {
      font-size: 12px;
      color: var(--text-muted);
      padding: 2px 6px 10px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .compose-btn {
      margin-bottom: 10px;
      padding: 9px 14px;
      font-weight: 600;
      border-radius: 8px;
      justify-content: center;
    }
    .icon-text {
      display: inline-flex;
      align-items: center;
      gap: 8px;
    }
    .folder {
      display: flex;
      align-items: center;
      gap: 8px;
      text-align: left;
      border: none;
      background: transparent;
      padding: 8px 10px;
      border-radius: 6px;
      color: var(--text);
      transition: background 100ms ease;
      cursor: pointer;
    }
    .folder:hover {
      background: var(--bg-sunken);
    }
    .folder.active {
      background: var(--bg-sunken);
      font-weight: 600;
      color: var(--accent);
    }
    .folder .folder-name {
      flex: 1;
    }
    .badge {
      background: var(--accent);
      color: var(--accent-text);
      border-radius: 10px;
      padding: 1px 8px;
      font-size: 11px;
      font-weight: 600;
    }
    .settings {
      margin-top: auto;
      color: var(--text-muted);
      text-decoration: none;
      padding: 8px 10px;
      font-size: 13px;
      border-radius: 6px;
      transition: background 100ms ease;
    }
    .settings:hover {
      background: var(--bg-sunken);
    }
    .logout {
      border: none;
      background: transparent;
      color: var(--text-muted);
      text-align: left;
      padding: 8px 10px;
      border-radius: 6px;
      transition: background 100ms ease;
      cursor: pointer;
    }
    .logout:hover {
      background: var(--bg-sunken);
    }
    .list header {
      position: sticky;
      top: 0;
      background: var(--bg);
      padding: 12px 14px;
      border-bottom: 1px solid var(--border);
      font-weight: 600;
      display: flex;
      align-items: center;
      gap: 10px;
      z-index: 1;
    }
    .list header .title {
      font-size: 14px;
      text-transform: capitalize;
    }
    .reader-head h2 {
      margin: 0 0 12px;
      font-size: 18px;
      font-weight: 600;
    }
    .search-wrap {
      position: relative;
      flex: 1;
      max-width: 220px;
    }
    .search-icon {
      position: absolute;
      left: 8px;
      top: 50%;
      transform: translateY(-50%);
      color: var(--text-muted);
      pointer-events: none;
    }
    .search {
      width: 100%;
      padding: 5px 8px 5px 26px;
      font-size: 12px;
      font-weight: normal;
      border-radius: 6px;
    }
    /* Message-list rows: avatar | sender+subject+snippet | date.
       Hover gives a subtle nudge; active is the selected message. */
    .row {
      display: grid;
      grid-template-columns: auto 1fr;
      gap: 10px;
      width: 100%;
      text-align: left;
      border: none;
      border-bottom: 1px solid var(--border);
      border-radius: 0;
      background: transparent;
      padding: 12px 14px;
      cursor: pointer;
      transition: background 80ms ease;
    }
    .row:hover {
      background: var(--bg-muted);
    }
    .row.active {
      background: var(--bg-sunken);
    }
    .avatar {
      display: inline-grid;
      place-items: center;
      width: 36px;
      height: 36px;
      border-radius: 50%;
      color: white;
      font-size: 13px;
      font-weight: 600;
      flex-shrink: 0;
      user-select: none;
    }
    .avatar.large {
      width: 44px;
      height: 44px;
      font-size: 15px;
    }
    .row-main {
      min-width: 0;
    }
    .row-top {
      display: flex;
      justify-content: space-between;
      align-items: baseline;
      gap: 8px;
    }
    .row-from {
      color: var(--text);
      font-size: 13px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .row-date {
      color: var(--text-muted);
      font-size: 11px;
      flex-shrink: 0;
    }
    .row.unread .row-from,
    .row.unread .row-subject {
      color: var(--unread);
      font-weight: 600;
    }
    .row-subject {
      font-size: 13px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      margin-top: 2px;
    }
    .row-snippet {
      font-size: 12px;
      color: var(--text-muted);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      margin-top: 2px;
    }
    .reader {
      display: flex;
      flex-direction: column;
      overflow-y: auto;
    }
    .reader-head {
      padding: 18px 20px;
      border-bottom: 1px solid var(--border);
    }
    .reader-meta {
      display: flex;
      gap: 12px;
      align-items: flex-start;
      margin: 10px 0;
    }
    .meta {
      flex: 1;
      font-size: 13px;
      color: var(--text-muted);
    }
    .meta-from {
      color: var(--text);
      font-weight: 500;
      font-size: 14px;
    }
    .meta-line {
      margin-top: 2px;
    }
    .meta .date {
      margin-top: 4px;
      font-size: 12px;
    }
    /* Action toolbar — icon buttons with a subtle hover, divider
       between thread actions and message-state actions. */
    .actions {
      display: flex;
      gap: 2px;
      margin-top: 8px;
      align-items: center;
    }
    .icon-btn {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 34px;
      height: 34px;
      border: none;
      background: transparent;
      border-radius: 6px;
      color: var(--text-muted);
      cursor: pointer;
      transition: background 100ms ease, color 100ms ease;
    }
    .icon-btn:hover {
      background: var(--bg-sunken);
      color: var(--text);
    }
    .icon-btn.flagged {
      color: #f5a623;
    }
    .icon-btn.danger:hover {
      background: rgba(220, 53, 69, 0.1);
      color: var(--danger);
    }
    .divider {
      width: 1px;
      height: 20px;
      background: var(--border);
      margin: 0 6px;
    }
    .reader-body {
      padding: 16px;
      flex: 1;
    }
    .text-body {
      margin: 0;
      white-space: pre-wrap;
      word-wrap: break-word;
      font: inherit;
    }
    .attachments {
      padding: 12px 16px;
      border-top: 1px solid var(--border);
      display: flex;
      flex-wrap: wrap;
      gap: 8px;
      align-items: center;
      font-size: 13px;
    }
    .chip {
      background: var(--bg-sunken);
      border-radius: 6px;
      padding: 3px 8px;
    }
    .hint {
      color: var(--text-muted);
      padding: 16px;
    }
    .hint.center {
      display: grid;
      place-items: center;
      height: 100%;
    }
  `,
})
export class MailboxComponent implements OnInit, OnDestroy {
  protected readonly api = inject(ApiService);
  private readonly router = inject(Router);

  // Icon set exposed to the template — one place to pin which icons
  // are imported above. Adding a new icon means adding it both to
  // the import list and to this map.
  protected readonly icons = {
    Archive, ChevronLeft, Edit3, Forward, Inbox, LogOut, MailOpen,
    Reply, ReplyAll, Search, Send, Settings, Star, Trash2,
  };

  readonly mailboxes = signal<Mailbox[]>([]);
  readonly selected = signal<string>('INBOX');
  readonly messages = signal<MessageSummary[]>([]);
  readonly openMessage = signal<MessageDetail | null>(null);
  readonly composing = signal(false);
  readonly composeSeed = signal<ComposeSeed | null>(null);
  // Mobile-only navigation state. On wide screens the CSS shows all
  // three columns regardless; on narrow screens the data-view attr
  // controls which one is visible.
  readonly view = signal<'folders' | 'list' | 'reader'>('list');
  readonly loadingList = signal(false);
  readonly query = signal<string>('');
  // index of the attachment currently being downloaded, or -1 for none.
  readonly downloading = signal<number>(-1);
  // search-input debounce timer; cleared on every keystroke.
  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  // Background poll that pulls fresh mailbox counts + list contents
  // every REFRESH_INTERVAL_MS so a user staring at the inbox sees a
  // new message without hitting reload. Cleared on destroy and paused
  // while the tab is hidden (no point keeping the connection warm to
  // a tab the user isn't looking at).
  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private readonly onVisibilityChange = () => {
    if (document.hidden) {
      this.stopPolling();
    } else {
      // Catch up immediately on tab refocus, then resume polling.
      this.quietRefresh();
      this.startPolling();
    }
  };

  ngOnInit(): void {
    this.refreshMailboxes();
    this.loadMessages();
    this.startPolling();
    document.addEventListener('visibilitychange', this.onVisibilityChange);
  }

  ngOnDestroy(): void {
    this.stopPolling();
    document.removeEventListener('visibilitychange', this.onVisibilityChange);
  }

  private startPolling(): void {
    if (this.pollTimer !== null || document.hidden) {
      return;
    }
    this.pollTimer = setInterval(() => this.quietRefresh(), REFRESH_INTERVAL_MS);
  }

  private stopPolling(): void {
    if (this.pollTimer !== null) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  // quietRefresh pulls fresh mailbox + message-list data without
  // toggling the loading spinner — the user is not waiting on this, so
  // the UI must not flicker. It also no-ops while a compose modal is
  // open so we don't yank the underlying list out from under it.
  private quietRefresh(): void {
    if (this.composing()) {
      return;
    }
    this.api.mailboxes().subscribe({
      next: (boxes) => this.mailboxes.set(boxes),
    });
    this.api.messages(this.selected(), this.query()).subscribe({
      next: (msgs) => this.messages.set(msgs),
    });
  }

  selectMailbox(name: string): void {
    if (name !== this.selected()) {
      this.selected.set(name);
      this.openMessage.set(null);
      this.query.set('');
      this.loadMessages();
    }
    this.view.set('list');
  }

  // onSearchInput debounces keystrokes by 250 ms before re-querying the
  // server — a body search costs a round-trip per match.
  onSearchInput(event: Event): void {
    const value = (event.target as HTMLInputElement).value;
    this.query.set(value);
    if (this.searchTimer !== null) {
      clearTimeout(this.searchTimer);
    }
    this.searchTimer = setTimeout(() => {
      this.searchTimer = null;
      this.loadMessages();
    }, 250);
  }

  // download fetches one attachment as a blob, then nudges the browser
  // to save it under the original filename.
  download(msg: MessageDetail, index: number, filename: string): void {
    if (this.downloading() === index) {
      return;
    }
    this.downloading.set(index);
    this.api.attachment(msg.id, index).subscribe({
      next: (blob: Blob) => {
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = filename || 'attachment';
        a.click();
        URL.revokeObjectURL(url);
        this.downloading.set(-1);
      },
      error: () => this.downloading.set(-1),
    });
  }

  open(id: number): void {
    this.api.message(id).subscribe({
      next: (msg) => {
        this.openMessage.set(msg);
        this.view.set('reader');
        // The API does not auto-mark on read, so the client does it.
        if (!msg.seen) {
          this.api.setFlags(id, 'add', [FLAG_SEEN]).subscribe({
            next: (updated) => this.applyUpdate(updated),
          });
        }
      },
    });
  }

  isFlagged(msg: MessageDetail): boolean {
    return msg.flags.includes(FLAG_FLAGGED);
  }

  toggleFlagged(msg: MessageDetail): void {
    const op = this.isFlagged(msg) ? 'remove' : 'add';
    this.api.setFlags(msg.id, op, [FLAG_FLAGGED]).subscribe({
      next: (updated) => this.applyUpdate(updated),
    });
  }

  markUnread(msg: MessageDetail): void {
    this.api.setFlags(msg.id, 'remove', [FLAG_SEEN]).subscribe({
      next: (updated) => this.applyUpdate(updated),
    });
  }

  moveToTrash(msg: MessageDetail): void {
    this.api.move(msg.id, 'Trash').subscribe({
      next: () => this.afterRemoval(),
    });
  }

  remove(msg: MessageDetail): void {
    this.api.remove(msg.id).subscribe({
      next: () => this.afterRemoval(),
    });
  }

  // reply opens the compose dialog pre-filled with a reply or
  // reply-all seed: To = original sender, Cc (reply-all) = original
  // To + Cc minus our own address, Subject with "Re: " prefix, body
  // is a quoted citation block, and the In-Reply-To / References
  // headers wire it into the thread.
  reply(msg: MessageDetail, all: boolean): void {
    let cc = '';
    if (all) {
      const me = this.api.address();
      const others = [...msg.to, ...msg.cc]
        .map(addressOnly)
        .filter((a) => a && a !== me);
      cc = Array.from(new Set(others)).join(', ');
    }
    this.composeSeed.set({
      to: addressOnly(msg.from),
      cc,
      subject: prefixedSubject(msg.subject, 'Re: '),
      text: quoteBody(msg),
      inReplyTo: msg.message_id,
      references: [msg.message_id],
    });
    this.composing.set(true);
  }

  // forward opens the compose dialog with the message inlined and the
  // recipient left blank.
  forward(msg: MessageDetail): void {
    this.composeSeed.set({
      to: '',
      subject: prefixedSubject(msg.subject, 'Fwd: '),
      text: forwardBody(msg),
    });
    this.composing.set(true);
  }

  // openCompose starts a fresh message — no seed.
  openCompose(): void {
    this.composeSeed.set(null);
    this.composing.set(true);
  }

  closeCompose(): void {
    this.composing.set(false);
    this.composeSeed.set(null);
  }

  onSent(): void {
    this.closeCompose();
    this.refreshMailboxes();
    this.loadMessages();
  }

  logout(): void {
    // Tell the server to revoke the session; even on error, drop local
    // state and bounce to the login page — the server is the authority
    // but the user clearly wants out either way.
    const done = () => {
      this.api.clearSession();
      void this.router.navigate(['/login']);
    };
    this.api.logout().subscribe({ next: done, error: done });
  }

  // folderIcon maps an IMAP mailbox name to a Lucide icon. Standard
  // folder names (case-insensitive) get a recognisable icon; anything
  // else falls back to a generic folder.
  protected folderIcon(name: string) {
    switch (name.toUpperCase()) {
      case 'INBOX': return Inbox;
      case 'SENT': return Send;
      case 'DRAFTS': return Edit3;
      case 'TRASH': return Trash2;
      case 'JUNK': return ShieldAlert;
      case 'ARCHIVE': return Archive;
      default: return Folder;
    }
  }

  // senderName extracts the human-readable part of an RFC 5322 From
  // header — "Alice <alice@x>" -> "Alice", or the address if no name
  // is present.
  protected senderName(from: string): string {
    if (!from) return '(unknown sender)';
    const m = from.match(/^\s*"?([^"<]+?)"?\s*<.+>$/);
    return m ? m[1].trim() : from.replace(/[<>]/g, '');
  }

  // initials returns one or two letters for the sender avatar. We
  // pick the first letter of the first two whitespace-separated tokens
  // of senderName; if there's only one token we fall back to its first
  // letter alone.
  protected initials(from: string): string {
    const name = this.senderName(from);
    const parts = name.split(/[\s.@]+/).filter(Boolean);
    if (parts.length === 0) return '?';
    if (parts.length === 1) return parts[0][0].toUpperCase();
    return (parts[0][0] + parts[1][0]).toUpperCase();
  }

  // avatarColor maps a sender to a stable HSL background so the same
  // sender always gets the same colour across reloads. Hash the
  // senderName for hue selection; saturation + lightness are fixed
  // to a palette that reads on both light and dark themes.
  protected avatarColor(from: string): string {
    const name = this.senderName(from);
    let hash = 0;
    for (let i = 0; i < name.length; i++) {
      hash = (hash * 31 + name.charCodeAt(i)) | 0;
    }
    const hue = Math.abs(hash) % 360;
    return `hsl(${hue}, 55%, 48%)`;
  }

  private refreshMailboxes(): void {
    this.api.mailboxes().subscribe({
      next: (boxes) => this.mailboxes.set(boxes),
    });
  }

  private loadMessages(): void {
    this.loadingList.set(true);
    this.api.messages(this.selected(), this.query()).subscribe({
      next: (msgs) => {
        this.messages.set(msgs);
        this.loadingList.set(false);
      },
      error: () => {
        this.messages.set([]);
        this.loadingList.set(false);
      },
    });
  }

  // applyUpdate folds a flag change back into the list and the open
  // message, and recomputes the current folder's unseen badge.
  private applyUpdate(updated: MessageSummary): void {
    this.messages.update((list) =>
      list.map((m) => (m.id === updated.id ? { ...m, ...updated } : m)),
    );
    const open = this.openMessage();
    if (open && open.id === updated.id) {
      this.openMessage.set({ ...open, flags: updated.flags, seen: updated.seen });
    }
    this.recountCurrent();
  }

  // afterRemoval reloads the folder after a message left it (move or
  // delete), and refreshes the folder counts.
  private afterRemoval(): void {
    this.openMessage.set(null);
    this.refreshMailboxes();
    this.loadMessages();
  }

  private recountCurrent(): void {
    const name = this.selected();
    const unseen = this.messages().filter((m) => !m.seen).length;
    this.mailboxes.update((boxes) =>
      boxes.map((b) => (b.name === name ? { ...b, unseen } : b)),
    );
  }
}

// addressOnly pulls "alice@x" out of "Alice <alice@x>"; on a plain
// address it is the identity.
function addressOnly(s: string): string {
  const start = s.lastIndexOf('<');
  const end = s.lastIndexOf('>');
  if (start >= 0 && end > start) {
    return s.slice(start + 1, end).trim();
  }
  return s.trim();
}

// prefixedSubject adds "Re: " / "Fwd: " unless the subject already
// starts with that prefix (case-insensitive) — so deep threads don't
// stack "Re: Re: Re:".
function prefixedSubject(subject: string, prefix: string): string {
  const lower = (subject || '').toLowerCase();
  if (lower.startsWith(prefix.toLowerCase())) {
    return subject;
  }
  return prefix + (subject || '');
}

// quoteBody builds the citation block that goes above the user's reply.
function quoteBody(msg: MessageDetail): string {
  const date = msg.date ? new Date(msg.date).toLocaleString() : '(unknown)';
  const header = `\n\nOn ${date}, ${msg.from} wrote:\n`;
  const body = (msg.text || stripHTML(msg.html || '')).split('\n').map((l) => '> ' + l).join('\n');
  return header + body;
}

// forwardBody builds the inline-forward block.
function forwardBody(msg: MessageDetail): string {
  const lines = [
    '',
    '',
    '---------- Forwarded message ----------',
    `From: ${msg.from}`,
    `To: ${msg.to.join(', ')}`,
    msg.cc.length ? `Cc: ${msg.cc.join(', ')}` : '',
    `Date: ${msg.date}`,
    `Subject: ${msg.subject}`,
    '',
    msg.text || stripHTML(msg.html || ''),
  ];
  return lines.filter((l) => l !== '').concat('').join('\n');
}

// stripHTML is the same crude tag-strip the server and the compose
// component use as a plain-text fallback.
function stripHTML(s: string): string {
  return s.replace(/<[^>]*>/g, '');
}
