import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';

import { AccountProfile, ApiService, SieveScript, VacationRule } from '../api.service';

@Component({
  selector: 'oximail-settings',
  imports: [FormsModule, RouterLink],
  template: `
    <div class="page">
      <header>
        <h1>Settings</h1>
        <a class="back" routerLink="/mail">← Back to mail</a>
      </header>

      <div class="tabs">
        <button
          type="button"
          class="tab"
          [class.active]="tab() === 'profile'"
          (click)="tab.set('profile')"
        >
          Profile
        </button>
        <button
          type="button"
          class="tab"
          [class.active]="tab() === 'vacation'"
          (click)="tab.set('vacation')"
        >
          Vacation auto-responder
        </button>
        <button
          type="button"
          class="tab"
          [class.active]="tab() === 'sieve'"
          (click)="tab.set('sieve')"
        >
          Filter rules (Sieve)
        </button>
        <button
          type="button"
          class="tab"
          [class.active]="tab() === 'password'"
          (click)="tab.set('password')"
        >
          Password
        </button>
      </div>

      @if (tab() === 'profile') {
        <section class="card">
          <h2>Profile</h2>
          <p class="hint">
            The display name appears in the From line of mail you send,
            so recipients see "Display Name &lt;{{ profileAddress() }}&gt;"
            instead of just the bare address. Leave it empty to send as
            the address alone.
          </p>

          <label>
            Email address
            <input type="email" [value]="profileAddress()" disabled />
          </label>

          <label>
            Display name
            <input
              type="text"
              maxlength="80"
              placeholder="e.g. Baris Akin"
              [(ngModel)]="profileDisplayName"
              name="profile-display-name"
            />
          </label>

          @if (profileError()) {
            <p class="error">{{ profileError() }}</p>
          }
          @if (profileSavedAt()) {
            <p class="ok">Profile updated.</p>
          }

          <footer>
            <button
              type="button"
              class="primary"
              [disabled]="profileBusy()"
              (click)="saveProfile()"
            >
              {{ profileBusy() ? 'Saving…' : 'Save profile' }}
            </button>
          </footer>
        </section>
      } @else if (tab() === 'password') {
        <section class="card">
          <h2>Change password</h2>
          <p class="hint">
            Your password is stored as a bcrypt hash — neither us nor
            anyone else can read it back. Changing it signs out every
            other browser and device, but keeps this session active.
          </p>

          <label>
            Current password
            <input
              type="password"
              autocomplete="current-password"
              [(ngModel)]="pwCurrent"
              name="pw-current"
            />
          </label>

          <label>
            New password (at least 8 characters)
            <input
              type="password"
              autocomplete="new-password"
              minlength="8"
              [(ngModel)]="pwNew"
              name="pw-new"
            />
          </label>

          <label>
            Confirm new password
            <input
              type="password"
              autocomplete="new-password"
              [(ngModel)]="pwConfirm"
              name="pw-confirm"
            />
          </label>

          @if (pwError()) {
            <p class="error">{{ pwError() }}</p>
          }
          @if (pwSavedAt()) {
            <p class="ok">Password updated.</p>
          }

          <footer>
            <button
              type="button"
              class="primary"
              [disabled]="pwBusy()"
              (click)="changePassword()"
            >
              {{ pwBusy() ? 'Updating…' : 'Update password' }}
            </button>
          </footer>
        </section>
      } @else if (tab() === 'vacation') {
        <section class="card">
          <h2>Vacation auto-responder</h2>
          <p class="hint">
            When enabled, OxiMail sends a canned reply to person-to-person mail
            that arrives at your address. The reply suppresses itself for
            bounces, mailing lists, and other auto-replies — and it won't
            re-fire to the same sender within the suppression window.
          </p>

          <label class="row">
            <input type="checkbox" [(ngModel)]="vacation.enabled" />
            Enable auto-reply
          </label>

          <label>
            Subject
            <input
              type="text"
              placeholder="Auto: Out of office"
              [(ngModel)]="vacation.subject"
            />
          </label>

          <label>
            Body
            <textarea
              rows="8"
              placeholder="I'm out until next Monday. For anything urgent please contact…"
              [(ngModel)]="vacation.body"
            ></textarea>
          </label>

          <label>
            Suppress re-replies for (days)
            <input
              type="number"
              min="1"
              max="90"
              [(ngModel)]="vacation.suppress_days"
            />
          </label>

          @if (vacationError()) {
            <p class="error">{{ vacationError() }}</p>
          }
          @if (vacationSavedAt()) {
            <p class="ok">Saved.</p>
          }

          <footer>
            <button
              type="button"
              class="danger"
              [disabled]="vacationBusy()"
              (click)="clearVacation()"
            >
              Clear & disable
            </button>
            <button
              type="button"
              class="primary"
              [disabled]="vacationBusy()"
              (click)="saveVacation()"
            >
              {{ vacationBusy() ? 'Saving…' : 'Save' }}
            </button>
          </footer>
        </section>
      } @else {
        <section class="card">
          <h2>Add a filter</h2>
          <p class="hint">
            Build one rule with the dropdowns. The "Add to script"
            button appends the equivalent Sieve to the editor below,
            where you can review or hand-edit. Stack multiple rules
            by clicking Add repeatedly.
          </p>
          <div class="rule-builder">
            <label class="rb-field">
              <span class="label-text">When</span>
              <select [(ngModel)]="rbField" name="rb-field">
                <option value="From">From</option>
                <option value="To">To</option>
                <option value="Cc">Cc</option>
                <option value="Subject">Subject</option>
              </select>
            </label>
            <label class="rb-field">
              <span class="label-text">{{ ' ' }}</span>
              <select [(ngModel)]="rbOp" name="rb-op">
                <option value="contains">contains</option>
                <option value="matches">matches (use * wildcard)</option>
                <option value="is">is exactly</option>
              </select>
            </label>
            <label class="rb-field rb-grow">
              <span class="label-text">value</span>
              <input
                type="text"
                placeholder="e.g. alice@example.com"
                [(ngModel)]="rbValue"
                name="rb-value"
              />
            </label>
            <label class="rb-field">
              <span class="label-text">then</span>
              <select [(ngModel)]="rbAction" name="rb-action">
                <option value="move">Move to folder…</option>
                <option value="delete">Delete (discard)</option>
              </select>
            </label>
            @if (rbAction() === 'move') {
              <label class="rb-field rb-grow">
                <span class="label-text">folder</span>
                <input
                  type="text"
                  placeholder="e.g. DMARC reports"
                  [(ngModel)]="rbTarget"
                  name="rb-target"
                />
              </label>
            }
          </div>
          @if (rbError()) {
            <p class="error">{{ rbError() }}</p>
          }
          <footer>
            <button type="button" class="primary" (click)="appendRule()">
              Add to script
            </button>
          </footer>
        </section>

        <section class="card">
          <h2>Filter rules (Sieve)</h2>
          <p class="hint">
            Sieve scripts run at delivery time. The supported subset covers
            <code>if</code> / <code>elsif</code> / <code>else</code>, the
            <code>header</code>, <code>address</code>, <code>size</code>,
            <code>exists</code>, <code>allof</code>, <code>anyof</code>, and
            <code>not</code> tests, and the <code>keep</code>,
            <code>fileinto</code>, <code>discard</code>, and
            <code>stop</code> actions.
          </p>
          <pre class="example">{{ sieveExample }}</pre>

          <textarea
            class="script"
            rows="16"
            placeholder="# Your sieve script…"
            [(ngModel)]="sieveSource"
          ></textarea>

          @if (sieveError()) {
            <p class="error">{{ sieveError() }}</p>
          }
          @if (sieveSavedAt()) {
            <p class="ok">Saved.</p>
          }

          <footer>
            <button
              type="button"
              class="danger"
              [disabled]="sieveBusy()"
              (click)="clearSieve()"
            >
              Clear script
            </button>
            <button
              type="button"
              class="primary"
              [disabled]="sieveBusy()"
              (click)="saveSieve()"
            >
              {{ sieveBusy() ? 'Saving…' : 'Save' }}
            </button>
          </footer>
        </section>
      }
    </div>
  `,
  styles: `
    :host {
      display: block;
      height: 100%;
      overflow-y: auto;
      background: var(--bg);
      color: var(--text);
      font-family: var(--font-ui);
    }
    .page {
      max-width: 760px;
      margin: 0 auto;
      padding: 36px 24px 80px;
    }

    /* Page header — editorial. Kicker label, serif headline, link
       back to mail. The whole strip carries the brand. */
    header.page-head, header {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 16px;
      padding-bottom: 18px;
      border-bottom: 1px solid var(--border);
      margin-bottom: 24px;
    }
    h1 {
      margin: 0;
      font-family: var(--font-display);
      font-size: 36px;
      font-weight: 380;
      letter-spacing: -0.025em;
      line-height: 1.05;
      color: var(--text);
      font-variation-settings: 'opsz' 144;
    }
    .back {
      color: var(--text-muted);
      text-decoration: none;
      font-size: var(--text-sm);
      transition: color 140ms var(--ease-quick);
    }
    .back:hover { color: var(--accent); }

    /* Tabs — typographic, no buttons. Active tab carries the
       accent underline. */
    .tabs {
      display: flex;
      gap: 6px;
      margin: 0 0 22px;
      border-bottom: 1px solid var(--border-soft);
    }
    .tab {
      padding: 10px 14px;
      border: none;
      background: transparent;
      border-radius: 0;
      color: var(--text-muted);
      font-size: var(--text-sm);
      font-weight: 500;
      letter-spacing: -0.005em;
      cursor: pointer;
      position: relative;
      margin-bottom: -1px;
      transition: color 140ms var(--ease-quick);
    }
    .tab:hover { color: var(--text); }
    .tab.active {
      color: var(--text);
      font-weight: 600;
    }
    .tab.active::after {
      content: '';
      position: absolute;
      left: 14px; right: 14px;
      bottom: -1px;
      height: 2px;
      background: var(--accent);
      border-radius: 1px;
    }

    /* Cards — clean parchment slabs with a hairline border. */
    .card {
      display: flex;
      flex-direction: column;
      gap: 16px;
      padding: 24px 26px;
      background: var(--bg);
      border: 1px solid var(--border-soft);
      border-radius: 12px;
      box-shadow: var(--shadow-line);
      margin-bottom: 18px;
    }
    .card h2 {
      margin: 0;
      font-family: var(--font-display);
      font-size: var(--text-xl);
      font-weight: 400;
      letter-spacing: -0.02em;
      color: var(--text);
      font-variation-settings: 'opsz' 144;
    }
    .hint {
      margin: 0;
      color: var(--text-muted);
      font-size: var(--text-sm);
      line-height: 1.55;
    }
    .hint code {
      font-family: var(--font-mono);
      font-size: 11.5px;
      background: var(--bg-muted);
      padding: 1px 6px;
      border-radius: 3px;
      color: var(--text);
    }

    .row { display: flex; align-items: center; gap: 10px; }
    label {
      display: flex;
      flex-direction: column;
      gap: 6px;
      font-size: var(--text-base);
      color: var(--text);
    }
    label .label-text {
      font-size: 10.5px;
      font-weight: 500;
      text-transform: uppercase;
      letter-spacing: 0.14em;
      color: var(--text-soft);
    }
    input, select, textarea {
      padding: 9px 12px;
      font-size: var(--text-base);
    }
    textarea {
      resize: vertical;
      font-family: var(--font-mono);
      font-size: 12.5px;
      line-height: 1.55;
    }
    textarea.script {
      min-height: 280px;
      background: var(--bg-muted);
      border: 1px solid var(--border-soft);
    }
    .example {
      background: var(--bg-muted);
      padding: 14px 16px;
      border-radius: 8px;
      border: 1px solid var(--border-soft);
      font-family: var(--font-mono);
      font-size: 11.5px;
      line-height: 1.6;
      white-space: pre-wrap;
      color: var(--text-muted);
    }
    .error {
      margin: 0;
      padding: 8px 12px;
      color: var(--danger);
      background: var(--danger-soft);
      border-radius: 6px;
      font-size: var(--text-sm);
      border: 1px solid color-mix(in oklab, var(--danger), transparent 75%);
    }
    .ok {
      margin: 0;
      padding: 6px 10px;
      color: var(--accent);
      background: var(--accent-soft);
      border-radius: 6px;
      font-size: var(--text-sm);
      display: inline-flex;
      align-items: center;
      gap: 6px;
    }
    .ok::before { content: '✓'; font-weight: 700; }
    footer {
      display: flex;
      justify-content: flex-end;
      gap: 8px;
      padding-top: 6px;
      border-top: none;
      margin-bottom: 0;
    }
    .danger { color: var(--danger); border-color: color-mix(in oklab, var(--danger), transparent 60%); }
    .danger:hover { background: var(--danger-soft); border-color: var(--danger); color: var(--danger); }

    /* Rule-builder mini-form: pretty wrapping flex row. */
    .rule-builder {
      display: flex;
      flex-wrap: wrap;
      gap: 10px;
      align-items: flex-end;
      padding: 14px 16px;
      background: var(--bg-muted);
      border: 1px solid var(--border-soft);
      border-radius: 10px;
    }
    .rb-field {
      flex: 0 0 auto;
      min-width: 130px;
      gap: 4px;
    }
    .rb-field.rb-grow { flex: 1 1 200px; }
    .rb-field select, .rb-field input { padding: 7px 10px; }
  `,
})
export class SettingsComponent {
  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  readonly tab = signal<'profile' | 'vacation' | 'sieve' | 'password'>('profile');

