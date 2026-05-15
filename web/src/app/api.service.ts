import { Injectable, inject, signal } from '@angular/core';
import {
  HttpClient,
  HttpErrorResponse,
  HttpInterceptorFn,
} from '@angular/common/http';
import { Router } from '@angular/router';
import { catchError, throwError } from 'rxjs';

import {
  LoginResult,
  Mailbox,
  MessageDetail,
  MessageSummary,
  SendResult,
} from './models';

const TOKEN_KEY = 'oximail.token';
const ADDRESS_KEY = 'oximail.address';

// ApiService is the client for the webmail HTTP+JSON API. It also holds
// the bearer-token session, persisted to localStorage so a page reload
// stays logged in.
@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  readonly token = signal<string>(localStorage.getItem(TOKEN_KEY) ?? '');
  readonly address = signal<string>(localStorage.getItem(ADDRESS_KEY) ?? '');

  get authenticated(): boolean {
    return this.token() !== '';
  }

  setSession(token: string, address: string): void {
    this.token.set(token);
    this.address.set(address);
    localStorage.setItem(TOKEN_KEY, token);
    localStorage.setItem(ADDRESS_KEY, address);
  }

  clearSession(): void {
    this.token.set('');
    this.address.set('');
    localStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(ADDRESS_KEY);
  }

  login(address: string, password: string) {
    return this.http.post<LoginResult>('/api/login', { address, password });
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

  // attachment fetches an attachment as a Blob — the bearer-token auth
  // interceptor handles authorization; the caller turns it into an
  // object URL or a download.
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

// authInterceptor attaches the bearer token to every request and, on a
// 401, clears the session and bounces to the login page.
export const authInterceptor: HttpInterceptorFn = (req, next) => {
  const api = inject(ApiService);
  const router = inject(Router);

  const token = api.token();
  const authed = token
    ? req.clone({ setHeaders: { Authorization: `Bearer ${token}` } })
    : req;

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
