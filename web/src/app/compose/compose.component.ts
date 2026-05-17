import { Component, inject, input, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { LucideAngularModule, Send, Save, X, Pilcrow, Code2, Maximize2, Minimize2 } from 'lucide-angular';
import { QuillEditorComponent } from 'ngx-quill';

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
  imports: [FormsModule, LucideAngularModule, QuillEditorComponent],
  template: `
    <div class="backdrop" (click)="cancel()"></div>
    <form class="dialog" [class.expanded]="expanded()" (ngSubmit)="submit()">
      <header>
        <h2>New message</h2>
        <div class="header-actions">
          <button
            type="button"
            class="icon-btn"
            (click)="toggleExpanded()"
            [attr.aria-label]="expanded() ? 'Shrink composer' : 'Expand composer'"
            [title]="expanded() ? 'Shrink' : 'Full screen'"
          >
            <i-lucide [img]="expanded() ? icons.Minimize2 : icons.Maximize2" [size]="16"></i-lucide>
          </button>
          <button type="button" class="icon-btn" (click)="cancel()" aria-label="Close" title="Close">
            <i-lucide [img]="icons.X" [size]="18"></i-lucide>
          </button>
        </div>
      </header>

      <label>
        <span class="label-text">To</span>
        <input
          name="to"
          type="text"
          placeholder="comma-separated addresses"
          [(ngModel)]="to"
          required
        />
      </label>

      <label>
        <span class="label-text">Cc</span>
        <input
          name="cc"
          type="text"
          placeholder="optional"
          [(ngModel)]="cc"
        />
      </label>

      <label>
        <span class="label-text">Subject</span>
        <input name="subject" type="text" [(ngModel)]="subject" />
      </label>

      <div class="format-toggle" role="tablist">
        <button
          type="button"
          class="toggle"
          [class.active]="htmlMode()"
          (click)="htmlMode.set(true)"
          role="tab"
          [attr.aria-selected]="htmlMode()"
          title="Rich text"
        >
          <i-lucide [img]="icons.Pilcrow" [size]="14"></i-lucide>
          Rich text
        </button>
        <button
          type="button"
          class="toggle"
          [class.active]="!htmlMode()"
          (click)="htmlMode.set(false)"
          role="tab"
          [attr.aria-selected]="!htmlMode()"
          title="Plain text"
        >
          <i-lucide [img]="icons.Code2" [size]="14"></i-lucide>
          Plain
        </button>
      </div>

      @if (htmlMode()) {
        <quill-editor
          name="html"
          [(ngModel)]="html"
          format="html"
          [styles]="expanded() ? quillStylesExpanded : quillStyles"
          theme="snow"
        ></quill-editor>
      } @else {
        <textarea
          name="text"
          rows="12"
          placeholder="Write your message…"
          [(ngModel)]="text"
        ></textarea>
      }

      @if (error()) {
        <p class="error">{{ error() }}</p>
      }

      <footer>
        <button type="button" class="ghost" (click)="cancel()">Cancel</button>
        <button type="button" class="ghost icon-text" (click)="saveDraft()" [disabled]="busy()">
          <i-lucide [img]="icons.Save" [size]="16"></i-lucide>
          {{ savedAt() ? 'Saved' : 'Save draft' }}
        </button>
        <button class="primary icon-text" type="submit" [disabled]="busy()">
          <i-lucide [img]="icons.Send" [size]="16"></i-lucide>
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
    /* Compact mode — bottom-right, like Gmail's snap-out composer.
       Right for quick replies; the expand button promotes it to the
       large centered .expanded variant. */
    .dialog {
      position: absolute;
      right: 24px;
      bottom: 24px;
      width: min(640px, calc(100vw - 48px));
      max-height: calc(100vh - 48px);
      display: flex;
      flex-direction: column;
      gap: 10px;
      padding: 18px 18px 14px;
      background: var(--bg);
      border: 1px solid var(--border);
      border-radius: 12px;
      box-shadow: 0 20px 48px rgba(0, 0, 0, 0.28);
      transition: width 160ms ease, height 160ms ease,
                  inset 160ms ease, border-radius 160ms ease;
    }
    /* Expanded mode — large centered modal. ~min(960px, 80vw) wide,
       fills 86vh tall. The editor body grows to take the slack so the
       user has a real surface to write into. */
    .dialog.expanded {
      right: auto;
      bottom: auto;
      top: 50%;
      left: 50%;
      transform: translate(-50%, -50%);
      width: min(960px, calc(100vw - 64px));
      height: 86vh;
      max-height: 86vh;
    }
    .dialog.expanded ::ng-deep quill-editor {
      flex: 1;
      min-height: 0;
    }
    .dialog.expanded ::ng-deep .ql-container.ql-snow {
      flex: 1;
      min-height: 0;
    }
    .dialog.expanded textarea {
      flex: 1;
      min-height: 0;
    }
    .header-actions {
      display: inline-flex;
      gap: 2px;
      align-items: center;
    }
    header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding-bottom: 4px;
      border-bottom: 1px solid var(--border);
    }
    h2 {
      margin: 0;
      font-size: 15px;
      font-weight: 600;
    }
    .icon-btn {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 32px;
      height: 32px;
      border: none;
      background: transparent;
      border-radius: 6px;
      color: var(--text-muted);
      cursor: pointer;
      transition: background 120ms ease, color 120ms ease;
    }
    .icon-btn:hover {
      background: var(--bg-sunken);
      color: var(--text);
    }
    label {
      display: flex;
      flex-direction: column;
      gap: 3px;
    }
    .label-text {
      font-size: 11px;
      color: var(--text-muted);
      text-transform: uppercase;
      letter-spacing: 0.04em;
    }
    textarea {
      resize: vertical;
      min-height: 220px;
      font-family: inherit;
    }
    .format-toggle {
      display: inline-flex;
      gap: 0;
      border: 1px solid var(--border);
      border-radius: 6px;
      padding: 2px;
      align-self: flex-start;
    }
    .toggle {
      display: inline-flex;
      align-items: center;
      gap: 4px;
      padding: 4px 10px;
      font-size: 12px;
      border: none;
      background: transparent;
      border-radius: 4px;
      color: var(--text-muted);
      cursor: pointer;
    }
    .toggle.active {
      background: var(--bg);
      box-shadow: 0 0 0 1px var(--border);
      color: var(--text);
      font-weight: 600;
    }
    /* Quill editor overrides — match the surrounding form's typography
       and let the editor body grow with the dialog. */
    ::ng-deep quill-editor {
      display: flex;
      flex-direction: column;
      min-height: 280px;
    }
    ::ng-deep .ql-toolbar.ql-snow {
      border-color: var(--border);
      border-top-left-radius: 6px;
      border-top-right-radius: 6px;
    }
    ::ng-deep .ql-container.ql-snow {
      border-color: var(--border);
      border-bottom-left-radius: 6px;
      border-bottom-right-radius: 6px;
      min-height: 220px;
      font-family: inherit;
      font-size: 14px;
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
      padding-top: 4px;
      border-top: 1px solid var(--border);
    }
    .icon-text {
      display: inline-flex;
      align-items: center;
      gap: 6px;
    }
    .ghost {
      background: transparent;
      border: 1px solid var(--border);
    }
    .ghost:hover:not(:disabled) {
      background: var(--bg-sunken);
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
  inReplyTo = '';
  references: string[] = [];
  draftId = 0;

  // Default to rich text — that's what a "professional" UI signals,
  // and the plain textarea is one click away in the toggle.
  readonly htmlMode = signal(true);
  readonly error = signal('');
  readonly busy = signal(false);
  readonly savedAt = signal<number>(0);

  // Icons referenced from the template — keep them as a single object
  // so the imports list above stays the one place that pins the icon
  // set (any new icon goes in both places).
  protected readonly icons = { Send, Save, X, Pilcrow, Code2, Maximize2, Minimize2 };

  // Expanded mode — large centered modal vs. the default bottom-right
  // panel. The initial value is context-driven (see ngOnInit): a
  // fresh "Compose" gets the expanded canvas because you usually
  // have something to say; a reply / forward / draft-resume opens
  // compact because you're acting in a thread you can already see.
  // The user can still flip mid-flow with the toolbar button.
  readonly expanded = signal<boolean>(false);

  toggleExpanded(): void {
    this.expanded.set(!this.expanded());
  }

  // Style passed to <quill-editor>. The compact mode locks the inner
  // editing area at 220px so the bottom-right panel stays a sensible
  // shape; the expanded mode hands the editor the full slack of the
  // centered dialog (height: 100% inside a flex parent).
  protected readonly quillStyles = { height: '220px' };
  protected readonly quillStylesExpanded = { height: '100%' };

  ngOnInit(): void {
    const s = this.seed();
    if (s) {
      this.to = s.to ?? '';
      this.cc = s.cc ?? '';
      this.subject = s.subject ?? '';
      this.text = s.text ?? '';
      this.html = s.html ?? '';
      // Resume in the mode that has content. Empty seed -> rich text.
      this.htmlMode.set((s.html ?? '') !== '' || (s.text ?? '') === '');
      this.inReplyTo = s.inReplyTo ?? '';
      this.references = s.references ?? [];
      this.draftId = s.draftId ?? 0;
      // A reply / forward / draft-resume opens compact — the user
      // can already see the thread underneath.
      this.expanded.set(false);
    } else {
      // A fresh "Compose" opens expanded — the user came to write,
      // give them the canvas.
      this.expanded.set(true);
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