  // Profile form. The address is the immutable login identity — shown
  // as a disabled input for context, never PATCHed. Only display_name
  // is sent on save.
  readonly profileAddress = signal('');
  profileDisplayName = '';
  readonly profileBusy = signal(false);
  readonly profileError = signal('');
  readonly profileSavedAt = signal<number>(0);

  // Change-password form state. All three fields are kept in the
  // component (not the API) so a navigate-away doesn't persist
  // anything sensitive; the form is cleared on success.
  pwCurrent = '';
  pwNew = '';
  pwConfirm = '';
  readonly pwBusy = signal(false);
  readonly pwError = signal('');
  readonly pwSavedAt = signal<number>(0);

  // The example block goes through a property rather than literal
  // template text so the Angular parser doesn't try to interpret the
  // Sieve braces as interpolation.
  readonly sieveExample =
    'require ["fileinto"];\n\n' +
    'if header :contains "Subject" "report" {\n' +
    '    fileinto "Reports";\n' +
    '}\n\n' +
    'if header :contains "From" "spam@" {\n' +
    '    discard;\n' +
    '}';

  // Local form state.
  vacation: VacationRule = { enabled: false, subject: '', body: '', suppress_days: 7 };
  sieveSource = '';

  readonly vacationBusy = signal(false);
  readonly vacationError = signal('');
  readonly vacationSavedAt = signal<number>(0);

