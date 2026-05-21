import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';

import { ApiService } from '../api.service';

@Component({
  selector: 'oximail-login',
  imports: [FormsModule],
  template: `
    <div class="wrap">
      <!-- Decorative left panel — only visible at desktop widths. The
           wordmark and the tagline are the brand moment; the
           ornamental rule beneath sits in for the colophon. -->
      <aside class="cover" aria-hidden="true">
        <div class="cover-inner">
          <div class="mark">
            <span class="mark-glyph">℠</span>
            <span class="mark-word">OxiMail</span>
          </div>
          <h2 class="cover-title">
            Mail kept<br />
            <em>where you keep</em><br />
            the rest of your<br />
            correspondence.
          </h2>
          <div class="rule"></div>
          <p class="colophon">
            A private correspondence terminal.<br />
            <span class="muted">est. 2026 · baltavista.com</span>
          </p>
        </div>
      </aside>

      <form class="card" (ngSubmit)="submit()">
        <header>
          <span class="kicker">Sign in</span>
          <h1>Welcome back.</h1>
          <p class="sub">Open your mailbox.</p>
        </header>

        <label>
          <span class="label-text">Email</span>
          <input
            name="address"
            type="email"
            autocomplete="username"
            placeholder="you@yourdomain.com"
            [(ngModel)]="address"
            required
          />
        </label>

        <label>
          <span class="label-text">Password</span>
          <input
            name="password"
            type="password"
            autocomplete="current-password"
            placeholder="••••••••"
            [(ngModel)]="password"
            required
          />
        </label>

        @if (error()) {
          <p class="error">{{ error() }}</p>
        }

        <button class="primary" type="submit" [disabled]="busy()">
          {{ busy() ? 'Signing in…' : 'Continue' }}
          <span class="enter-hint" aria-hidden="true">↵</span>
        </button>
      </form>
    </div>
  `,
  styles: `
    :host { display: block; height: 100%; }

    .wrap {
      display: grid;
      grid-template-columns: 1.1fr 1fr;
      height: 100%;
      background: var(--bg);
    }
    @media (max-width: 880px) {
      .wrap { grid-template-columns: 1fr; }
      .cover { display: none; }
    }

    /* Cover panel — the "magazine cover" half. Subtle terracotta wash
       + a hairline border keeps it grounded. */
    .cover {
      position: relative;
      background:
        radial-gradient(80% 60% at 30% 20%, var(--accent-soft), transparent 70%),
        linear-gradient(180deg, var(--bg) 0%, var(--bg-muted) 100%);
      border-right: 1px solid var(--border);
      overflow: hidden;
    }
    .cover::before {
      /* Fine grain via repeating subtle dot pattern. The dots are
         translucent ink — they read as printed texture, not as a
         glitch. */
      content: '';
      position: absolute;
      inset: 0;
      background-image: radial-gradient(rgba(27, 24, 20, 0.05) 1px, transparent 1px);
      background-size: 18px 18px;
      mix-blend-mode: multiply;
      pointer-events: none;
      opacity: 0.5;
    }
    .cover-inner {
      position: relative;
      max-width: 460px;
      height: 100%;
      margin: 0 0 0 auto;
      padding: 48px 48px 48px 56px;
      display: flex;
      flex-direction: column;
      justify-content: space-between;
    }

    .mark {
      display: flex;
      align-items: baseline;
      gap: 10px;
    }
    .mark-glyph {
      font-family: var(--font-display);
      font-size: 28px;
      color: var(--accent);
      line-height: 1;
    }
    .mark-word {
      font-family: var(--font-display);
      font-size: 18px;
      font-weight: 500;
      letter-spacing: 0.02em;
      color: var(--text);
    }

    .cover-title {
      margin: 0;
      font-family: var(--font-display);
      font-weight: 350;
      font-size: clamp(36px, 5.2vw, 64px);
      line-height: 1.02;
      letter-spacing: -0.025em;
      color: var(--text);
      font-variation-settings: 'opsz' 144, 'SOFT' 60;
    }
    .cover-title em {
      font-style: italic;
      color: var(--accent);
      font-weight: 350;
      font-variation-settings: 'opsz' 144, 'SOFT' 100;
    }

    .rule {
      width: 64px;
      height: 1px;
      background: var(--text-muted);
      margin: 24px 0 16px;
      opacity: 0.4;
    }

    .colophon {
      margin: 0;
      font-family: var(--font-display);
      font-style: italic;
      font-size: 13px;
      color: var(--text-muted);
      line-height: 1.5;
    }
    .colophon .muted {
      font-style: normal;
      font-family: var(--font-mono);
      font-size: 11px;
      color: var(--text-soft);
      letter-spacing: 0.02em;
    }

    /* Form card — sits in the right column, vertically centered. */
    .card {
      align-self: center;
      justify-self: center;
      display: flex;
      flex-direction: column;
      gap: 14px;
      width: min(360px, calc(100% - 48px));
      padding: 32px 0;
    }

    header { display: flex; flex-direction: column; gap: 6px; margin-bottom: 6px; }
    .kicker {
      font-size: 10.5px;
      font-weight: 500;
      text-transform: uppercase;
      letter-spacing: 0.18em;
      color: var(--accent);
    }
    h1 {
      margin: 0;
      font-family: var(--font-display);
      font-weight: 360;
      font-size: 36px;
      line-height: 1.05;
      letter-spacing: -0.025em;
      color: var(--text);
      font-variation-settings: 'opsz' 144;
    }
    .sub {
      margin: 0;
      color: var(--text-muted);
      font-size: var(--text-md);
    }

    label {
      display: flex;
      flex-direction: column;
      gap: 6px;
    }
    .label-text {
      font-size: 10.5px;
      font-weight: 500;
      text-transform: uppercase;
      letter-spacing: 0.12em;
      color: var(--text-soft);
    }
    input {
      padding: 10px 12px;
      font-size: var(--text-md);
      background: var(--bg-muted);
      border-color: transparent;
      border-radius: 8px;
    }
    input:hover:not(:focus) { background: var(--bg-sunken); }
    input:focus {
      background: var(--bg);
      border-color: var(--accent);
    }

    button.primary {
      margin-top: 4px;
      padding: 11px 14px;
      font-size: var(--text-md);
      font-weight: 500;
      border-radius: 8px;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 8px;
      letter-spacing: -0.005em;
    }
    .enter-hint {
      font-family: var(--font-mono);
      font-size: 11px;
      opacity: 0.7;
      padding: 2px 6px;
      border-radius: 4px;
      background: rgba(250, 247, 240, 0.18);
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
  `,
})
export class LoginComponent {
  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  address = '';
  password = '';
  readonly error = signal('');
  readonly busy = signal(false);

  submit(): void {
    if (this.busy()) {
      return;
    }
    this.busy.set(true);
    this.error.set('');
    this.api.login(this.address, this.password).subscribe({
      next: () => {
        // ApiService.setAddress already ran in the login pipe.
        void this.router.navigate(['/mail']);
      },
      error: () => {
        this.error.set('Invalid email address or password.');
        this.busy.set(false);
      },
    });
  }
}
