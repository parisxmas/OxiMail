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

// BLOCKED_ATTACHMENT_EXTENSIONS mirrors blockedAttachmentExts in
// internal/webmail/handlers.go. Surfacing the same list client-side
// turns a "Send pressed → 400 from server" round-trip into an
// inline error the moment the user picks the file. If the two
// lists ever drift, the server is authoritative — the SPA is just a
// fast feedback layer.
const BLOCKED_ATTACHMENT_EXTENSIONS = new Set<string>([
  '.exe', '.bat', '.cmd', '.com', '.scr', '.pif', '.lnk',
  '.vbs', '.vbe', '.js', '.jse', '.wsf', '.wsh', '.hta',
  '.jar', '.ps1', '.ps2', '.msi', '.msp',
  '.iso', '.img', '.vhd', '.vhdx',
  '.docm', '.dotm', '.xlsm', '.xltm', '.xlsb',
  '.pptm', '.potm', '.ppam',
]);

function blockedAttachmentExtension(filename: string): string | null {
  const dot = filename.lastIndexOf('.');
  if (dot < 0 || dot === filename.length - 1) return null;
  const ext = filename.substring(dot).toLowerCase();
  return BLOCKED_ATTACHMENT_EXTENSIONS.has(ext) ? ext : null;
}

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
      font-family: var(--font-ui);
      color: var(--text);
    }

    .backdrop {
      position: absolute;
      inset: 0;
      background: rgba(27, 24, 20, 0.32);
      backdrop-filter: blur(2px);
      animation: backdrop-in 200ms var(--ease-quick);
    }
    @keyframes backdrop-in {
      from { opacity: 0; }
      to { opacity: 1; }
    }

    /* Compact mode — bottom-right snap-out, ~640px wide. The editor
       feels like a letterpress slip: warm paper background, a quiet
       hairline border, generous shadow that suggests it floats above
       the page. */
    .dialog {
      position: absolute;
      right: 24px;
      bottom: 24px;
      width: min(640px, calc(100vw - 48px));
      max-height: calc(100vh - 48px);
      display: flex;
      flex-direction: column;
      gap: 12px;
      padding: 18px 20px 14px;
      background: var(--bg);
      border: 1px solid var(--border);
      border-radius: 14px;
      box-shadow: var(--shadow-modal);
      overflow: hidden;
      animation: dialog-in 260ms var(--ease) backwards;
    }
    @keyframes dialog-in {
      from {
        opacity: 0;
        transform: translateY(12px) scale(0.99);
      }
      to {
        opacity: 1;
        transform: translateY(0) scale(1);
      }
    }

    /* Expanded mode — centered modal, full-bleed editor surface. */
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
    .dialog.expanded ::ng-deep quill-editor { flex: 1; min-height: 0; }
    .dialog.expanded ::ng-deep .ql-container.ql-snow { flex: 1; min-height: 0; }
    .dialog.expanded textarea { flex: 1; min-height: 0; }

    /* Header */
    header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding-bottom: 4px;
      border-bottom: 1px solid var(--border-soft);
    }
    h2 {
      margin: 0;
      font-family: var(--font-display);
      font-size: var(--text-lg);
      font-weight: 400;
      letter-spacing: -0.015em;
      color: var(--text);
      font-variation-settings: 'opsz' 144;
    }
    .header-actions { display: inline-flex; align-items: center; gap: 2px; }

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
      transition:
        background 140ms var(--ease-quick),
        color 140ms var(--ease-quick);
    }
    .icon-btn:hover {
      background: var(--bg-muted);
      color: var(--text);
    }

    /* Recipient + subject fields. Underline-only treatment — feels
       like filling in a printed slip rather than a CRM form. */
    label {
      display: flex;
      flex-direction: column;
      gap: 3px;
    }
    .label-text {
      font-family: var(--font-ui);
      font-size: 10.5px;
      font-weight: 500;
      text-transform: uppercase;
      letter-spacing: 0.14em;
      color: var(--text-soft);
    }
    label input {
      padding: 6px 0;
      background: transparent;
      border: none;
      border-bottom: 1px solid var(--border);
      border-radius: 0;
      font-size: var(--text-md);
      color: var(--text);
      transition: border-color 140ms var(--ease-quick);
    }
    label input:focus {
      outline: none;
      border-color: var(--accent);
      box-shadow: none;
    }
    label input::placeholder {
      color: var(--text-soft);
      font-style: italic;
      font-family: var(--font-display);
      font-size: var(--text-md);
    }

    /* Format toggle — segment-control pair of buttons, Apple-style. */
    .format-toggle {
      display: inline-flex;
      gap: 0;
      border: 1px solid var(--border);
      border-radius: 6px;
      padding: 2px;
      align-self: flex-start;
      background: var(--bg-muted);
    }
    .toggle {
      display: inline-flex;
      align-items: center;
      gap: 4px;
      padding: 4px 10px;
      font-size: var(--text-xs);
      font-weight: 500;
      border: none;
      background: transparent;
      border-radius: 4px;
      color: var(--text-muted);
      cursor: pointer;
      letter-spacing: 0.005em;
    }
    .toggle.active {
      background: var(--bg);
      box-shadow: 0 0 0 1px var(--border-soft), 0 1px 2px rgba(27, 24, 20, 0.04);
      color: var(--text);
      font-weight: 600;
    }

    /* Plain-text textarea */
    textarea {
      resize: vertical;
      min-height: 220px;
      font-family: var(--font-ui);
      font-size: var(--text-md);
      line-height: 1.6;
      background: var(--bg);
      border: 1px solid var(--border-soft);
      border-radius: 8px;
      padding: 12px 14px;
    }

    /* Quill editor overrides — paint the toolbar in our chrome so the
       editor stops looking like a generic Quill demo. */
    ::ng-deep quill-editor {
      display: flex;
      flex-direction: column;
      min-height: 280px;
      border-radius: 8px;
      overflow: hidden;
    }
    ::ng-deep .ql-toolbar.ql-snow {
      border: 1px solid var(--border-soft) !important;
      border-bottom: 1px solid var(--border) !important;
      border-top-left-radius: 8px;
      border-top-right-radius: 8px;
      background: var(--bg-muted);
      padding: 6px 8px !important;
    }
    ::ng-deep .ql-toolbar.ql-snow button,
    ::ng-deep .ql-toolbar.ql-snow .ql-picker-label {
      border-radius: 4px;
      transition: background 100ms var(--ease-quick);
    }
    ::ng-deep .ql-toolbar.ql-snow button:hover,
    ::ng-deep .ql-toolbar.ql-snow button.ql-active,
    ::ng-deep .ql-toolbar.ql-snow .ql-picker-label:hover,
    ::ng-deep .ql-toolbar.ql-snow .ql-picker-label.ql-active {
      background: var(--bg);
      color: var(--accent);
    }
    ::ng-deep .ql-toolbar.ql-snow .ql-stroke {
      stroke: var(--text-muted);
      transition: stroke 100ms var(--ease-quick);
    }
    ::ng-deep .ql-toolbar.ql-snow .ql-fill {
      fill: var(--text-muted);
      transition: fill 100ms var(--ease-quick);
    }
    ::ng-deep .ql-toolbar.ql-snow button:hover .ql-stroke,
    ::ng-deep .ql-toolbar.ql-snow button.ql-active .ql-stroke {
      stroke: var(--accent);
    }
    ::ng-deep .ql-toolbar.ql-snow button:hover .ql-fill,
    ::ng-deep .ql-toolbar.ql-snow button.ql-active .ql-fill {
      fill: var(--accent);
    }
    ::ng-deep .ql-toolbar.ql-snow .ql-picker {
      color: var(--text-muted);
    }
    ::ng-deep .ql-container.ql-snow {
      border: 1px solid var(--border-soft) !important;
      border-top: none !important;
      border-bottom-left-radius: 8px;
      border-bottom-right-radius: 8px;
      min-height: 220px;
      font-family: var(--font-ui);
      font-size: var(--text-md);
      line-height: 1.6;
      overflow: auto;
      background: var(--bg);
    }
    ::ng-deep .ql-editor {
      padding: 14px 16px !important;
      color: var(--text);
    }
    ::ng-deep .ql-editor.ql-blank::before {
      color: var(--text-soft) !important;
      font-style: italic !important;
      font-family: var(--font-display);
      left: 16px !important;
    }

    .error {
      margin: 0;
      padding: 8px 12px;
      color: var(--danger);
      background: var(--danger-soft);
      border-radius: 6px;
      font-size: var(--text-sm);
      border: 1px solid color-mix(in oklab, var(--danger), transparent 70%);
    }

    /* Footer (Cancel / Attach / Save draft / Send) */
    footer {
      display: flex;
      justify-content: flex-end;
      gap: 8px;
      padding-top: 6px;
      border-top: 1px solid var(--border-soft);
      flex-shrink: 0;
    }
    .icon-text { display: inline-flex; align-items: center; gap: 6px; }
    .ghost {
      background: transparent;
      border: 1px solid var(--border);
      color: var(--text-muted);
    }
    .ghost:hover {
      background: var(--bg-muted);
      color: var(--text);
      border-color: var(--text-soft);
    }
    button.primary {
      padding: 7px 14px;
      font-size: var(--text-sm);
      font-weight: 500;
      letter-spacing: -0.005em;
    }

    /* Attachment chips */
    .attachments {
      display: flex;
      flex-wrap: wrap;
      gap: 6px;
      align-items: center;
    }
    .att-chip {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      padding: 4px 8px;
      background: var(--bg-muted);
      border: 1px solid var(--border-soft);
      border-radius: 999px;
      font-size: var(--text-xs);
      font-family: var(--font-mono);
      color: var(--text);
    }
    .att-size { color: var(--text-soft); }
    .att-remove {
      background: transparent;
      border: none;
      color: var(--text-soft);
      padding: 0 2px;
      cursor: pointer;
      font-size: 13px;
      line-height: 1;
      transition: color 100ms var(--ease-quick);
    }
    .att-remove:hover { color: var(--danger); }
    .att-total {
      font-size: 10.5px;
      font-family: var(--font-ui);
      text-transform: uppercase;
      letter-spacing: 0.12em;
      color: var(--text-soft);
      margin-left: auto;
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
      const blockedExt = blockedAttachmentExtension(f.name);
      if (blockedExt) {
        this.error.set(
          `Attachment "${f.name}" is blocked (${blockedExt} files are a common malware vector). ` +
            `Wrap it in a zip if you really need to send it.`,
        );
        return;
      }
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