  readonly sieveBusy = signal(false);
  readonly sieveError = signal('');
  readonly sieveSavedAt = signal<number>(0);

  // Rule-builder form state. The fields drive a tiny single-condition
  // Sieve compiler in appendRule(); the output is just glued onto the
  // bottom of sieveSource so the user can review / hand-edit before
  // hitting Save. We don't auto-save — keeps the existing Save button
  // the only path that talks to the server.
  rbField: 'From' | 'To' | 'Cc' | 'Subject' = 'From';
  rbOp: 'contains' | 'matches' | 'is' = 'contains';
  rbValue = '';
  readonly rbAction = signal<'move' | 'delete'>('move');
  rbTarget = '';
  readonly rbError = signal('');

  ngOnInit(): void {
    // Load profile up front: it's the default tab, so the user
    // sees the current display name immediately on landing.
    this.api.getProfile().subscribe({
      next: (p) => {
        this.profileAddress.set(p.address);
        this.profileDisplayName = p.display_name ?? '';
      },
      error: () => void this.router.navigate(['/login']),
    });
    this.api.getVacation().subscribe({
      next: (v) => {
        // Server returns the all-zero default when nothing is set;
        // give the form a sensible suppress-days default in that
        // case so the spinner doesn't show 0.
        this.vacation = {
          enabled: v.enabled,
          subject: v.subject,
          body: v.body,
          suppress_days: v.suppress_days || 7,
        };
      },
      error: () => void this.router.navigate(['/login']),
    });
    this.api.getSieve().subscribe({
      next: (s) => (this.sieveSource = s.source ?? ''),
    });
  }

