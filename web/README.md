# OxiMail webmail frontend

An Angular 21 single-page app over OxiMail's webmail HTTP+JSON API
(`internal/webmail`). Standalone components, signals, the new control
flow, functional guards and interceptors — no NgModules.

## Build

```sh
cd web
npm install
npm run build      # -> dist/oximail-webmail/browser/
```

Then point the server at the build output and it serves the SPA
alongside the API:

```sh
OXIMAIL_WEBMAIL_STATIC=web/dist/oximail-webmail/browser ./oximail
```

## Develop

```sh
npm start          # ng serve on :4200, proxying /api to :8080
```

`proxy.conf.json` forwards `/api` to a locally-running `oximail`
(default webmail port `:8080`), so the dev server and the real API
share an origin.

## Layout

```
src/
  index.html              host page
  main.ts                 bootstrapApplication
  styles.css              global styles
  app/
    app.config.ts         providers — router, HttpClient + interceptor
    app.routes.ts         routes (lazy-loaded components)
    app.component.ts      root — <router-outlet>
    models.ts             TypeScript types mirroring the JSON API
    api.service.ts        API client + bearer-token session + auth interceptor
    auth.guard.ts         route guard — redirects to /login when unauthenticated
    login/                the login form
    mailbox/              the three-pane mail view
    compose/              the compose form
```
