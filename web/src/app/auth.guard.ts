import { inject } from '@angular/core';
import { CanActivateFn, Router } from '@angular/router';

import { ApiService } from './api.service';

// authGuard keeps the mail view behind a session: an unauthenticated
// visitor is redirected to the login page.
export const authGuard: CanActivateFn = () => {
  const api = inject(ApiService);
  const router = inject(Router);
  return api.authenticated ? true : router.createUrlTree(['/login']);
};
