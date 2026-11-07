import { ChangeDetectionStrategy, Component, computed, inject, input } from '@angular/core';
import { DomSanitizer } from '@angular/platform-browser';

// Styles for the frame document. The frame is opaque white whatever the
// app theme is, because mail is written for a light background.
const FRAME_CSS =
  'html{background:#fff;color:#202124}' +
  'body{margin:0;padding:8px 12px;font:14px/1.6 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;overflow-wrap:anywhere}' +
  'img{max-width:100%;height:auto}' +
  'a{color:#1a73e8}' +
  'blockquote{margin:12px 0;padding-left:12px;border-left:2px solid #dadce0;color:#5f6368}' +
  'pre{white-space:pre-wrap}';

// frameDocument wraps server-sanitised mail HTML in the frame's document.
// Its CSP narrows the app's (a srcdoc frame inherits that one as well),
// so even a bug in the sanitiser cannot fetch anything: remote images
// are allowed only once the reader has asked for them.
function frameDocument(html: string, showImages: boolean): string {
  const img = showImages ? 'data: https:' : 'data:';
  return (
    '<!doctype html><html><head><meta charset="utf-8">' +
    `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src ${img}; style-src 'unsafe-inline'">` +
    `<meta name="color-scheme" content="light"><style>${FRAME_CSS}</style></head>` +
    `<body>${html}</body></html>`
  );
}

// estimateHeight guesses the frame height in pixels. It has to guess:
// a frame with neither scripts nor same-origin access cannot report its
// content size to the parent. The frame scrolls and the reader can drag
// it taller, so a bad guess costs little.
function estimateHeight(html: string, showImages: boolean): number {
  const max = Math.round(window.innerHeight * 0.7);
  if (showImages && html.includes('<img')) {
    return max;
  }
  const chars = html.replace(/<[^>]*>/g, '').length;
  const blocks = (html.match(/<(br|p|div|tr|li|h[1-6]|img)\b/gi) ?? []).length;
  return Math.min(max, Math.max(96, 48 + 24 * (blocks + Math.ceil(chars / 80))));
}

// MailFrameComponent shows one HTML mail body in a sandboxed frame. The
// sandbox has neither allow-scripts nor allow-same-origin: nothing in
// the frame runs, and it cannot reach the app or its cookies.
// allow-popups lets links open in a new tab, and the escape flag lets
// the page they open work normally.
@Component({
  selector: 'oximail-mail-frame',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <iframe
      title="Message body"
      sandbox="allow-popups allow-popups-to-escape-sandbox"
      referrerpolicy="no-referrer"
      [style.height.px]="height()"
      [srcdoc]="srcdoc()"
    ></iframe>
  `,
  styles: `
    :host { display: block; }
    iframe {
      display: block;
      width: 100%;
      border: 0;
      resize: vertical;
      overflow: auto;
    }
  `,
})
export class MailFrameComponent {
  private readonly sanitizer = inject(DomSanitizer);

  // html has been through the server's sanitizeHTML.
  readonly html = input.required<string>();
  readonly showImages = input(false);

  // bypassSecurityTrustHtml is needed because Angular's own sanitiser
  // would strip the frame's CSP and styles from the srcdoc. computed()
  // also keeps the value stable, so the frame does not reload on every
  // change detection pass.
  protected readonly srcdoc = computed(() =>
    this.sanitizer.bypassSecurityTrustHtml(frameDocument(this.html(), this.showImages())),
  );
  protected readonly height = computed(() => estimateHeight(this.html(), this.showImages()));
}
