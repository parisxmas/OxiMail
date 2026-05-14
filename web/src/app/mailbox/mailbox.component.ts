import { Component, OnInit, inject, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { Router } from '@angular/router';

import { ApiService } from '../api.service';
import { ComposeComponent } from '../compose/compose.component';
import {
  FLAG_FLAGGED,
  FLAG_SEEN,
  Mailbox,
  MessageDetail,
  MessageSummary,
} from '../models';

@Component({
  selector: 'oximail-mailbox',
  imports: [DatePipe, ComposeComponent],
  template: `
    <div class="app">
      <!-- Folder sidebar -->
      <aside class="folders">
        <div class="me" [title]="api.address()">{{ api.address() }}</div>
        <button class="primary compose-btn" (click)="composing.set(true)">
          Compose
        </button>
        @for (mb of mailboxes(); track mb.name) {
          <button
            class="folder"
            [class.active]="mb.name === selected()"
            (click)="selectMailbox(mb.name)"
          >
            <span class="folder-name">{{ mb.name }}</span>
            @if (mb.unseen > 0) {
              <span class="badge">{{ mb.unseen }}</span>
            }
          </button>
        }
        <button class="logout" (click)="logout()">Sign out</button>
      </aside>

      <!-- Message list -->
      <section class="list">
        <header>{{ selected() }}</header>
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
              <div class="row-from">{{ m.from || '(unknown sender)' }}</div>
              <div class="row-subject">{{ m.subject || '(no subject)' }}</div>
              <div class="row-date">{{ m.date | date: 'short' }}</div>
            </button>
          }
        }
      </section>

      <!-- Reader -->
      <section class="reader">
        @if (openMessage(); as msg) {
          <div class="reader-head">
            <h2>{{ msg.subject || '(no subject)' }}</h2>
            <div class="meta">
              <div><strong>From:</strong> {{ msg.from }}</div>
              @if (msg.to.length) {
                <div><strong>To:</strong> {{ msg.to.join(', ') }}</div>
              }
              @if (msg.cc.length) {
                <div><strong>Cc:</strong> {{ msg.cc.join(', ') }}</div>
              }
              <div class="date">{{ msg.date | date: 'medium' }}</div>
            </div>
            <div class="actions">
              <button (click)="toggleFlagged(msg)">
                {{ isFlagged(msg) ? 'Unflag' : 'Flag' }}
              </button>
              <button (click)="markUnread(msg)">Mark unread</button>
              <button (click)="moveToTrash(msg)">Move to Trash</button>
              <button class="danger" (click)="remove(msg)">Delete</button>
            </div>
          </div>
          <div class="reader-body">
            @if (msg.text) {
              <pre class="text-body">{{ msg.text }}</pre>
            } @else if (msg.html) {
              <!-- Angular sanitizes [innerHTML]; a production client
                   should render email HTML in a sandboxed iframe with a
                   strict CSP. -->
              <div class="html-body" [innerHTML]="msg.html"></div>
            } @else {
              <p class="hint">(empty message)</p>
            }
          </div>
          @if (msg.attachments.length) {
            <div class="attachments">
              <strong>Attachments</strong>
              @for (a of msg.attachments; track $index) {
                <span class="chip">
                  {{ a.filename || '(unnamed)' }} · {{ a.size }} bytes
                </span>
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
        (close)="composing.set(false)"
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
    .folders,
    .list {
      border-right: 1px solid var(--border);
      overflow-y: auto;
    }
    .folders {
      display: flex;
      flex-direction: column;
      gap: 4px;
      padding: 12px;
      background: var(--bg-muted);
    }
    .me {
      font-size: 12px;
      color: var(--text-muted);
      padding: 2px 4px 8px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .compose-btn {
      margin-bottom: 8px;
    }
    .folder {
      display: flex;
      justify-content: space-between;
      align-items: center;
      text-align: left;
      border: none;
      background: transparent;
      padding: 7px 8px;
    }
    .folder.active {
      background: var(--bg-sunken);
      font-weight: 600;
    }
    .badge {
      background: var(--accent);
      color: var(--accent-text);
      border-radius: 10px;
      padding: 0 7px;
      font-size: 11px;
    }
    .logout {
      margin-top: auto;
      border: none;
      background: transparent;
      color: var(--text-muted);
      text-align: left;
      padding: 7px 8px;
    }
    .list header,
    .reader-head h2 {
      margin: 0;
    }
    .list header {
      position: sticky;
      top: 0;
      background: var(--bg);
      padding: 12px;
      border-bottom: 1px solid var(--border);
      font-weight: 600;
    }
    .row {
      display: grid;
      grid-template-columns: 1fr auto;
      gap: 2px 8px;
      width: 100%;
      text-align: left;
      border: none;
      border-bottom: 1px solid var(--border);
      border-radius: 0;
      background: transparent;
      padding: 10px 12px;
    }
    .row.active {
      background: var(--bg-sunken);
    }
    .row-from {
      color: var(--text-muted);
      font-size: 13px;
    }
    .row.unread .row-from,
    .row.unread .row-subject {
      color: var(--unread);
      font-weight: 600;
    }
    .row-subject {
      grid-column: 1;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .row-date {
      grid-row: 1 / span 2;
      grid-column: 2;
      align-self: center;
      color: var(--text-muted);
      font-size: 12px;
    }
    .reader {
      display: flex;
      flex-direction: column;
      overflow-y: auto;
    }
    .reader-head {
      padding: 16px;
      border-bottom: 1px solid var(--border);
    }
    .meta {
      margin: 8px 0;
      font-size: 13px;
      color: var(--text-muted);
    }
    .meta .date {
      margin-top: 4px;
    }
    .actions {
      display: flex;
      gap: 8px;
    }
    .actions .danger {
      color: var(--danger);
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
export class MailboxComponent implements OnInit {
  protected readonly api = inject(ApiService);
  private readonly router = inject(Router);

  readonly mailboxes = signal<Mailbox[]>([]);
  readonly selected = signal<string>('INBOX');
  readonly messages = signal<MessageSummary[]>([]);
  readonly openMessage = signal<MessageDetail | null>(null);
  readonly composing = signal(false);
  readonly loadingList = signal(false);

  ngOnInit(): void {
    this.refreshMailboxes();
    this.loadMessages();
  }

  selectMailbox(name: string): void {
    if (name === this.selected()) {
      return;
    }
    this.selected.set(name);
    this.openMessage.set(null);
    this.loadMessages();
  }

  open(id: number): void {
    this.api.message(id).subscribe({
      next: (msg) => {
        this.openMessage.set(msg);
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

  onSent(): void {
    this.composing.set(false);
    this.refreshMailboxes();
    this.loadMessages();
  }

  logout(): void {
    this.api.clearSession();
    void this.router.navigate(['/login']);
  }

  private refreshMailboxes(): void {
    this.api.mailboxes().subscribe({
      next: (boxes) => this.mailboxes.set(boxes),
    });
  }

  private loadMessages(): void {
    this.loadingList.set(true);
    this.api.messages(this.selected()).subscribe({
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
