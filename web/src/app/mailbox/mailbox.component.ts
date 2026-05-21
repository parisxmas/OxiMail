import { Component, HostListener, OnDestroy, OnInit, computed, inject, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { forkJoin } from 'rxjs';
import { Router, RouterLink } from '@angular/router';
import {
  LucideAngularModule,
  Archive,
  ChevronLeft,
  Edit3,
  Folder,
  Forward,
  Inbox,
  LogOut,
  Mail,
  MailOpen,
  Reply,
  ReplyAll,
  Search,
  Send,
  Settings,
  ShieldAlert,
  Star,
  Trash2,
} from 'lucide-angular';

import { ApiService } from '../api.service';
import { ComposeComponent, ComposeSeed } from '../compose/compose.component';
import {
  FLAG_FLAGGED,
  FLAG_SEEN,
  Mailbox,
  MessageDetail,
  MessageSummary,
  Thread,
} from '../models';

// Auto-refresh cadence for the open mailbox + folder counts. 10s strikes
// a balance between feeling live and the load it puts on the server
// (one /mailboxes + one /messages call per tab per cadence). A future
// upgrade would push these from the server via SSE/WebSocket keyed off
// the IMAP tracker's mailbox-update events instead of polling.
const REFRESH_INTERVAL_MS = 10_000;

// Pane-width constraints + defaults for the resizable splitter between
// folders | list | reader. The reader pane takes whatever's left
// (1fr); only the first two panes are explicit-width. Persisted to
// localStorage under STORAGE_KEY_* so the layout survives reload.
const FOLDERS_MIN = 160;
const FOLDERS_MAX = 360;
const FOLDERS_DEFAULT = 200;
// Vertical bounds for the list pane (top half of the stacked
// reading-pane layout). MIN keeps at least a few rows visible; MAX
// is generous — the reader minimum will kick in on the other side.
const LIST_HEIGHT_MIN = 120;
const LIST_HEIGHT_MAX = 1200;
const LIST_HEIGHT_DEFAULT = 380;
const STORAGE_KEY_FOLDERS = 'oximail.foldersWidth';
// The horizontal list-width key from the pre-stacked layout. Kept
// only because the value range happens to overlap "reasonable
// list-pane height in px" — loadWidth's clamp will accept a
// previously-stored 360 and treat it as a height. New users save
// under STORAGE_KEY_LIST_HEIGHT.
const STORAGE_KEY_LIST_HEIGHT = 'oximail.listHeight';

// Undo-toast lifetime. 6s matches Gmail's snackbar and is long enough
// to react to a misclick without parking visual noise on screen.
const UNDO_TIMEOUT_MS = 6_000;

// One pending undoable move at a time, mirrored in the toast.
interface UndoState {
  messageId: number;
  sourceFolder: string;
  label: string;
  timer: ReturnType<typeof setTimeout>;
}

@Component({
  selector: 'oximail-mailbox',
  imports: [DatePipe, ComposeComponent, RouterLink, LucideAngularModule],
  template: `
    <div
      class="app"
      [attr.data-view]="view()"
      [style.--folders-width.px]="foldersWidth()"
      [style.--list-height.px]="listHeight()"
    >
      <!-- Folder sidebar -->
      <aside class="folders">
        <div class="me" [title]="api.address()">{{ api.address() }}</div>
        <button class="compose-btn primary icon-text" (click)="openCompose()">
          <i-lucide [img]="icons.Edit3" [size]="16"></i-lucide>
          Compose
        </button>
        @for (mb of mailboxes(); track mb.name) {
          <div class="folder-row">
            <button
              class="folder icon-text"
              [class.active]="mb.name === selected()"
              [class.has-unread]="mb.unseen > 0"
              (click)="selectMailbox(mb.name)"
            >
              <i-lucide [img]="folderIcon(mb.name)" [size]="16"></i-lucide>
              <span class="folder-name">{{ mb.name }}</span>
              @if (mb.unseen > 0) {
                <span class="badge">{{ mb.unseen }}</span>
              }
            </button>
            @if (!mb.is_system) {
              <!-- Hover-only rename/delete cluster on user-created
                   folders. System folders (INBOX/Sent/…) intentionally
                   have no controls — the server refuses anyway, this
                   just keeps the UI honest. -->
              <span class="folder-actions">
                <button
                  type="button"
                  class="icon-btn"
                  (click)="promptRenameFolder(mb.name)"
                  title="Rename folder"
                  aria-label="Rename folder"
                >
                  <i-lucide [img]="icons.Edit3" [size]="14"></i-lucide>
                </button>
                <button
                  type="button"
                  class="icon-btn danger"
                  (click)="promptDeleteFolder(mb.name, mb.total)"
                  title="Delete folder"
                  aria-label="Delete folder"
                >
                  <i-lucide [img]="icons.Trash2" [size]="14"></i-lucide>
                </button>
              </span>
            }
          </div>
        }
        <button class="new-folder icon-text" type="button" (click)="promptNewFolder()">
          <i-lucide [img]="icons.Folder" [size]="16"></i-lucide>
          New folder
        </button>
        <a class="settings icon-text" routerLink="/settings">
          <i-lucide [img]="icons.Settings" [size]="16"></i-lucide>
          Settings
        </a>
        <button class="logout icon-text" (click)="logout()">
          <i-lucide [img]="icons.LogOut" [size]="16"></i-lucide>
          Sign out
        </button>
      </aside>

      <!-- Divider: folders | main. Drag to resize. Double-click resets. -->
      <div
        class="divider"
        role="separator"
        aria-label="Resize folders pane"
        (mousedown)="startResize($event, 'folders')"
        (dblclick)="resetWidth('folders')"
      ></div>

      <!-- Main column: list on top, reader stacked below it
           (Outlook-style "reading pane: bottom"). Both visible at
           the same time; clicking a row fills the reader without
           losing the list above. -->
      <div class="main">

      <!-- Message list -->
      <section class="list">
        <header>
          <button
            class="back mobile-only icon-btn"
            type="button"
            (click)="view.set('folders')"
            aria-label="Back to folders"
          >
            <i-lucide [img]="icons.ChevronLeft" [size]="20"></i-lucide>
          </button>
          <span class="title icon-text">
            <i-lucide [img]="folderIcon(selected())" [size]="16"></i-lucide>
            {{ selected() }}
          </span>
          <div class="search-wrap">
            <i-lucide class="search-icon" [img]="icons.Search" [size]="14"></i-lucide>
            <input
              class="search"
              type="search"
              placeholder="Search… (try from:alice, subject:foo, has:attachment, after:2024-01-01)"
              [value]="query()"
              (input)="onSearchInput($event)"
              [attr.aria-label]="'Search messages. Supports from:, to:, subject:, has:attachment, before:, after:'"
            />
          </div>
        </header>
        @if (loadingList()) {
          <p class="hint">Loading…</p>
        } @else if (threads().length === 0) {
          <p class="hint">No messages.</p>
        } @else {
          @for (t of threads(); track t.key) {
            <div
              class="row"
              [class.unread]="t.unreadCount > 0"
              [class.active]="threadHasOpen(t)"
              (click)="open(t.messages[0].id)"
              role="button"
              tabindex="0"
              (keydown.enter)="open(t.messages[0].id)"
            >
              <span class="avatar" [style.background]="avatarColor(t.messages[0].from)">{{ initials(t.messages[0].from) }}</span>
              <div class="row-main">
                <div class="row-top">
                  <span class="row-from">
                    {{ threadSenders(t) }}
                    @if (t.messages.length > 1) {
                      <span class="thread-count">({{ t.messages.length }})</span>
                    }
                  </span>
                  <span class="row-date">{{ t.messages[0].date | date: 'shortDate' }}</span>
                </div>
                <div class="row-subject">{{ t.subject || '(no subject)' }}</div>
                @if (t.messages[0].snippet) {
                  <div class="row-snippet">{{ t.messages[0].snippet }}</div>
                }
              </div>
              <!-- Right-edge cluster: always-visible star + hover-only
                   action strip share one grid column so the layout
                   doesn't shift between hover states. -->
              <div class="row-right">
                <!-- Star: always visible (gmail-style). Clicking
                     toggles \\Flagged on the thread's newest message;
                     stopPropagation keeps the row from opening
                     underneath. -->
                <button
                  type="button"
                  class="row-star icon-btn"
                  [class.flagged]="isFlagged(t.messages[0])"
                  (click)="$event.stopPropagation(); toggleFlagged(t.messages[0])"
                  [title]="isFlagged(t.messages[0]) ? 'Unstar' : 'Star'"
                  [attr.aria-label]="isFlagged(t.messages[0]) ? 'Unstar' : 'Star'"
                >
                  <i-lucide [img]="icons.Star" [size]="16"></i-lucide>
                </button>
              <!-- Hover-only action strip. Stop click propagation so the
                   button doesn't also open the thread underneath. The
                   actions apply to the thread's newest message (the one
                   the user sees in the row); a multi-message thread thus
                   archives/trashes one mail at a time, like gmail's
                   per-row hover actions. -->
              <div class="row-actions" (click)="$event.stopPropagation()">
                <button
                  type="button"
                  class="icon-btn"
                  (click)="archive(t.messages[0])"
                  title="Archive"
                  aria-label="Archive"
                >
                  <i-lucide [img]="icons.Archive" [size]="16"></i-lucide>
                </button>
                <button
                  type="button"
                  class="icon-btn"
                  (click)="deleteOrTrash(t.messages[0])"
                  title="Delete"
                  aria-label="Delete"
                >
                  <i-lucide [img]="icons.Trash2" [size]="16"></i-lucide>
                </button>
                @if (!t.messages[0].seen) {
                  <!-- Mark-unread only makes sense when the row is read
                       (well, currently flagged unread). Hide otherwise
                       to avoid a no-op click. -->
                } @else {
                  <button
                    type="button"
                    class="icon-btn"
                    (click)="markUnread(t.messages[0])"
                    title="Mark unread"
                    aria-label="Mark unread"
                  >
                    <i-lucide [img]="icons.MailOpen" [size]="16"></i-lucide>
                  </button>
                }
              </div>
              </div>
            </div>
          }
        }
      </section>

      <!-- Horizontal divider between list (top) and reader (bottom).
           Only present at desktop widths; the narrow-breakpoint media
           query stacks list and reader as full-screen alternatives
           via the 'view' signal instead, so this gutter is hidden. -->
      <div
        class="hdivider"
        role="separator"
        aria-orientation="horizontal"
        aria-label="Resize message list / reader split"
        (mousedown)="startResize($event, 'split')"
        (dblclick)="resetWidth('split')"
      ></div>

      <!-- Reader. Bottom row of the main column. Always visible at
           desktop widths (empty-state hint when nothing's selected);
           on narrow screens, view='reader' toggles to full-screen. -->
      <section class="reader">
        @if (openMessage(); as msg) {
          <!-- Conversation header: subject + thread length hint -->
          <div class="conversation-head">
            <button
              class="back mobile-only icon-btn"
              type="button"
              (click)="view.set('list')"
              aria-label="Back to list"
            >
              <i-lucide [img]="icons.ChevronLeft" [size]="20"></i-lucide>
            </button>
            <h2>{{ msg.subject || '(no subject)' }}</h2>
            @if (openThreadDetails().length > 1) {
              <span class="conversation-count">{{ openThreadDetails().length }} messages</span>
            }
          </div>

          <!-- Stack of message cards. Single-message threads degrade
               to a one-card stack — same content as the pre-thread
               reader, just wrapped in <article>. -->
          <div class="conversation-stack">
            @for (cm of openThreadDetails(); track cm.id) {
              <article
                class="conv-card"
                [class.expanded]="isExpanded(cm.id)"
                [class.unread]="!cm.seen"
              >
                <!-- Card header — clickable to expand/collapse. Stops
                     propagation on inner action buttons so they don't
                     also toggle the card under the cursor. -->
                <header class="conv-card-head" (click)="toggleExpand(cm)">
                  <span class="avatar" [style.background]="avatarColor(cm.from)">{{ initials(cm.from) }}</span>
                  <div class="conv-card-meta">
                    <div class="conv-card-from">{{ cm.from }}</div>
                    @if (isExpanded(cm.id)) {
                      @if (cm.to?.length) {
                        <div class="meta-line">to {{ cm.to.join(', ') }}</div>
                      }
                      @if (cm.cc?.length) {
                        <div class="meta-line">cc {{ cm.cc.join(', ') }}</div>
                      }
                      <div class="meta-line date">{{ cm.date | date: 'medium' }}</div>
                    } @else if (cm.snippet) {
                      <div class="conv-card-snippet">{{ cm.snippet }}</div>
                    }
                  </div>
                  <span class="conv-card-date">{{ cm.date | date: 'shortDate' }}</span>
                  <!-- Always-visible star on each card — gmail does
                       this. stopPropagation stops the header's
                       toggle-expand from also firing. -->
                  <button
                    type="button"
                    class="icon-btn"
                    [class.flagged]="isFlagged(cm)"
                    (click)="$event.stopPropagation(); toggleFlagged(cm)"
                    [title]="isFlagged(cm) ? 'Unflag' : 'Flag'"
                    [attr.aria-label]="isFlagged(cm) ? 'Unflag' : 'Flag'"
                  >
                    <i-lucide [img]="icons.Star" [size]="18"></i-lucide>
                  </button>
                  @if (isExpanded(cm.id)) {
                    <button class="icon-btn" type="button" (click)="$event.stopPropagation(); reply(cm, false)" title="Reply" aria-label="Reply">
                      <i-lucide [img]="icons.Reply" [size]="18"></i-lucide>
                    </button>
                    <button class="icon-btn" type="button" (click)="$event.stopPropagation(); reply(cm, true)" title="Reply all" aria-label="Reply all">
                      <i-lucide [img]="icons.ReplyAll" [size]="18"></i-lucide>
                    </button>
                    <button class="icon-btn" type="button" (click)="$event.stopPropagation(); forward(cm)" title="Forward" aria-label="Forward">
                      <i-lucide [img]="icons.Forward" [size]="18"></i-lucide>
                    </button>
                    <button class="icon-btn" type="button" (click)="$event.stopPropagation(); markUnread(cm)" title="Mark unread" aria-label="Mark unread">
                      <i-lucide [img]="icons.MailOpen" [size]="18"></i-lucide>
                    </button>
                    <button class="icon-btn" type="button" (click)="$event.stopPropagation(); archive(cm)" title="Archive" aria-label="Archive">
                      <i-lucide [img]="icons.Archive" [size]="18"></i-lucide>
                    </button>
                    <button
                      class="icon-btn danger"
                      type="button"
                      (click)="$event.stopPropagation(); deleteOrTrash(cm)"
                      [title]="inTrash() ? 'Delete permanently' : 'Move to Trash'"
                      [attr.aria-label]="inTrash() ? 'Delete permanently' : 'Move to Trash'"
                    >
                      <i-lucide [img]="icons.Trash2" [size]="18"></i-lucide>
                    </button>
                  }
                </header>

                @if (isExpanded(cm.id)) {
                  <div class="conv-card-body">
                    @if (cm.html) {
                      <!-- Angular sanitizes [innerHTML]; a production
                           client should render email HTML in a sandboxed
                           iframe with a strict CSP. -->
                      <div class="html-body" [innerHTML]="cm.html"></div>
                    } @else if (cm.text) {
                      <pre class="text-body">{{ cm.text }}</pre>
                    } @else {
                      <p class="hint">(empty message)</p>
                    }
                  </div>
                  @if (cm.attachments.length) {
                    <div class="attachments">
                      <strong>Attachments</strong>
                      @for (a of cm.attachments; track $index) {
                        <button
                          type="button"
                          class="chip"
                          (click)="download(cm, $index, a.filename)"
                          [disabled]="downloading() === $index"
                        >
                          {{ a.filename || '(unnamed)' }} · {{ a.size }} bytes
                        </button>
                      }
                    </div>
                  }
                }
              </article>
            }
          </div>
        } @else {
          <p class="hint center">Select a message to read it.</p>
        }
      </section>
      </div>
    </div>

    @if (composing()) {
      <oximail-compose
        [seed]="composeSeed()"
        (close)="closeCompose()"
        (sent)="onSent()"
      />
    }

    <!-- Undo toast — bottom-left. Auto-dismisses after UNDO_TIMEOUT_MS;
         clicking Undo moves the message back to where it came from. -->
    @if (undoState(); as u) {
      <div class="undo-toast" role="status" aria-live="polite">
        <span class="undo-label">{{ u.label }}</span>
        <button type="button" class="undo-action" (click)="undo()">Undo</button>
        <button
          type="button"
          class="undo-dismiss"
          (click)="dismissUndo()"
          aria-label="Dismiss"
          title="Dismiss"
        >✕</button>
      </div>
    }
  `,
  styles: `
    .app {
      display: grid;
      /* Two columns at desktop widths: folders sidebar | main. The
         main column is itself a vertical stack of [list, divider,
         reader] — Outlook-style "reading pane: bottom". */
      grid-template-columns:
        var(--folders-width, 200px)
        6px
        1fr;
      height: 100%;
    }
    /* Main column: list on top, hdivider, reader below. Three-row
       grid — first row's height comes from the --list-height CSS
       variable bound to the listHeight signal so a drag on the
       hdivider updates it live. The reader takes whatever's left
       (1fr). min-height:0 on each pane is what keeps the children's
       intrinsic content height from blowing out the grid; without
       it the whole column would scroll instead of each pane
       independently. */
    .main {
      display: grid;
      grid-template-rows: var(--list-height, 380px) 6px 1fr;
      min-width: 0;
      min-height: 0;
    }
    .main .list,
    .main .reader {
      min-height: 0;
    }
    /* Gutter between list and reader. The transparent-until-hover
       design from the folders divider was invisible here — users
       reported they couldn't find the drop zone. Give it a visible
       neutral line at rest (matches the rest of the chrome) and
       use ::before to extend the pointer hit area without
       widening the visual gutter. Three states stack cleanly:
         rest    — thin grey line
         hover   — accent-tinted band
         active  — accent-tinted band, slightly stronger */
    .hdivider {
      position: relative;
      height: 6px;
      background: var(--border);
      cursor: row-resize;
      transition: background 120ms ease;
    }
    .hdivider::before {
      /* 12px-tall hit pad centred on the visible line. Users with
         imprecise mice and trackpads can grab anywhere in that
         band; the visual gutter stays a calm 6px. */
      content: '';
      position: absolute;
      left: 0;
      right: 0;
      top: -4px;
      bottom: -4px;
    }
    .hdivider:hover,
    .hdivider:active {
      background: var(--accent);
    }
    .hdivider:hover { opacity: 0.5; }
    .hdivider:active { opacity: 0.8; }
    /* Same visible-at-rest treatment for the folders divider so
       the two axes feel consistent. Without this the user only
       discovers the sidebar resize by accident. */
    .divider {
      background: var(--border);
    }
    .divider::before {
      content: '';
      position: absolute;
      top: 0;
      bottom: 0;
      left: -4px;
      right: -4px;
    }
    .divider { position: relative; }
    /* The drag handle itself. 6px wide, transparent until hover/active
       so it reads as a thin gutter at rest. col-resize cursor advertises
       the affordance. */
    .divider {
      background: transparent;
      cursor: col-resize;
      user-select: none;
      transition: background 120ms ease;
    }
    .divider:hover,
    .divider:active {
      background: var(--accent);
      opacity: 0.4;
    }
    .mobile-only { display: none; }
    /* Below ~720px we collapse to a single column and show only the
       column that matches the current view signal. Back buttons in
       the list header and reader header navigate between them. The
       dividers are hidden — at that width there's nothing to resize. */
    @media (max-width: 720px) {
      .app {
        grid-template-columns: 1fr;
      }
      .app > * { display: none; }
      .divider, .hdivider { display: none !important; }
      .app[data-view='folders'] .folders { display: flex; }
      /* At narrow widths the stacked main column is awkward — show
         only ONE pane at a time and let the back buttons toggle. The
         desktop flex stack is overridden to a single-pane container
         here so list and reader can each fill the screen on their
         turn. */
      .app[data-view='list'] .main,
      .app[data-view='reader'] .main { display: flex; }
      .app[data-view='list'] .main .reader { display: none; }
      .app[data-view='reader'] .main .list { display: none; }
      .mobile-only.icon-btn {
        display: inline-flex;
        align-items: center;
        justify-content: center;
        width: 32px;
        height: 32px;
        background: transparent;
        border: none;
        color: var(--text-muted);
        margin-right: 4px;
      }
    }
    .folders,
    .list {
      border-right: 1px solid var(--border);
      overflow-y: auto;
    }
    .folders {
      display: flex;
      flex-direction: column;
      gap: 2px;
      padding: 14px 10px;
      background: var(--bg-muted);
    }
    .me {
      font-size: 12px;
      color: var(--text-muted);
      padding: 2px 6px 10px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .compose-btn {
      margin-bottom: 10px;
      padding: 9px 14px;
      font-weight: 600;
      border-radius: 8px;
      justify-content: center;
    }
    .icon-text {
      display: inline-flex;
      align-items: center;
      gap: 8px;
    }
    /* Folder rows: the folder button + a hover-only rename/delete
       cluster sit on a flex row so the actions tuck against the
       right edge without pushing the folder name. */
    .folder-row {
      display: flex;
      align-items: stretch;
      gap: 2px;
    }
    .folder {
      display: flex;
      align-items: center;
      gap: 8px;
      text-align: left;
      border: none;
      background: transparent;
      padding: 8px 10px;
      border-radius: 6px;
      color: var(--text);
      transition: background 100ms ease;
      cursor: pointer;
      flex: 1;
      min-width: 0;
    }
    .folder:hover {
      background: var(--bg-sunken);
    }
    .folder-actions {
      display: none;
      align-items: center;
      gap: 0;
    }
    .folder-row:hover .folder-actions,
    .folder-actions:focus-within {
      display: inline-flex;
    }
    .folder-actions .icon-btn {
      width: 26px;
      height: 26px;
    }
    /* New-folder button — same row geometry as the folder list, but
       muted so it reads as an affordance, not a navigation item. */
    .new-folder {
      display: flex;
      align-items: center;
      gap: 8px;
      text-align: left;
      border: none;
      background: transparent;
      padding: 8px 10px;
      border-radius: 6px;
      color: var(--text-muted);
      font-size: 13px;
      cursor: pointer;
      transition: background 100ms ease, color 100ms ease;
    }
    .new-folder:hover {
      background: var(--bg-sunken);
      color: var(--text);
    }
    .folder.active {
      background: var(--bg-sunken);
      font-weight: 600;
      color: var(--accent);
    }
    /* Folder with unread messages stays bold even when it's not the
       active folder — same affordance Gmail uses to draw the eye to
       inboxes that have new mail. */
    .folder.has-unread {
      font-weight: 600;
      color: var(--text);
    }
    .folder .folder-name {
      flex: 1;
    }
    .badge {
      background: var(--accent);
      color: var(--accent-text);
      border-radius: 10px;
      padding: 1px 8px;
      font-size: 11px;
      font-weight: 600;
    }
    .settings {
      margin-top: auto;
      color: var(--text-muted);
      text-decoration: none;
      padding: 8px 10px;
      font-size: 13px;
      border-radius: 6px;
      transition: background 100ms ease;
    }
    .settings:hover {
      background: var(--bg-sunken);
    }
    .logout {
      border: none;
      background: transparent;
      color: var(--text-muted);
      text-align: left;
      padding: 8px 10px;
      border-radius: 6px;
      transition: background 100ms ease;
      cursor: pointer;
    }
    .logout:hover {
      background: var(--bg-sunken);
    }
    .list header {
      position: sticky;
      top: 0;
      background: var(--bg);
      padding: 12px 14px;
      border-bottom: 1px solid var(--border);
      font-weight: 600;
      display: flex;
      align-items: center;
      gap: 10px;
      z-index: 1;
    }
    .list header .title {
      font-size: 14px;
      text-transform: capitalize;
    }
    .reader-head h2 {
      margin: 0 0 12px;
      font-size: 18px;
      font-weight: 600;
    }
    .search-wrap {
      position: relative;
      flex: 1;
      max-width: 220px;
    }
    .search-icon {
      position: absolute;
      left: 8px;
      top: 50%;
      transform: translateY(-50%);
      color: var(--text-muted);
      pointer-events: none;
    }
    .search {
      width: 100%;
      padding: 5px 8px 5px 26px;
      font-size: 12px;
      font-weight: normal;
      border-radius: 6px;
    }
    /* Message-list rows: avatar | sender+subject+snippet | date.
       Hover gives a subtle nudge; active is the selected message. */
    .row {
      display: grid;
      grid-template-columns: auto 1fr auto;
      gap: 10px;
      width: 100%;
      text-align: left;
      border: none;
      border-bottom: 1px solid var(--border);
      border-radius: 0;
      background: transparent;
      padding: 12px 14px;
      cursor: pointer;
      transition: background 80ms ease;
      position: relative;
    }
    .row:hover {
      background: var(--bg-muted);
    }
    /* Right-edge cluster: star (always shown) + action strip
       (hover-only). The wrapper gives both elements a single grid
       cell to share and a vertical center alignment. */
    .row-right {
      display: inline-flex;
      align-items: center;
      gap: 2px;
      align-self: center;
    }
    /* The star is always visible. Muted by default; the .flagged
       state lights it up yellow via the existing .icon-btn.flagged
       rule below (shared with the reader's star). */
    .row-star {
      width: 28px;
      height: 28px;
      color: var(--text-muted);
    }
    /* Hover-only quick actions on the right of each row. Hidden by
       default (display: none keeps them out of layout so the row
       doesn't reserve space and shift). Visible on row hover or
       when any child has keyboard focus. */
    .row-actions {
      display: none;
      align-items: center;
      gap: 2px;
    }
    .row:hover .row-actions,
    .row-actions:focus-within {
      display: inline-flex;
    }
    /* Slightly smaller icon buttons inside row actions so they
       don't visually compete with the avatar. */
    .row-actions .icon-btn {
      width: 28px;
      height: 28px;
    }
    .row.active {
      background: var(--bg-sunken);
    }
    .avatar {
      display: inline-grid;
      place-items: center;
      width: 36px;
      height: 36px;
      border-radius: 50%;
      color: white;
      font-size: 13px;
      font-weight: 600;
      flex-shrink: 0;
      user-select: none;
    }
    .avatar.large {
      width: 44px;
      height: 44px;
      font-size: 15px;
    }
    .row-main {
      min-width: 0;
    }
    .row-top {
      display: flex;
      justify-content: space-between;
      align-items: baseline;
      gap: 8px;
    }
    .row-from {
      color: var(--text);
      font-size: 13px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .row-date {
      color: var(--text-muted);
      font-size: 11px;
      flex-shrink: 0;
    }
    .row.unread .row-from,
    .row.unread .row-subject {
      color: var(--unread);
      font-weight: 600;
    }
    .row-subject {
      font-size: 13px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      margin-top: 2px;
    }
    .row-snippet {
      font-size: 12px;
      color: var(--text-muted);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      margin-top: 2px;
    }
    /* Inline count badge on a thread row, e.g. "Alice, Bob (3)". A
       small muted parenthetical that doesn't compete with the sender
       names for attention — gmail keeps the thread length subtle
       there. */
    .thread-count {
      color: var(--text-muted);
      font-weight: 400;
      font-size: 12px;
      margin-left: 4px;
    }
    /* Reader's thread navigation strip — a vertical list of
       per-message rows shown above the message header when the open
       message lives in a thread of 2+. Each row is clickable and
       swaps the open message. The currently-open one is muted; an
       unread sibling renders in the same bold/accent style as an
       unread list row. */
    .thread-strip {
      display: flex;
      flex-direction: column;
      gap: 2px;
      padding: 8px 12px;
      border-bottom: 1px solid var(--border);
      background: var(--bg-sunken);
    }
    .thread-strip-head {
      font-size: 11px;
      color: var(--text-muted);
      text-transform: uppercase;
      letter-spacing: 0.04em;
      margin-bottom: 4px;
    }
    .thread-strip-row {
      display: flex;
      align-items: baseline;
      gap: 8px;
      padding: 4px 8px;
      border: none;
      background: transparent;
      color: var(--text-muted);
      border-radius: 4px;
      cursor: pointer;
      text-align: left;
      font: inherit;
    }
    .thread-strip-row:hover {
      background: var(--bg);
    }
    .thread-strip-row.active {
      background: var(--bg);
      color: var(--text);
    }
    .thread-strip-row.unread {
      color: var(--unread);
      font-weight: 600;
    }
    .thread-strip-from {
      flex-shrink: 0;
      font-size: 13px;
    }
    .thread-strip-snippet {
      flex: 1;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      font-size: 12px;
    }
    .thread-strip-date {
      flex-shrink: 0;
      font-size: 11px;
      color: var(--text-muted);
    }
    .reader {
      display: flex;
      flex-direction: column;
      overflow-y: auto;
    }
    /* Conversation view (stacked-card thread render).
       conversation-head holds the subject + length hint;
       conversation-stack is the vertical list of cards; each
       conv-card is one message, with conv-card-head always visible
       and conv-card-body shown only when expanded. The collapsed
       state mirrors gmail's one-line summary: avatar + sender +
       snippet + date + star. Click anywhere on the header expands. */
    .conversation-head {
      display: flex;
      align-items: center;
      gap: 12px;
      padding: 16px 20px;
      border-bottom: 1px solid var(--border);
    }
    .conversation-head h2 {
      margin: 0;
      font-size: 18px;
      font-weight: 500;
      flex: 1;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .conversation-count {
      font-size: 12px;
      color: var(--text-muted);
      flex-shrink: 0;
    }
    .conversation-stack {
      display: flex;
      flex-direction: column;
      gap: 8px;
      padding: 12px 16px 24px;
    }
    .conv-card {
      border: 1px solid var(--border);
      border-radius: 8px;
      background: var(--bg);
      overflow: hidden;
    }
    .conv-card.unread {
      border-color: var(--accent);
    }
    .conv-card-head {
      display: flex;
      gap: 10px;
      align-items: flex-start;
      padding: 10px 14px;
      cursor: pointer;
      transition: background 80ms ease;
    }
    .conv-card-head:hover {
      background: var(--bg-muted);
    }
    .conv-card.expanded .conv-card-head {
      border-bottom: 1px solid var(--border);
    }
    .conv-card-meta {
      flex: 1;
      min-width: 0;
      font-size: 13px;
      color: var(--text-muted);
    }
    .conv-card-from {
      color: var(--text);
      font-weight: 500;
      font-size: 14px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .conv-card.unread .conv-card-from {
      color: var(--unread);
      font-weight: 600;
    }
    .conv-card-snippet {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      margin-top: 2px;
    }
    .conv-card-date {
      flex-shrink: 0;
      font-size: 12px;
      color: var(--text-muted);
      align-self: flex-start;
    }
    .conv-card-body {
      padding: 12px 16px 16px;
    }
    .reader-head {
      padding: 18px 20px;
      border-bottom: 1px solid var(--border);
    }
    .reader-meta {
      display: flex;
      gap: 12px;
      align-items: flex-start;
      margin: 10px 0;
    }
    .meta {
      flex: 1;
      font-size: 13px;
      color: var(--text-muted);
    }
    .meta-from {
      color: var(--text);
      font-weight: 500;
      font-size: 14px;
    }
    .meta-line {
      margin-top: 2px;
    }
    .meta .date {
      margin-top: 4px;
      font-size: 12px;
    }
    /* Action toolbar — icon buttons with a subtle hover, divider
       between thread actions and message-state actions. */
    .actions {
      display: flex;
      gap: 2px;
      margin-top: 8px;
      align-items: center;
    }
    .icon-btn {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 34px;
      height: 34px;
      border: none;
      background: transparent;
      border-radius: 6px;
      color: var(--text-muted);
      cursor: pointer;
      transition: background 100ms ease, color 100ms ease;
    }
    .icon-btn:hover {
      background: var(--bg-sunken);
      color: var(--text);
    }
    .icon-btn.flagged {
      color: #f5a623;
    }
    .icon-btn.danger:hover {
      background: rgba(220, 53, 69, 0.1);
      color: var(--danger);
    }
    .divider {
      width: 1px;
      height: 20px;
      background: var(--border);
      margin: 0 6px;
    }
    .reader-body {
      padding: 16px;
      flex: 1;
    }
    .text-body {
      margin: 0;
      white-space: pre-wrap;
      word-wrap: break-word;
      font: inherit;
    }
    .attachments {
      padding: 12px 16px;
      border-top: 1px solid var(--border);
      display: flex;
      flex-wrap: wrap;
      gap: 8px;
      align-items: center;
      font-size: 13px;
    }
    .chip {
      background: var(--bg-sunken);
      border-radius: 6px;
      padding: 3px 8px;
    }
    .hint {
      color: var(--text-muted);
      padding: 16px;
    }
    .hint.center {
      display: grid;
      place-items: center;
      height: 100%;
    }
    /* Undo toast — floats bottom-left, above the page chrome. Slide-in
       animation cues that something just happened; clicking outside it
       doesn't dismiss (so users have the full timeout to react). */
    .undo-toast {
      position: fixed;
      left: 24px;
      bottom: 24px;
      z-index: 9;
      display: flex;
      align-items: center;
      gap: 12px;
      padding: 10px 12px 10px 16px;
      background: #2d3748;
      color: white;
      border-radius: 8px;
      box-shadow: 0 8px 24px rgba(0, 0, 0, 0.3);
      font-size: 13px;
      max-width: min(420px, calc(100vw - 48px));
      animation: undo-slide-in 160ms ease-out;
    }
    @keyframes undo-slide-in {
      from { transform: translateY(8px); opacity: 0; }
      to { transform: translateY(0); opacity: 1; }
    }
    .undo-label {
      flex: 1;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .undo-action {
      background: transparent;
      border: none;
      color: #93c5fd;
      font-weight: 600;
      padding: 4px 8px;
      border-radius: 4px;
      cursor: pointer;
      text-transform: uppercase;
      font-size: 12px;
      letter-spacing: 0.04em;
    }
    .undo-action:hover {
      background: rgba(255, 255, 255, 0.08);
    }
    .undo-dismiss {
      background: transparent;
      border: none;
      color: rgba(255, 255, 255, 0.6);
      width: 24px;
      height: 24px;
      border-radius: 4px;
      cursor: pointer;
      font-size: 14px;
      display: inline-flex;
      align-items: center;
      justify-content: center;
    }
    .undo-dismiss:hover {
      background: rgba(255, 255, 255, 0.08);
      color: white;
    }
  `,
})
export class MailboxComponent implements OnInit, OnDestroy {
  protected readonly api = inject(ApiService);
  private readonly router = inject(Router);

  // Icon set exposed to the template — one place to pin which icons
  // are imported above. Adding a new icon means adding it both to
  // the import list and to this map.
  protected readonly icons = {
    Archive, ChevronLeft, Edit3, Folder, Forward, Inbox, LogOut, MailOpen,
    Reply, ReplyAll, Search, Send, Settings, Star, Trash2,
  };

  // Pane widths for the resizable splitter. Restored from localStorage
  // on construction so a returning user gets their last layout back;
  // the template binds them to CSS variables on .app, which the grid
  // template-columns then consumes. See FOLDERS_DEFAULT / LIST_DEFAULT
  // and the related min/max constants up top.
  readonly foldersWidth = signal<number>(this.loadWidth(STORAGE_KEY_FOLDERS, FOLDERS_DEFAULT, FOLDERS_MIN, FOLDERS_MAX));
  // listHeight is the vertical size of the message-list pane in
  // the stacked reading-pane layout. The reader takes whatever
  // height is left. Drag the .hdivider to retune.
  readonly listHeight = signal<number>(this.loadWidth(STORAGE_KEY_LIST_HEIGHT, LIST_HEIGHT_DEFAULT, LIST_HEIGHT_MIN, LIST_HEIGHT_MAX));

  // Pending undo-able move (archive or trash). When non-null the
  // bottom toast is shown; cleared on Undo, on dismiss, or when the
  // UNDO_TIMEOUT_MS timer fires.
  readonly undoState = signal<UndoState | null>(null);

  // Active resize state — populated on mousedown over a divider, drives
  // the document-level mousemove/mouseup listeners. 'folders' is the
  // X-axis sidebar drag; 'split' is the Y-axis list/reader split in
  // the stacked main column.
  private resizeTarget: 'folders' | 'split' | null = null;
  private resizeStartCoord = 0;
  private resizeStartWidth = 0;
  private readonly onResizeMove = (e: MouseEvent) => this.resizeMove(e);
  private readonly onResizeEnd = () => this.resizeEnd();

  readonly mailboxes = signal<Mailbox[]>([]);
  readonly selected = signal<string>('INBOX');
  readonly messages = signal<MessageSummary[]>([]);
  readonly openMessage = signal<MessageDetail | null>(null);

  // Stacked-conversation state. When a row is opened we fetch every
  // message in its thread (parallel /api/messages/{id} GETs) so the
  // reader can render them as a vertical stack, gmail-style. The
  // array is sorted newest-first so the reader's scroll position
  // lines up with the list-view ordering. expandedIds tracks which
  // cards are open; we start with just the clicked message expanded
  // and let the user click headers to expand the rest.
  readonly openThreadDetails = signal<MessageDetail[]>([]);
  readonly expandedIds = signal<Set<number>>(new Set());

  // Threads derived from messages() — a computed signal so we never
  // store and risk drifting from the source list. Drafts and Sent
  // benefit from threading just as much as INBOX, so we don't
  // special-case the folder; if a folder happens to have one message
  // per subject, every thread has length 1 and rendering looks
  // identical to the pre-thread view.
  readonly threads = computed<Thread[]>(() => groupByThread(this.messages()));

  // The thread the currently-open message lives in, if any. Derived
  // from openMessage() + threads(). Used by the reader's thread
  // navigation strip; null when no message is open or the thread has
  // a single message (no nav needed).
  readonly openThread = computed<Thread | null>(() => {
    const m = this.openMessage();
    if (!m) return null;
    const t = this.threads().find((th) => th.messages.some((mm) => mm.id === m.id));
    return t && t.messages.length > 1 ? t : null;
  });
  readonly composing = signal(false);
  readonly composeSeed = signal<ComposeSeed | null>(null);
  // Mobile-only navigation state. On wide screens the CSS shows all
  // three columns regardless; on narrow screens the data-view attr
  // controls which one is visible.
  readonly view = signal<'folders' | 'list' | 'reader'>('list');
  readonly loadingList = signal(false);
  readonly query = signal<string>('');
  // index of the attachment currently being downloaded, or -1 for none.
  readonly downloading = signal<number>(-1);
  // search-input debounce timer; cleared on every keystroke.
  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  // Background poll that pulls fresh mailbox counts + list contents
  // every REFRESH_INTERVAL_MS so a user staring at the inbox sees a
  // new message without hitting reload. Cleared on destroy and paused
  // while the tab is hidden (no point keeping the connection warm to
  // a tab the user isn't looking at).
  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private readonly onVisibilityChange = () => {
    if (document.hidden) {
      this.stopPolling();
    } else {
      // Catch up immediately on tab refocus, then resume polling.
      this.quietRefresh();
      this.startPolling();
    }
  };

  ngOnInit(): void {
    this.refreshMailboxes();
    this.loadMessages();
    this.startPolling();
    document.addEventListener('visibilitychange', this.onVisibilityChange);
  }

  ngOnDestroy(): void {
    this.stopPolling();
    document.removeEventListener('visibilitychange', this.onVisibilityChange);
    // Guard against a mid-drag teardown — clear the document-level
    // resize listeners and reset the cursor, otherwise they leak.
    if (this.resizeTarget) {
      document.removeEventListener('mousemove', this.onResizeMove);
      document.removeEventListener('mouseup', this.onResizeEnd);
      document.body.style.cursor = '';
      this.resizeTarget = null;
    }
    // Cancel a pending undo timer — otherwise it'd fire against a
    // detached component and silently NPE.
    this.clearUndoTimer();
  }

  private startPolling(): void {
    if (this.pollTimer !== null || document.hidden) {
      return;
    }
    this.pollTimer = setInterval(() => this.quietRefresh(), REFRESH_INTERVAL_MS);
  }

  private stopPolling(): void {
    if (this.pollTimer !== null) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  // quietRefresh pulls fresh mailbox + message-list data without
  // toggling the loading spinner — the user is not waiting on this, so
  // the UI must not flicker. It also no-ops while a compose modal is
  // open so we don't yank the underlying list out from under it.
  private quietRefresh(): void {
    if (this.composing()) {
      return;
    }
    this.api.mailboxes().subscribe({
      next: (boxes) => this.mailboxes.set(boxes),
    });
    this.api.messages(this.selected(), this.query()).subscribe({
      next: (msgs) => this.messages.set(msgs),
    });
  }

  selectMailbox(name: string): void {
    if (name !== this.selected()) {
      this.selected.set(name);
      this.openMessage.set(null);
      this.query.set('');
      this.loadMessages();
    }
    this.view.set('list');
  }

  // onSearchInput debounces keystrokes by 250 ms before re-querying the
  // server — a body search costs a round-trip per match.
  onSearchInput(event: Event): void {
    const value = (event.target as HTMLInputElement).value;
    this.query.set(value);
    if (this.searchTimer !== null) {
      clearTimeout(this.searchTimer);
    }
    this.searchTimer = setTimeout(() => {
      this.searchTimer = null;
      this.loadMessages();
    }, 250);
  }

  // download fetches one attachment as a blob, then nudges the browser
  // to save it under the original filename.
  download(msg: MessageDetail, index: number, filename: string): void {
    if (this.downloading() === index) {
      return;
    }
    this.downloading.set(index);
    this.api.attachment(msg.id, index).subscribe({
      next: (blob: Blob) => {
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = filename || 'attachment';
        a.click();
        URL.revokeObjectURL(url);
        this.downloading.set(-1);
      },
      error: () => this.downloading.set(-1),
    });
  }

  // open does double duty: it loads the clicked message AND every
  // other message in its thread so the reader can render the full
  // conversation as a stack. Single-message threads degrade to a
  // one-element fetch with no behaviour change.
  //
  // Marking-as-read fires only for the clicked message (the one
  // initially expanded). Other messages get marked when the user
  // expands their card.
  open(id: number): void {
    // Pick the thread that contains this id. If we somehow miss
    // (e.g. the list refreshed and dropped the row mid-click), fall
    // back to a single-message fetch — safer than crashing.
    const thread = this.threads().find((t) => t.messages.some((m) => m.id === id));
    const ids = thread ? thread.messages.map((m) => m.id) : [id];
    // forkJoin over an empty array completes synchronously with no
    // emissions, so we only enter it when there's something to fetch.
    if (ids.length === 0) return;
    forkJoin(ids.map((mid) => this.api.message(mid))).subscribe({
      next: (details) => {
        details.sort((a, b) => +new Date(b.date) - +new Date(a.date));
        this.openThreadDetails.set(details);
        const primary = details.find((m) => m.id === id) ?? details[0] ?? null;
        this.openMessage.set(primary);
        this.expandedIds.set(new Set(primary ? [primary.id] : []));
        this.view.set('reader');
        // Mark only the clicked message as read — older cards stay
        // unread until the user expands them, like gmail.
        if (primary && !primary.seen) {
          this.api.setFlags(primary.id, 'add', [FLAG_SEEN]).subscribe({
            next: (updated) => this.applyUpdate(updated),
          });
        }
      },
    });
  }

  // toggleExpand flips a card's collapsed/expanded state. When
  // expanding a previously-unread message we also mark it read so a
  // user "skimming the conversation" doesn't have to do that
  // explicitly — matches the click-to-open contract elsewhere.
  protected toggleExpand(m: MessageDetail): void {
    const cur = this.expandedIds();
    const next = new Set(cur);
    const wasExpanded = next.has(m.id);
    if (wasExpanded) {
      next.delete(m.id);
    } else {
      next.add(m.id);
    }
    this.expandedIds.set(next);
    if (!wasExpanded && !m.seen) {
      this.api.setFlags(m.id, 'add', [FLAG_SEEN]).subscribe({
        next: (updated) => this.applyUpdate(updated),
      });
    }
  }

  // isExpanded is a tiny convenience for the template — Sets don't
  // have a clean Angular template syntax for membership tests.
  protected isExpanded(id: number): boolean {
    return this.expandedIds().has(id);
  }

  // isFlagged / toggleFlagged are called from both the reader (with a
  // MessageDetail) and the list rows + 's' shortcut (with a
  // MessageSummary). Both shapes have id+flags, which is all we need
  // — narrow the parameter type to the structural minimum so the same
  // method serves every caller.
  isFlagged(msg: { flags: string[] }): boolean {
    return msg.flags.includes(FLAG_FLAGGED);
  }

  toggleFlagged(msg: { id: number; flags: string[] }): void {
    const op = this.isFlagged(msg) ? 'remove' : 'add';
    this.api.setFlags(msg.id, op, [FLAG_FLAGGED]).subscribe({
      next: (updated) => this.applyUpdate(updated),
    });
  }

  // markUnread / archive / deleteOrTrash all only need {id, subject}
  // — both MessageSummary (list rows) and MessageDetail (the reader)
  // satisfy that shape, so we accept the narrow structural type and
  // let either caller through without a coerce step.
  markUnread(msg: { id: number; subject: string }): void {
    this.api.setFlags(msg.id, 'remove', [FLAG_SEEN]).subscribe({
      next: (updated) => this.applyUpdate(updated),
    });
  }

  // archive moves the message to the Archive folder. Distinct from
  // deleteOrTrash — the user explicitly wants to keep this message,
  // just out of the Inbox.
  archive(msg: { id: number; subject: string }): void {
    const sourceFolder = this.selected();
    this.api.move(msg.id, 'Archive').subscribe({
      next: () => {
        this.afterRemoval();
        this.showUndo(msg.id, sourceFolder, `Archived "${msg.subject || '(no subject)'}"`);
      },
    });
  }

  // deleteOrTrash matches Gmail's single-button delete UX: from any
  // folder it moves to Trash; from inside Trash itself it permanently
  // removes the message (after a confirm so it's not a foot-gun).
  deleteOrTrash(msg: { id: number; subject: string }): void {
    if (this.inTrash()) {
      if (!confirm('Delete this message permanently? This cannot be undone.')) {
        return;
      }
      this.api.remove(msg.id).subscribe({
        next: () => this.afterRemoval(),
      });
    } else {
      const sourceFolder = this.selected();
      this.api.move(msg.id, 'Trash').subscribe({
        next: () => {
          this.afterRemoval();
          this.showUndo(msg.id, sourceFolder, `Moved "${msg.subject || '(no subject)'}" to Trash`);
        },
      });
    }
  }

  // inTrash reports whether the currently selected mailbox IS the
  // Trash folder. Drives the dual-mode behaviour of the trash icon.
  protected inTrash(): boolean {
    return this.selected().toUpperCase() === 'TRASH';
  }

  // showUndo schedules an undo toast for a just-completed move. The
  // toast auto-dismisses after UNDO_TIMEOUT_MS; a fresh move replaces
  // any pending undo (only one operation is undoable at a time —
  // matches Gmail). After the message is moved BACK we refresh the
  // mailboxes + message list so the user sees it return immediately.
  private showUndo(messageId: number, sourceFolder: string, label: string): void {
    this.clearUndoTimer();
    const timer = setTimeout(() => this.undoState.set(null), UNDO_TIMEOUT_MS);
    this.undoState.set({ messageId, sourceFolder, label, timer });
  }

  protected undo(): void {
    const u = this.undoState();
    if (!u) return;
    this.clearUndoTimer();
    this.undoState.set(null);
    this.api.move(u.messageId, u.sourceFolder).subscribe({
      next: () => {
        this.refreshMailboxes();
        this.loadMessages();
      },
    });
  }

  protected dismissUndo(): void {
    this.clearUndoTimer();
    this.undoState.set(null);
  }

  private clearUndoTimer(): void {
    const u = this.undoState();
    if (u) clearTimeout(u.timer);
  }

  // reply opens the compose dialog pre-filled with a reply or
  // reply-all seed: To = original sender, Cc (reply-all) = original
  // To + Cc minus our own address, Subject with "Re: " prefix, body
  // is a quoted citation block, and the In-Reply-To / References
  // headers wire it into the thread.
  reply(msg: MessageDetail, all: boolean): void {
    let cc = '';
    if (all) {
      const me = this.api.address();
      const others = [...msg.to, ...msg.cc]
        .map(addressOnly)
        .filter((a) => a && a !== me);
      cc = Array.from(new Set(others)).join(', ');
    }
    this.composeSeed.set({
      to: addressOnly(msg.from),
      cc,
      subject: prefixedSubject(msg.subject, 'Re: '),
      text: quoteBody(msg),
      inReplyTo: msg.message_id,
      references: [msg.message_id],
    });
    this.composing.set(true);
  }

  // forward opens the compose dialog with the message inlined and the
  // recipient left blank.
  forward(msg: MessageDetail): void {
    this.composeSeed.set({
      to: '',
      subject: prefixedSubject(msg.subject, 'Fwd: '),
      text: forwardBody(msg),
    });
    this.composing.set(true);
  }

  // promptNewFolder / promptRenameFolder / promptDeleteFolder are
  // intentionally low-tech for now: they use the browser's native
  // prompt() / confirm() dialogs instead of a custom modal. The
  // backend already validates the name (length, control chars,
  // collision); the SPA passes the input through and surfaces server
  // errors as a window.alert. A proper inline-edit UI is the next
  // improvement here, but native dialogs let us ship the feature in
  // one commit rather than three.
  protected promptNewFolder(): void {
    const name = window.prompt('New folder name')?.trim();
    if (!name) return;
    this.api.createMailbox(name).subscribe({
      next: () => {
        this.refreshMailboxes();
        this.selectMailbox(name);
      },
      error: (err) => window.alert(err.error?.error || 'Could not create folder.'),
    });
  }

  protected promptRenameFolder(current: string): void {
    const next = window.prompt(`Rename folder "${current}" to:`, current)?.trim();
    if (!next || next === current) return;
    this.api.renameMailbox(current, next).subscribe({
      next: () => {
        // If the user was viewing the folder we just renamed, follow
        // it to the new name so they don't end up on an empty page.
        if (this.selected() === current) this.selected.set(next);
        this.refreshMailboxes();
        this.loadMessages();
      },
      error: (err) => window.alert(err.error?.error || 'Could not rename folder.'),
    });
  }

  protected promptDeleteFolder(name: string, total: number): void {
    const note = total > 0
      ? ` This will permanently delete ${total} message${total === 1 ? '' : 's'} inside it.`
      : '';
    if (!window.confirm(`Delete folder "${name}"?${note}`)) return;
    this.api.deleteMailbox(name).subscribe({
      next: () => {
        // Bounce back to INBOX if the deleted folder was active —
        // showing "messages from a folder that no longer exists" is
        // the worst possible post-delete state.
        if (this.selected() === name) {
          this.selected.set('INBOX');
          this.openMessage.set(null);
        }
        this.refreshMailboxes();
        this.loadMessages();
      },
      error: (err) => window.alert(err.error?.error || 'Could not delete folder.'),
    });
  }

  // openCompose starts a fresh message — no seed.
  openCompose(): void {
    this.composeSeed.set(null);
    this.composing.set(true);
  }

  closeCompose(): void {
    this.composing.set(false);
    this.composeSeed.set(null);
  }

  // Gmail-style keyboard shortcuts. Active anywhere on the page
  // unless the user is typing into a real input (so `c` doesn't
  // hijack the search box's letter-by-letter typing). We also
  // bail on modifier-held keypresses so the browser's own
  // shortcuts (Cmd+R reload, Cmd+L address bar, etc.) keep working.
  //
  // The shortcut surface is small on purpose — j/k to navigate,
  // c/r to author, e/#/u to triage, / to search. That's the gmail
  // core; the rest of gmail's wider set (s star, x select, l label,
  // …) can come later when there's a real demand.
  @HostListener('document:keydown', ['$event'])
  protected onGlobalKeydown(e: KeyboardEvent): void {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    const target = e.target as HTMLElement | null;
    if (target && isEditable(target)) return;
    // Compose dialog is its own modal — its own escape / shortcut
    // semantics should win. We deliberately do nothing while it's
    // open (the dialog handles its own keys).
    if (this.composing()) return;

    switch (e.key) {
      case 'j':
        e.preventDefault();
        this.moveThreadCursor(+1);
        break;
      case 'k':
        e.preventDefault();
        this.moveThreadCursor(-1);
        break;
      case 'c':
        e.preventDefault();
        this.openCompose();
        break;
      case 'r': {
        const open = this.openMessage();
        if (open) {
          e.preventDefault();
          this.reply(open, e.shiftKey);
        }
        break;
      }
      case 'e': {
        // Archive the open message; if nothing's open, archive the
        // newest message of the first thread (so e on a fresh
        // landing still works).
        const target = this.openMessage() ?? this.threads()[0]?.messages[0];
        if (target) {
          e.preventDefault();
          this.archive(target);
        }
        break;
      }
      case '#': {
        const target = this.openMessage() ?? this.threads()[0]?.messages[0];
        if (target) {
          e.preventDefault();
          this.deleteOrTrash(target);
        }
        break;
      }
      case 's': {
        // Toggle the star on the open message; fall back to the
        // first thread when nothing's open (matches the e/# fallback
        // and lets a freshly-loaded inbox star the top row).
        const target = this.openMessage() ?? this.threads()[0]?.messages[0];
        if (target) {
          e.preventDefault();
          this.toggleFlagged(target);
        }
        break;
      }
      case 'u':
        // Back-to-list: clears the reader. Useful on narrow screens
        // (also handy on wide screens to deselect).
        if (this.openMessage()) {
          e.preventDefault();
          this.openMessage.set(null);
          this.view.set('list');
        }
        break;
      case '/': {
        // Focus the search input. We use a DOM lookup rather than a
        // @ViewChild ref because the search field is a tiny part of
        // a large template — querySelector is cheap and keeps the
        // component lean.
        const input = document.querySelector<HTMLInputElement>('input.search');
        if (input) {
          e.preventDefault();
          input.focus();
          input.select();
        }
        break;
      }
    }
  }

  // moveThreadCursor advances the open message to the next / previous
  // thread in the list. Wraps neither end — gmail also stops at
  // boundaries, and wrapping makes "I'm at the bottom, did I miss
  // one?" confusing.
  private moveThreadCursor(delta: number): void {
    const list = this.threads();
    if (list.length === 0) return;
    const open = this.openMessage();
    let idx = -1;
    if (open) {
      idx = list.findIndex((t) => t.messages.some((m) => m.id === open.id));
    }
    let next = idx + delta;
    if (idx === -1 && delta > 0) next = 0;
    if (idx === -1 && delta < 0) next = list.length - 1;
    if (next < 0 || next >= list.length) return;
    this.open(list[next].messages[0].id);
  }

  onSent(): void {
    this.closeCompose();
    this.refreshMailboxes();
    this.loadMessages();
  }

  logout(): void {
    // Tell the server to revoke the session; even on error, drop local
    // state and bounce to the login page — the server is the authority
    // but the user clearly wants out either way.
    const done = () => {
      this.api.clearSession();
      void this.router.navigate(['/login']);
    };
    this.api.logout().subscribe({ next: done, error: done });
  }

  // startResize captures the initial mouse position and current
  // pane size, then attaches document-level listeners that track
  // the drag through to mouseup. Listeners go on `document` (not
  // the 6px divider) so dragging works even when the cursor leaves
  // the tiny target — the standard splitter idiom.
  //
  // 'folders' tracks clientX → folders sidebar width.
  // 'split'   tracks clientY → list pane height in the main stack.
  protected startResize(event: MouseEvent, target: 'folders' | 'split'): void {
    event.preventDefault();
    this.resizeTarget = target;
    if (target === 'folders') {
      this.resizeStartCoord = event.clientX;
      this.resizeStartWidth = this.foldersWidth();
      document.body.style.cursor = 'col-resize';
    } else {
      this.resizeStartCoord = event.clientY;
      this.resizeStartWidth = this.listHeight();
      document.body.style.cursor = 'row-resize';
    }
    document.addEventListener('mousemove', this.onResizeMove);
    document.addEventListener('mouseup', this.onResizeEnd);
  }

  private resizeMove(event: MouseEvent): void {
    if (!this.resizeTarget) return;
    if (this.resizeTarget === 'folders') {
      const delta = event.clientX - this.resizeStartCoord;
      this.foldersWidth.set(clamp(this.resizeStartWidth + delta, FOLDERS_MIN, FOLDERS_MAX));
    } else {
      const delta = event.clientY - this.resizeStartCoord;
      this.listHeight.set(clamp(this.resizeStartWidth + delta, LIST_HEIGHT_MIN, LIST_HEIGHT_MAX));
    }
  }

  private resizeEnd(): void {
    if (!this.resizeTarget) return;
    // Persist the final value only on drag-end (not every mousemove)
    // so we don't hammer localStorage during the drag.
    const key = this.resizeTarget === 'folders' ? STORAGE_KEY_FOLDERS : STORAGE_KEY_LIST_HEIGHT;
    const value = this.resizeTarget === 'folders' ? this.foldersWidth() : this.listHeight();
    try { localStorage.setItem(key, String(value)); } catch { /* ignore quota / disabled storage */ }
    this.resizeTarget = null;
    document.body.style.cursor = '';
    document.removeEventListener('mousemove', this.onResizeMove);
    document.removeEventListener('mouseup', this.onResizeEnd);
  }

  // resetWidth (double-click on a divider) restores the pane to its
  // default size and clears the stored override.
  protected resetWidth(target: 'folders' | 'split'): void {
    if (target === 'folders') {
      this.foldersWidth.set(FOLDERS_DEFAULT);
      try { localStorage.removeItem(STORAGE_KEY_FOLDERS); } catch { /* ignore */ }
    } else {
      this.listHeight.set(LIST_HEIGHT_DEFAULT);
      try { localStorage.removeItem(STORAGE_KEY_LIST_HEIGHT); } catch { /* ignore */ }
    }
  }

  // loadWidth restores a persisted pane width, clamped into the legal
  // range. Returns the default if storage is empty, unparseable, or
  // out of bounds.
  private loadWidth(key: string, def: number, min: number, max: number): number {
    try {
      const raw = localStorage.getItem(key);
      if (!raw) return def;
      const n = parseInt(raw, 10);
      if (!Number.isFinite(n)) return def;
      return clamp(n, min, max);
    } catch {
      return def;
    }
  }

  // folderIcon maps an IMAP mailbox name to a Lucide icon. Standard
  // folder names (case-insensitive) get a recognisable icon; anything
  // else falls back to a generic folder.
  protected folderIcon(name: string) {
    switch (name.toUpperCase()) {
      case 'INBOX': return Inbox;
      case 'SENT': return Send;
      case 'DRAFTS': return Edit3;
      case 'TRASH': return Trash2;
      case 'JUNK': return ShieldAlert;
      case 'ARCHIVE': return Archive;
      default: return Folder;
    }
  }

  // threadHasOpen reports whether the currently-open message belongs to
  // this thread. Drives the .active highlight in the list view; matches
  // pre-thread behaviour where a row was active iff it was the open one.
  protected threadHasOpen(t: Thread): boolean {
    const open = this.openMessage();
    return !!open && t.messages.some((m) => m.id === open.id);
  }

  // threadSenders returns the comma-joined sender label for the row.
  // Singletons show the bare name; threads show "Alice, Bob" or
  // "Alice, Bob +1" when there are more than two distinct senders.
  // We pipe each through senderName so display-name parsing applies.
  protected threadSenders(t: Thread): string {
    const names = t.senders.map((s) => this.senderName(s));
    if (names.length === 0) return '(unknown)';
    if (names.length === 1) return names[0];
    if (names.length === 2) return names[0] + ', ' + names[1];
    return names[0] + ', ' + names[1] + ' +' + (names.length - 1);
  }

  // senderName extracts the human-readable part of an RFC 5322 From
  // header — "Alice <alice@x>" -> "Alice", or the address if no name
  // is present.
  protected senderName(from: string): string {
    if (!from) return '(unknown sender)';
    const m = from.match(/^\s*"?([^"<]+?)"?\s*<.+>$/);
    return m ? m[1].trim() : from.replace(/[<>]/g, '');
  }

  // initials returns one or two letters for the sender avatar. We
  // pick the first letter of the first two whitespace-separated tokens
  // of senderName; if there's only one token we fall back to its first
  // letter alone.
  protected initials(from: string): string {
    const name = this.senderName(from);
    const parts = name.split(/[\s.@]+/).filter(Boolean);
    if (parts.length === 0) return '?';
    if (parts.length === 1) return parts[0][0].toUpperCase();
    return (parts[0][0] + parts[1][0]).toUpperCase();
  }

  // avatarColor maps a sender to a stable HSL background so the same
  // sender always gets the same colour across reloads. Hash the
  // senderName for hue selection; saturation + lightness are fixed
  // to a palette that reads on both light and dark themes.
  protected avatarColor(from: string): string {
    const name = this.senderName(from);
    let hash = 0;
    for (let i = 0; i < name.length; i++) {
      hash = (hash * 31 + name.charCodeAt(i)) | 0;
    }
    const hue = Math.abs(hash) % 360;
    return `hsl(${hue}, 55%, 48%)`;
  }

  private refreshMailboxes(): void {
    this.api.mailboxes().subscribe({
      next: (boxes) => this.mailboxes.set(boxes),
    });
  }

  private loadMessages(): void {
    this.loadingList.set(true);
    this.api.messages(this.selected(), this.query()).subscribe({
      next: (msgs) => {
        this.messages.set(msgs);
        this.loadingList.set(false);
      },
      error: () => {
        this.messages.set([]);
        this.loadingList.set(false);
      },
    });
  }

  // applyUpdate folds a flag change back into the list and the open
  // message, and recomputes the current folder's unseen badge.
  private applyUpdate(updated: MessageSummary): void {
    this.messages.update((list) =>
      list.map((m) => (m.id === updated.id ? { ...m, ...updated } : m)),
    );
    const open = this.openMessage();
    if (open && open.id === updated.id) {
      this.openMessage.set({ ...open, flags: updated.flags, seen: updated.seen });
    }
    this.recountCurrent();
  }

  // afterRemoval reloads the folder after a message left it (move or
  // delete), refreshes the folder counts, and switches the gmail-style
  // single-pane back to the list so the user isn't staring at an
  // empty reader.
  private afterRemoval(): void {
    this.openMessage.set(null);
    this.openThreadDetails.set([]);
    this.view.set('list');
    this.refreshMailboxes();
    this.loadMessages();
  }

  private recountCurrent(): void {
    const name = this.selected();
    const unseen = this.messages().filter((m) => !m.seen).length;
    this.mailboxes.update((boxes) =>
      boxes.map((b) => (b.name === name ? { ...b, unseen } : b)),
    );
  }
}

// isEditable reports whether el (or any ancestor) is a control the
// user is actively typing into. The keyboard-shortcut handler bails
// out in that case so `c` doesn't yank the cursor out of the search
// box while you're spelling "contract". contenteditable covers
// rich-text editors (the Quill compose body matches), input /
// textarea / select cover the standard form fields.
function isEditable(el: HTMLElement): boolean {
  for (let cur: HTMLElement | null = el; cur; cur = cur.parentElement) {
    const tag = cur.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
    if (cur.isContentEditable) return true;
  }
  return false;
}

// SUBJECT_PREFIX_RE captures one leading reply/forward-style prefix
// plus the bracketed-tag prefix some lists glue on top
// ("[oximail-dev] Re: foo"). We strip iteratively so chains like
// "Re: Re: Fwd: …" collapse to the bare subject.
const SUBJECT_PREFIX_RE = /^\s*(?:re|fwd|fw|aw|ynt|rv|tr)\s*:\s*|^\s*\[[^\]]+\]\s*/i;

