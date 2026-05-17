import { ApplicationConfig } from '@angular/core';
import { provideRouter } from '@angular/router';
import { provideHttpClient, withInterceptors } from '@angular/common/http';
import { provideQuillConfig } from 'ngx-quill';

import { routes } from './app.routes';
import { authInterceptor } from './api.service';

export const appConfig: ApplicationConfig = {
  providers: [
    provideRouter(routes),
    provideHttpClient(withInterceptors([authInterceptor])),
    // Default Quill toolbar shared by every editor instance. The
    // module list roughly matches what Gmail's web composer exposes:
    // basic inline formatting, colour, lists, links, headings,
    // blockquote, code, and a remove-formatting button. Components
    // can still pass an [config] override if they need something
    // different.
    provideQuillConfig({
      modules: {
        toolbar: [
          [{ header: [1, 2, 3, false] }],
          ['bold', 'italic', 'underline', 'strike'],
          [{ color: [] }, { background: [] }],
          [{ list: 'ordered' }, { list: 'bullet' }],
          [{ indent: '-1' }, { indent: '+1' }],
          ['blockquote', 'code-block'],
          ['link'],
          ['clean'],
        ],
      },
      placeholder: 'Write your message…',
    }),
  ],
};
