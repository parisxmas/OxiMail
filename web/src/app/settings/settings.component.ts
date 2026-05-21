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
    :host { display: block; height: 100%; overflow-y: auto; background: var(--bg-muted); }
    .page { max-width: 720px; margin: 0 auto; padding: 24px 16px 64px; }
    header { display: flex; align-items: baseline; justify-content: space-between; }
    h1 { margin: 0; font-size: 20px; }
    .back { color: var(--text-muted); text-decoration: none; }
    .tabs { display: flex; gap: 8px; margin: 16px 0; }
    .tab { padding: 6px 12px; border: 1px solid var(--border); background: var(--bg); border-radius: 6px; }
    .tab.active { background: var(--bg-sunken); font-weight: 600; }
    .card {
      display: flex; flex-direction: column; gap: 12px;
      padding: 20px; background: var(--bg); border: 1px solid var(--border); border-radius: 10px;
    }
    .card h2 { margin: 0; font-size: 16px; }
    .hint { margin: 0; color: var(--text-muted); font-size: 13px; }
    .row { display: flex; align-items: center; gap: 8px; }
    label { display: flex; flex-direction: column; gap: 4px; font-size: 13px; color: var(--text-muted); }
    textarea { resize: vertical; font-family: ui-monospace, monospace; font-size: 13px; }
    textarea.script { min-height: 240px; }
    .example { background: var(--bg-sunken); padding: 10px 12px; border-radius: 6px;
               font-size: 12px; white-space: pre-wrap; }
    .error { margin: 0; color: var(--danger); font-size: 13px; }
    .ok { margin: 0; color: var(--accent); font-size: 13px; }
    footer { display: flex; justify-content: flex-end; gap: 8px; }
    .danger { color: var(--danger); }
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