  saveVacation(): void {
    if (this.vacation.enabled && !this.vacation.body.trim()) {
      this.vacationError.set('Body is required when enabling the auto-reply.');
      return;
    }
    this.vacationBusy.set(true);
    this.vacationError.set('');
    this.api.putVacation(this.vacation).subscribe({
      next: () => {
        this.vacationSavedAt.set(Date.now());
        this.vacationBusy.set(false);
      },
      error: (err) => {
        this.vacationError.set(err.error?.error || 'Could not save the rule.');
        this.vacationBusy.set(false);
      },
    });
  }

  clearVacation(): void {
    this.vacationBusy.set(true);
    this.vacationError.set('');
    this.api.deleteVacation().subscribe({
      next: () => {
        this.vacation = { enabled: false, subject: '', body: '', suppress_days: 7 };
        this.vacationSavedAt.set(Date.now());
        this.vacationBusy.set(false);
      },
      error: () => {
        this.vacationError.set('Could not clear the rule.');
        this.vacationBusy.set(false);
      },
    });
  }

  saveSieve(): void {
    this.sieveBusy.set(true);
    this.sieveError.set('');
    this.api.putSieve(this.sieveSource).subscribe({
      next: () => {
        this.sieveSavedAt.set(Date.now());
        this.sieveBusy.set(false);
      },
      error: (err) => {
        // The server returns the parser error in the response body,
        // which is much more useful than a generic "Save failed".
        this.sieveError.set(err.error?.error || 'Save failed.');
        this.sieveBusy.set(false);
      },
    });
  }

