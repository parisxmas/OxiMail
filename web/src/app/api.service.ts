import { Injectable, inject, signal } from '@angular/core';
import {
  HttpClient,
  HttpErrorResponse,
  HttpInterceptorFn,
} from '@angular/common/http';
import { Router } from '@angular/router';
import { catchError, tap, throwError } from 'rxjs';

import {
  LoginResult,
  Mailbox,
  MessageDetail,
  MessageSummary,
  SendResult,
} from './models';

const ADDRESS_KEY = 'oximail.address';

// ApiService is the client for the webmail HTTP+JSON API.
//
// Auth lives in two cookies set by the server on /api/login:
//
//   - oximail_session (HttpOnly): the bearer credential. JavaScript can
//     neither read nor forge it, which kills XSS-driven session theft.
//   - oximail_csrf (readable): the SPA echoes its value in
//     X-CSRF-Token on every mutating request, the server validates the
//     match. SameSite=Strict on both cookies blocks cross-origin
//     replay.
//
// We only persist the user's email address (so the UI can render it on
// reload before the first network round-trip); presence of an address
// is treated as "probably logged in" — the first failing API call will
// disabuse us and redirect to /login.
@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  readonly address = signal<string>(localStorage.getItem(ADDRESS_KEY) ?? '');

  get authenticated(): boolean {
    return this.address() !== '';
  }

  private setAddress(address: string): void {
    this.address.set(address);
    localStorage.setItem(ADDRESS_KEY, address);
  }

  clearSession(): void {
    this.address.set('');
    localStorage.removeItem(ADDRESS_KEY);
  }

  login(address: string, password: string) {
    return this.http
      .post<LoginResult>('/api/login', { address, password })
      .pipe(tap((res) => this.setAddress(res.address)));
  }

  logout() {
    return this.http
      .post<void>('/api/logout', {})
      .pipe(tap(() => this.clearSession()));
  }

  mailboxes() {
    return this.http.get<Mailbox[]>('/api/mailboxes');
  }

  messages(mailbox: string, query = '') {
    // Always opt in to body snippets — the SPA renders them in the
    // list row, and the per-message body fetch cost is acceptable
    // for a personal mailbox. For larger mailboxes, the server caps
    // the listing via ?limit and snippets honour that cap too.
    const params = new URLSearchParams({ snippets: '1' });
    if (query) {
      params.set('q', query);
    }
    const url = `/api/mailboxes/${encodeURIComponent(mailbox)}/messages?${params}`;
    return this.http.get<MessageSummary[]>(url);
  }

  message(id: number) {
    return this.http.get<MessageDetail>(`/api/messages/${id}`);
  }

  // attachment fetches an attachment as a Blob; the caller turns it
  // into an object URL or a download.
  attachment(id: number, index: number) {
    return this.http.get(`/api/messages/${id}/attachments/${index}`, {
      responseType: 'blob',
    });
  }

  send(body: {
    to: string[];
    cc: string[];
    subject: string;
    text: string;
    html?: string;
    in_reply_to?: string;
    references?: string[];
  }) {
    return this.http.post<SendResult>('/api/messages', body);
  }

  // saveDraft files a compose payload into the Drafts folder. Passing
  // an id overwrites that draft so auto-save keeps a single entry
  // there instead of one per keystroke window.
  saveDraft(body: {
    id?: number;
    to: string[];
    cc: string[];
    subject: string;
    text: string;
    html?: string;
    in_reply_to?: string;
    references?: string[];
  }) {
    return this.http.post<MessageSummary>('/api/drafts', body);
  }

  setFlags(id: number, op: 'add' | 'remove' | 'set', flags: string[]) {
    return this.http.patch<MessageSummary>(`/api/messages/${id}/flags`, {
      op,
      flags,
    });
  }

  move(id: number, mailbox: string) {
    return this.http.post<MessageSummary>(`/api/messages/${id}/move`, {
      mailbox,
    });
  }

  remove(id: number) {
    return this.http.delete<void>(`/api/messages/${id}`);
  }

  // --- Settings: vacation + sieve ---

  getVacation() {
    return this.http.get<VacationRule>('/api/vacation');
  }
  putVacation(rule: VacationRule) {
    return this.http.put<VacationRule>('/api/vacation', rule);
  }
  deleteVacation() {
    return this.http.delete<void>('/api/vacation');
  }

  getSieve() {
    return this.http.get<SieveScript>('/api/sieve');
  }
  putSieve(source: string) {
    return this.http.put<SieveScript>('/api/sieve', { source });
  }
  deleteSieve() {
    return this.http.delete<void>('/api/sieve');
  }

  // changePassword rotates the caller's password. The server verifies
  // current_password, enforces the min-length rule, and revokes every
  // OTHER session for the account on success — the current session
  // stays valid (the user doesn't get bounced back to login).
  changePassword(currentPassword: string, newPassword: string) {
    return this.http.post<void>('/api/account/password', {
      current_password: currentPassword,
      new_password: newPassword,
    });
  }
}

// VacationRule mirrors internal/webmail.vacationResponse.
export interface VacationRule {
  enabled: boolean;
  subject: string;
  body: string;
  suppress_days?: number;
  updated_at?: string;
}

// SieveScript mirrors internal/webmail.sieveResponse.
export interface SieveScript {
  source: string;
  updated_at?: string;
}

// readCookie returns the value of cookie `name`, or '' if absent. It is
// only used to fetch the CSRF token, which the server marks non-
// HttpOnly precisely for this purpose.
function readCookie(name: string): string {
  const prefix = name + '=';
  for (const part of document.cookie.split('; ')) {
    if (part.startsWith(prefix)) {
      return decodeURIComponent(part.slice(prefix.length));
    }
  }
  return '';
}

// isMutatingMethod mirrors the server's CSRF gate: GET/HEAD/OPTIONS
// are safe, everything else needs the X-CSRF-Token header.
function isMutatingMethod(method: string): boolean {
  const m = method.toUpperCase();
  return m !== 'GET' && m !== 'HEAD' && m !== 'OPTIONS';
}

// authInterceptor turns every API request into a cookie-authenticated
// one (withCredentials: true) and, for mutating requests, attaches the
// double-submit CSRF token. On a 401 it clears local session state and
// bounces to the login page.
export const authInterceptor: HttpInterceptorFn = (req, next) => {
  const api = inject(ApiService);
  const router = inject(Router);

  const headers: Record<string, string> = {};
  if (isMutatingMethod(req.method)) {
    const csrf = readCookie('oximail_csrf');
    if (csrf) {
      headers['X-CSRF-Token'] = csrf;
    }
  }
  const authed = req.clone({
    withCredentials: true,
    setHeaders: headers,
  });

  return next(authed).pipe(
    catchError((err: HttpErrorResponse) => {
      if (err.status === 401 && !req.url.endsWith('/api/login')) {
        api.clearSession();
        void router.navigate(['/login']);
      }
      return throwError(() => err);
    }),
  );
};
