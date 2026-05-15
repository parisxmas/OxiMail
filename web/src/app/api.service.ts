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
    let url = `/api/mailboxes/${encodeURIComponent(mailbox)}/messages`;
    if (query) {
      url += `?q=${encodeURIComponent(query)}`;
    }
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
  }) {
    return this.http.post<SendResult>('/api/messages', body);
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