  // changePassword validates the three inputs client-side (mirror of
  // what the server checks, surfaced as immediate feedback), then
  // calls the API. On success the form is cleared so leftover bytes
  // don't sit in the DOM longer than necessary, and a "Password
  // updated." confirmation shows for the next 5 seconds.
  changePassword(): void {
    this.pwError.set('');
    this.pwSavedAt.set(0);
    if (!this.pwCurrent) {
      this.pwError.set('Enter your current password.');
      return;
    }
    if (this.pwNew.length < 8) {
      this.pwError.set('New password must be at least 8 characters.');
      return;
    }
    if (this.pwNew === this.pwCurrent) {
      this.pwError.set('New password must differ from the current one.');
      return;
    }
    if (this.pwNew !== this.pwConfirm) {
      this.pwError.set('Confirmation does not match the new password.');
      return;
    }
    this.pwBusy.set(true);
    this.api.changePassword(this.pwCurrent, this.pwNew).subscribe({
      next: () => {
        this.pwCurrent = '';
        this.pwNew = '';
        this.pwConfirm = '';
        this.pwSavedAt.set(Date.now());
        this.pwBusy.set(false);
        // Clear the success banner after 5s so it doesn't linger.
        setTimeout(() => this.pwSavedAt.set(0), 5_000);
      },
      error: (err) => {
        this.pwError.set(err.error?.error || 'Could not change the password.');
        this.pwBusy.set(false);
      },
    });
  }

