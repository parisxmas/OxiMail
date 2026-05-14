import { Routes } from '@angular/router';

import { authGuard } from './auth.guard';

export const routes: Routes = [
  {
    path: 'login',
    loadComponent: () =>
      import('./login/login.component').then((m) => m.LoginComponent),
  },
  {
    path: 'mail',
    loadComponent: () =>
      import('./mailbox/mailbox.component').then((m) => m.MailboxComponent),
    canActivate: [authGuard],
  },
  { path: '', pathMatch: 'full', redirectTo: 'mail' },
  { path: '**', redirectTo: 'mail' },
];