// normalizeSubject is the grouping key for thread detection. Strips
// every leading reply/forward prefix and bracketed tag, lowercases,
// and trims. An empty subject becomes "(no subject)" so messages with
// no header still bucket together (same as the row label does).
export function normalizeSubject(raw: string): string {
  let s = raw || '';
  while (true) {
    const next = s.replace(SUBJECT_PREFIX_RE, '');
    if (next === s) break;
    s = next;
  }
  s = s.trim().toLowerCase();
  return s === '' ? '(no subject)' : s;
}

// groupByThread folds a flat message list into Thread objects. The
// algorithm is deliberately simple: bucket by normalizeSubject, sort
// each bucket newest-first, then sort threads by their newest
// message's date so the list still feels "newest activity first".
export function groupByThread(msgs: MessageSummary[]): Thread[] {
  const buckets = new Map<string, MessageSummary[]>();
  for (const m of msgs) {
    const key = normalizeSubject(m.subject);
    const arr = buckets.get(key);
    if (arr) arr.push(m);
    else buckets.set(key, [m]);
  }
  const out: Thread[] = [];
  for (const [key, arr] of buckets) {
    arr.sort((a, b) => +new Date(b.date) - +new Date(a.date));
    const senders: string[] = [];
    const seen = new Set<string>();
    for (const m of arr) {
      const addr = (m.from || '').trim();
      if (addr && !seen.has(addr)) {
        seen.add(addr);
        senders.push(addr);
        if (senders.length === 3) break;
      }
    }
    out.push({
      key,
      subject: arr[0].subject || '(no subject)',
      messages: arr,
      senders,
      unreadCount: arr.filter((m) => !m.seen).length,
    });
  }
  // Sort threads by their newest message's date, newest first.
  out.sort((a, b) => +new Date(b.messages[0].date) - +new Date(a.messages[0].date));
  return out;
}

