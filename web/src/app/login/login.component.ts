import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';

import { ApiService } from '../api.service';

@Component({
  selector: 'oximail-login',
  imports: [FormsModule],
  template: `
    <div class="wrap">
      <form class="card" (ngSubmit)="submit()">
        <h1>OxiMail</h1>
        <p class="sub">Sign in to your mailbox</p>

        <label>
          Email address
          <input
            name="address"
            type="email"
            autocomplete="username"
            [(ngModel)]="address"
            required
          />
        </label>

        <label>
          Password
          <input
            name="password"
            type="password"
            autocomplete="current-password"
            [(ngModel)]="password"
            required
          />
        </label>

        @if (error()) {
          <p class="error">{{ error() }}</p>
        }

        <button class="primary" type="submit" [disabled]="busy()">
          {{ busy() ? 'Signing in…' : 'Sign in' }}
        </button>
      </form>
    </div>
  `,
  styles: `
    .wrap {
      display: grid;
      place-items: center;
      height: 100%;
      background: var(--bg-muted);
    }
    .card {
      display: flex;
      flex-direction: column;
      gap: 14px;
      width: 320px;
      padding: 28px;
      background: var(--bg);
      border: 1px solid var(--border);
      border-radius: 10px;
    }
    h1 {
      margin: 0;
      font-size: 22px;
    }
    .sub {
      margin: -8px 0 4px;
      color: var(--text-muted);
    }
    label {
      display: flex;
      flex-direction: column;
      gap: 4px;
      font-size: 13px;
      color: var(--text-muted);
    }
    .error {
      margin: 0;
      color: var(--danger);
      font-size: 13px;
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
      next: (res) => {
        this.api.setSession(res.token, res.address);
        void this.router.navigate(['/mail']);
      },
      error: () => {
        this.error.set('Invalid email address or password.');
        this.busy.set(false);
      },
    });
  }
}
