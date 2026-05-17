import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';

import { ApiService, SieveScript, VacationRule } from '../api.service';

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
      </div>

      @if (tab() === 'vacation') {
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

  readonly tab = signal<'vacation' | 'sieve'>('vacation');

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