  // appendRule compiles the rule-builder form into a Sieve if-block
  // and appends it to the script textarea. We do NOT save here —
  // the user reviews the script in the editor and hits Save when
  // ready. That keeps the form a strict authoring helper, never an
  // auto-pusher behind the user's back.
  //
  // The compiled output has the same structure no matter what the
  // user picked: one `if header :OP "FIELD" "VALUE" { ACTIONS }`.
  // ACTIONS is `fileinto "X"; stop;` for Move or `discard;` for
  // Delete. `stop` after fileinto keeps later rules from also
  // touching the message — gmail behaviour for explicit filters.
  appendRule(): void {
    this.rbError.set('');
    const value = this.rbValue.trim();
    if (!value) {
      this.rbError.set('Enter a value to match against.');
      return;
    }
    let actions: string;
    if (this.rbAction() === 'move') {
      const target = this.rbTarget.trim();
      if (!target) {
        this.rbError.set('Enter a folder to move matching mail into.');
        return;
      }
      actions = `    fileinto "${escapeSieve(target)}";\n    stop;`;
    } else {
      actions = `    discard;`;
    }
    const block =
      `\n# Rule: ${this.rbField} ${this.rbOp} ${truncate(value, 60)}\n` +
      `if header :${this.rbOp} "${this.rbField}" "${escapeSieve(value)}" {\n` +
      `${actions}\n` +
      `}\n`;

    // Make sure the script has the right require[] line — appending a
    // fileinto needs `require ["fileinto"];` at the top, otherwise
    // the server-side parser rejects the whole script. If the user
    // has no script yet, we seed one with the right require.
    let next = this.sieveSource;
    const needsFileinto = this.rbAction() === 'move' && !/require\s*\[[^\]]*"fileinto"/.test(next);
    if (needsFileinto) {
      next = `require ["fileinto"];\n` + next;
    }
    this.sieveSource = (next + block).replace(/\n{3,}/g, '\n\n');

    // Clear the value/target so a repeat Add doesn't accidentally
    // duplicate the same rule on a stray click. Leave field/op/action
    // alone since users often build several rules of the same shape.
    this.rbValue = '';
    this.rbTarget = '';
  }

  // saveProfile sends the (possibly empty) display name to the server.
  // We don't validate length client-side beyond the maxlength=80 the
  // input already enforces; the server's normaliser is authoritative
  // and returns the post-trim value so we can reflect what it
  // actually stored.
  saveProfile(): void {
    this.profileBusy.set(true);
    this.profileError.set('');
    this.profileSavedAt.set(0);
    this.api.updateProfile(this.profileDisplayName).subscribe({
      next: (p) => {
        this.profileDisplayName = p.display_name ?? '';
        this.profileSavedAt.set(Date.now());
        this.profileBusy.set(false);
        setTimeout(() => this.profileSavedAt.set(0), 5_000);
      },
      error: (err) => {
        this.profileError.set(err.error?.error || 'Could not save the profile.');
        this.profileBusy.set(false);
      },
    });
  }

  clearSieve(): void {
    this.sieveBusy.set(true);
    this.sieveError.set('');
    this.api.deleteSieve().subscribe({
      next: () => {
        this.sieveSource = '';
        this.sieveSavedAt.set(Date.now());
        this.sieveBusy.set(false);
      },
      error: () => {
        this.sieveError.set('Could not clear the script.');
        this.sieveBusy.set(false);
      },
    });
  }
}

// escapeSieve escapes the two characters that have meaning inside a
// Sieve quoted-string: backslash and double-quote. RFC 5228 §2.4.2.1.
// Anything else (including non-ASCII) is fine as raw bytes.
function escapeSieve(s: string): string {
  return s.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

// truncate returns at most n chars, with an ellipsis when clipped.
// Used only for human-readable comments in the generated Sieve, so
// the rule-list header reads "Rule: From contains alice…" instead
// of the full pasted value if it's a paragraph.
function truncate(s: string, n: number): string {
  return s.length <= n ? s : s.slice(0, n - 1) + '…';
}
