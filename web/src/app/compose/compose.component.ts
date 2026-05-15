import { Component, inject, input, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';

import { ApiService } from '../api.service';

// ComposeSeed pre-populates the dialog: reply / forward callers fill
// the threading fields, draft-resume callers fill the id.
export interface ComposeSeed {
  to?: string;
  cc?: string;
  subject?: string;
  text?: string;
  html?: string;
  inReplyTo?: string;
  references?: string[];
  draftId?: number;
}

@Component({
  selector: 'oximail-compose',
  imports: [FormsModule],
  template: `
    <div class="backdrop" (click)="cancel()"></div>
    <form class="dialog" (ngSubmit)="submit()">
      <header>
        <h2>New message</h2>
        <button type="button" class="close" (click)="cancel()" aria-label="Close">
          ✕
        </button>
      </header>

      <label>
        To
        <input
          name="to"
          type="text"
          placeholder="comma-separated addresses"
          [(ngModel)]="to"
          required
        />
      </label>

      <label>
        Cc
        <input
          name="cc"
          type="text"
          placeholder="optional"
          [(ngModel)]="cc"
        />
      </label>

      <label>
        Subject
        <input name="subject" type="text" [(ngModel)]="subject" />
      </label>

      <div class="format-toggle">
        <button
          type="button"
          class="toggle"
          [class.active]="!htmlMode()"
          (click)="htmlMode.set(false)"
        >
          Plain
        </button>
        <button
          type="button"
          class="toggle"
          [class.active]="htmlMode()"
          (click)="htmlMode.set(true)"
        >
          HTML
        </button>
      </div>

      @if (!htmlMode()) {
        <textarea
          name="text"
          rows="12"
          placeholder="Write your message…"
          [(ngModel)]="text"
        ></textarea>
      } @else {
        <textarea
          name="html"
          rows="12"
          placeholder="<p>Write HTML here…</p>"
          [(ngModel)]="html"
        ></textarea>
        <p class="hint">
          A plain-text fallback is generated for clients that cannot render HTML.
        </p>
      }

      @if (error()) {
        <p class="error">{{ error() }}</p>
      }

      <footer>
        <button type="button" (click)="cancel()">Cancel</button>
        <button type="button" (click)="saveDraft()" [disabled]="busy()">
          {{ savedAt() ? 'Saved' : 'Save draft' }}
        </button>
        <button class="primary" type="submit" [disabled]="busy()">
          {{ busy() ? 'Sending…' : 'Send' }}
        </button>
      </footer>
    </form>
  `,
  styles: `
    :host {
      position: fixed;
      inset: 0;
      z-index: 10;
    }
    .backdrop {
      position: absolute;
      inset: 0;
      background: rgba(0, 0, 0, 0.4);
    }
    .dialog {
      position: absolute;
      right: 24px;
      bottom: 24px;
      width: min(560px, calc(100vw - 48px));
      display: flex;
      flex-direction: column;
      gap: 10px;
      padding: 16px;
      background: var(--bg);
      border: 1px solid var(--border);
      border-radius: 10px;
      box-shadow: 0 12px 32px rgba(0, 0, 0, 0.28);
    }
    header {
      display: flex;
      align-items: center;
      justify-content: space-between;
    }
    h2 {
      margin: 0;
      font-size: 16px;
    }
    .close {
      border: none;
      background: transparent;
      padding: 4px 8px;
    }
    label {
      display: flex;
      flex-direction: column;
      gap: 3px;
      font-size: 12px;
      color: var(--text-muted);
    }
    textarea {
      resize: vertical;
    }
    .format-toggle {
      display: flex;
      gap: 4px;
    }
    .toggle {
      padding: 3px 10px;
      font-size: 12px;
    }
    .toggle.active {
      background: var(--bg-sunken);
      font-weight: 600;
    }
    .hint {
      margin: 0;
      color: var(--text-muted);
      font-size: 11px;
    }
    .error {
      margin: 0;
      color: var(--danger);
      font-size: 13px;
    }
    footer {
      display: flex;
      justify-content: flex-end;
      gap: 8px;
    }
  `,
})
export class ComposeComponent {
  private readonly api = inject(ApiService);

  /** Optional starting data for reply / forward / draft-resume. */
  readonly seed = input<ComposeSeed | null>(null);

  /** Emitted when the dialog is dismissed without sending. */
  readonly close = output<void>();
  /** Emitted after a message has been sent. */
  readonly sent = output<void>();

  to = '';
  cc = '';
  subject = '';
  text = '';
  html = '';
  // inReplyTo / references travel with the form so a Save Draft and a
  // later Send both preserve the threading metadata.
  inReplyTo = '';
  references: string[] = [];
  // draftId is set by the server on the first Save Draft and on a
  // resume; on subsequent saves we send it back so the existing draft
  // gets overwritten in place.
  draftId = 0;

  readonly htmlMode = signal(false);
  readonly error = signal('');
  readonly busy = signal(false);
  readonly savedAt = signal<number>(0); // unix ms of the last successful save

  ngOnInit(): void {
    const s = this.seed();
    if (s) {
      this.to = s.to ?? '';
      this.cc = s.cc ?? '';
      this.subject = s.subject ?? '';
      this.text = s.text ?? '';
      this.html = s.html ?? '';
      this.htmlMode.set((s.html ?? '') !== '');
      this.inReplyTo = s.inReplyTo ?? '';
      this.references = s.references ?? [];
      this.draftId = s.draftId ?? 0;
    }
  }

  cancel(): void {
    this.close.emit();
  }

  submit(): void {
    if (this.busy()) {
      return;
    }
    const to = splitAddresses(this.to);
    if (to.length === 0) {
      this.error.set('Add at least one recipient.');
      return;
    }
    this.busy.set(true);
    this.error.set('');
    // When sending HTML, generate a crude plain-text fallback by
    // stripping tags. The server then builds multipart/alternative.
    const html = this.htmlMode() ? this.html : '';
    const text = this.htmlMode() ? stripTags(this.html) : this.text;
    this.api
      .send({
        to,
        cc: splitAddresses(this.cc),
        subject: this.subject,
        text,
        html,
        in_reply_to: this.inReplyTo || undefined,
        references: this.references.length ? this.references : undefined,
      })
      .subscribe({
        next: () => this.sent.emit(),
        error: () => {
          this.error.set('Could not send the message. Please try again.');
          this.busy.set(false);
        },
      });
  }

  saveDraft(): void {
    if (this.busy()) {
      return;
    }
    this.busy.set(true);
    this.error.set('');
    const html = this.htmlMode() ? this.html : '';
    const text = this.htmlMode() ? stripTags(this.html) : this.text;
    this.api
      .saveDraft({
        id: this.draftId || undefined,
        to: splitAddresses(this.to),
        cc: splitAddresses(this.cc),
        subject: this.subject,
        text,
        html,
        in_reply_to: this.inReplyTo || undefined,
        references: this.references.length ? this.references : undefined,
      })
      .subscribe({
        next: (msg) => {
          this.draftId = msg.id;
          this.savedAt.set(Date.now());
          this.busy.set(false);
        },
        error: () => {
          this.error.set('Could not save the draft. Please try again.');
          this.busy.set(false);
        },
      });
  }
}

// splitAddresses parses a comma-separated address field into a trimmed,
// non-empty list.
function splitAddresses(raw: string): string[] {
  return raw
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

// stripTags is the same trick the server uses for body search: drop
// everything between '<' and '>'. Good enough as a fallback for clients
// that show only the text part.
function stripTags(s: string): string {
  return s.replace(/<[^>]*>/g, '');
}
