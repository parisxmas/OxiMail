import { Component, inject, input, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { LucideAngularModule, Send, Save, X, Pilcrow, Code2, Maximize2, Minimize2, Paperclip } from 'lucide-angular';
import { QuillEditorComponent } from 'ngx-quill';

import { ApiService, AttachmentUpload } from '../api.service';

// Hard cap on combined attachment size. Mirrors the server-side
// maxAttachmentBytes (25 MB) — surfaced as a client-side guard so the
// SPA shows a clear error before the upload, not after a wasted
// round-trip.
const MAX_ATTACHMENT_BYTES = 25 * 1024 * 1024;

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

      @if (attachments().length) {
        <div class="attachments">
          @for (a of attachments(); track $index) {
            <span class="att-chip" [title]="a.content_type">
              <i-lucide [img]="icons.Paperclip" [size]="12"></i-lucide>
              {{ a.filename }}
              <span class="att-size">· {{ formatBytes(a.sizeBytes) }}</span>
              <button
                type="button"
                class="att-remove"
                (click)="removeAttachment($index)"
                aria-label="Remove attachment"
                title="Remove"
              >✕</button>
            </span>
          }
          <span class="att-total">total {{ formatBytes(totalAttachmentBytes()) }}</span>
        </div>
      }

      @if (error()) {
        <p class="error">{{ error() }}</p>
      }

      <!-- Hidden multi-file picker; the toolbar button below triggers it -->
      <input
        #fileInput
        type="file"
        multiple
        hidden
        (change)="onFilesSelected($event)"
      />

      <footer>
        <button type="button" class="ghost" (click)="cancel()">Cancel</button>
        <button
          type="button"
          class="ghost icon-text"
          (click)="fileInput.click()"
          [disabled]="busy()"
          title="Attach files"
        >
          <i-lucide [img]="icons.Paperclip" [size]="16"></i-lucide>
          Attach
        </button>
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
      /* The Quill editor's contenteditable area expands with content
         and, in some browsers, overdraws past its flex parent. With
         the dialog's overflow at its default (visible), that overdraw
         would cover the footer and intercept pointer events — the
         buttons stay visible but clicks land on the invisible editor
         instead. Clip at the dialog and let inner regions scroll. */
      overflow: hidden;
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
      /* Pin overflow on the container — when the editor content
         exceeds the dialog's body region, content scrolls inside
         this container rather than pushing the footer offscreen
         or overdrawing other regions. */
      overflow: auto;
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
      /* Hard pin: the action row must never shrink. Without this, a
         tall body (long draft, big Quill toolbar dropdown) can squash
         the footer to 0 height in tight flex layouts. */
      flex-shrink: 0;
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
    /* Attachment chips — one per file the user has picked, plus a
       total-size label at the end. Removable via the inline ✕. */
    .attachments {
      display: flex;
      flex-wrap: wrap;
      gap: 6px;
      align-items: center;
      padding: 6px 0;
    }
    .att-chip {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      padding: 4px 8px;
      background: var(--bg-sunken);
      border: 1px solid var(--border);
      border-radius: 999px;
      font-size: 12px;
      color: var(--text);
    }
    .att-size {
      color: var(--text-muted);
      font-size: 11px;
    }
    .att-remove {
      background: transparent;
      border: none;
      color: var(--text-muted);
      cursor: pointer;
      padding: 0 0 0 2px;
      font-size: 12px;
      line-height: 1;
    }
    .att-remove:hover {
      color: var(--danger);
    }
    .att-total {
      font-size: 11px;
      color: var(--text-muted);
      margin-left: 4px;
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
  protected readonly icons = { Send, Save, X, Pilcrow, Code2, Maximize2, Minimize2, Paperclip };

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

  // Attachments queued by the user. Each entry carries the raw bytes
  // (base64-encoded once, at File-read time) so we don't repeat the
  // encode on every render. The sizeBytes field tracks the DECODED
  // size for the chip + total guard — what the server actually has
  // to store after base64 unwrap.
  readonly attachments = signal<PendingAttachment[]>([]);

  // totalAttachmentBytes is the sum of decoded sizes; used both for
  // the "total NN MB" chip and for the pre-upload size guard.
  protected totalAttachmentBytes(): number {
    return this.attachments().reduce((acc, a) => acc + a.sizeBytes, 0);
  }

  // onFilesSelected reads each picked file, base64-encodes it, and
  // appends to the attachments list. Files that would push the total
  // over MAX_ATTACHMENT_BYTES are rejected with an inline error
  // (matching the server-side guard).
  protected async onFilesSelected(event: Event): Promise<void> {
    const input = event.target as HTMLInputElement;
    const files = input.files ? Array.from(input.files) : [];
    input.value = ''; // allow re-picking the same file later
    if (files.length === 0) return;

    let total = this.totalAttachmentBytes();
    const additions: PendingAttachment[] = [];
    for (const f of files) {
      if (total + f.size > MAX_ATTACHMENT_BYTES) {
        this.error.set(
          `Attachment "${f.name}" pushes total over ${formatBytesStatic(MAX_ATTACHMENT_BYTES)}. ` +
            `Remove some files or split the message.`,
        );
        return;
      }
      const data = await readAsBase64(f);
      additions.push({
        filename: f.name,
        content_type: f.type || 'application/octet-stream',
        data,
        sizeBytes: f.size,
      });
      total += f.size;
    }
    this.attachments.update((cur) => cur.concat(additions));
    this.error.set('');
  }

  protected removeAttachment(index: number): void {
    this.attachments.update((cur) => cur.filter((_, i) => i !== index));
    this.error.set('');
  }

  // formatBytes returns a short human-readable size (e.g. "3.2 MB").
  // Same logic as formatBytesStatic; kept as a method for template
  // binding ergonomics (Angular templates can't call free functions).
  protected formatBytes(n: number): string {
    return formatBytesStatic(n);
  }

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
        attachments: this.toUploads(),
      })
      .subscribe({
        next: () => this.sent.emit(),
        error: (err) => {
          this.error.set(err?.error?.error || 'Could not send the message. Please try again.');
          this.busy.set(false);
        },
      });
  }

  // toUploads strips the chip-only sizeBytes field off pending
  // attachments so what we POST matches the server's attachmentInput
  // shape (filename / content_type / data).
  private toUploads(): AttachmentUpload[] | undefined {
    const list = this.attachments();
    if (!list.length) return undefined;
    return list.map((a) => ({
      filename: a.filename,
      content_type: a.content_type,
      data: a.data,
    }));
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
        attachments: this.toUploads(),
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

// PendingAttachment is the in-flight attachment record the compose
// component holds. data is already base64-encoded so the chip render
// + the eventual POST don't pay the encoding cost twice. sizeBytes is
// the DECODED length, used by chips and by the total-size guard.
interface PendingAttachment {
  filename: string;
  content_type: string;
  data: string;
  sizeBytes: number;
}

// readAsBase64 reads a File and returns its body base64-encoded. We
// use FileReader.readAsDataURL (which already returns a base64-encoded
// data URL) and strip the "data:...;base64," prefix — saves us a
// manual ArrayBuffer → base64 conversion path.
function readAsBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const result = reader.result as string;
      const comma = result.indexOf(',');
      resolve(comma >= 0 ? result.slice(comma + 1) : result);
    };
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(file);
  });
}

// formatBytesStatic renders a byte count as "1.2 KB" / "3.4 MB" /
// "5.6 GB". Three significant figures, two digits after the decimal
// for sub-10 values to read naturally.
function formatBytesStatic(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return v < 10 ? `${v.toFixed(2)} ${units[i]}` : `${v.toFixed(1)} ${units[i]}`;
}