// addressOnly pulls "alice@x" out of "Alice <alice@x>"; on a plain
// address it is the identity.
function addressOnly(s: string): string {
  const start = s.lastIndexOf('<');
  const end = s.lastIndexOf('>');
  if (start >= 0 && end > start) {
    return s.slice(start + 1, end).trim();
  }
  return s.trim();
}

// prefixedSubject adds "Re: " / "Fwd: " unless the subject already
// starts with that prefix (case-insensitive) — so deep threads don't
// stack "Re: Re: Re:".
function prefixedSubject(subject: string, prefix: string): string {
  const lower = (subject || '').toLowerCase();
  if (lower.startsWith(prefix.toLowerCase())) {
    return subject;
  }
  return prefix + (subject || '');
}

// quoteBody builds the citation block that goes above the user's reply.
function quoteBody(msg: MessageDetail): string {
  const date = msg.date ? new Date(msg.date).toLocaleString() : '(unknown)';
  const header = `\n\nOn ${date}, ${msg.from} wrote:\n`;
  const body = (msg.text || stripHTML(msg.html || '')).split('\n').map((l) => '> ' + l).join('\n');
  return header + body;
}

// forwardBody builds the inline-forward block.
function forwardBody(msg: MessageDetail): string {
  const lines = [
    '',
    '',
    '---------- Forwarded message ----------',
    `From: ${msg.from}`,
    `To: ${msg.to.join(', ')}`,
    msg.cc.length ? `Cc: ${msg.cc.join(', ')}` : '',
    `Date: ${msg.date}`,
    `Subject: ${msg.subject}`,
    '',
    msg.text || stripHTML(msg.html || ''),
  ];
  return lines.filter((l) => l !== '').concat('').join('\n');
}

// stripHTML is the same crude tag-strip the server and the compose
// component use as a plain-text fallback.
function stripHTML(s: string): string {
  return s.replace(/<[^>]*>/g, '');
}

// clamp confines n to [min, max]. Used by the resizable splitter to
// keep pane widths inside the configured legal range.
function clamp(n: number, min: number, max: number): number {
  return Math.min(Math.max(n, min), max);
}
