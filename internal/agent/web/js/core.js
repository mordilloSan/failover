// Shared by every part of the page: the status (st), helpers, icons, toasts and post.
import {refresh, renderAll} from './main.js';

// mdi icons, the set LinuxIO uses (@iconify-json/mdi)
export const ICONS = {"server":"M4 1h16a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V2a1 1 0 0 1 1-1m0 8h16a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-4a1 1 0 0 1 1-1m0 8h16a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-4a1 1 0 0 1 1-1M9 5h1V3H9zm0 8h1v-2H9zm0 8h1v-2H9zM5 3v2h2V3zm0 8v2h2v-2zm0 8v2h2v-2z","nas":"M4 5c-1.11 0-2 .89-2 2v10c0 1.11.89 2 2 2h16c1.11 0 2-.89 2-2V7c0-1.11-.89-2-2-2zm.5 2a1 1 0 0 1 1 1a1 1 0 0 1-1 1a1 1 0 0 1-1-1a1 1 0 0 1 1-1M7 7h13v10H7zm1 1v8h3V8zm4 0v8h3V8zm4 0v8h3V8zM9 9h1v1H9zm4 0h1v1h-1zm4 0h1v1h-1z","router":"M5 9c-1.1 0-2 .9-2 2v4a2 2 0 0 0 2 2h6v2h-1c-.55 0-1 .45-1 1H2v2h7c0 .55.45 1 1 1h4c.55 0 1-.45 1-1h7v-2h-7c0-.55-.45-1-1-1h-1v-2h6c1.11 0 2-.89 2-2v-4a2 2 0 0 0-2-2zm1 3h2v2H6zm3.5 0h2v2h-2zm3.5 0h2v2h-2z","shield":"M21 11c0 5.55-3.84 10.74-9 12c-5.16-1.26-9-6.45-9-12V5l9-4l9 4zm-9 10c3.75-1 7-5.46 7-9.78V6.3l-7-3.12L5 6.3v4.92C5 15.54 8.25 20 12 21m2.8-10V9.5C14.8 8.1 13.4 7 12 7S9.2 8.1 9.2 9.5V11c-.6 0-1.2.6-1.2 1.2v3.5c0 .7.6 1.3 1.2 1.3h5.5c.7 0 1.3-.6 1.3-1.2v-3.5c0-.7-.6-1.3-1.2-1.3m-1.3 0h-3V9.5c0-.8.7-1.3 1.5-1.3s1.5.5 1.5 1.3z","images":"M21 17H7V3h14m0-2H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2V3a2 2 0 0 0-2-2M3 5H1v16a2 2 0 0 0 2 2h16v-2H3m12.96-10.71l-2.75 3.54l-1.96-2.36L8.5 15h11z","speed":"M12 16a3 3 0 0 1-3-3c0-1.12.61-2.1 1.5-2.61l9.71-5.62l-5.53 9.58c-.5.98-1.51 1.65-2.68 1.65m0-13c1.81 0 3.5.5 4.97 1.32l-2.1 1.21C14 5.19 13 5 12 5a8 8 0 0 0-8 8c0 2.21.89 4.21 2.34 5.65h.01c.39.39.39 1.02 0 1.41s-1.03.39-1.42.01A9.97 9.97 0 0 1 2 13A10 10 0 0 1 12 3m10 10c0 2.76-1.12 5.26-2.93 7.07c-.39.38-1.02.38-1.41-.01a.996.996 0 0 1 0-1.41A7.95 7.95 0 0 0 20 13c0-1-.19-2-.54-2.9L20.67 8C21.5 9.5 22 11.18 22 13","dashboard":"M19 5v2h-4V5zM9 5v6H5V5zm10 8v6h-4v-6zM9 17v2H5v-2zM21 3h-8v6h8zM11 3H3v10h8zm10 8h-8v10h8zm-10 4H3v6h8z","ap":"M4.93 4.93A9.97 9.97 0 0 0 2 12c0 2.76 1.12 5.26 2.93 7.07l1.41-1.41A7.94 7.94 0 0 1 4 12c0-2.21.89-4.22 2.34-5.66zm14.14 0l-1.41 1.41A7.96 7.96 0 0 1 20 12c0 2.22-.89 4.22-2.34 5.66l1.41 1.41A9.97 9.97 0 0 0 22 12c0-2.76-1.12-5.26-2.93-7.07M7.76 7.76A5.98 5.98 0 0 0 6 12c0 1.65.67 3.15 1.76 4.24l1.41-1.41A4 4 0 0 1 8 12c0-1.11.45-2.11 1.17-2.83zm8.48 0l-1.41 1.41A4 4 0 0 1 16 12c0 1.11-.45 2.11-1.17 2.83l1.41 1.41A5.98 5.98 0 0 0 18 12c0-1.65-.67-3.15-1.76-4.24M12 10a2 2 0 0 0-2 2a2 2 0 0 0 2 2a2 2 0 0 0 2-2a2 2 0 0 0-2-2","cube":"M21 16.5c0 .38-.21.71-.53.88l-7.9 4.44c-.16.12-.36.18-.57.18s-.41-.06-.57-.18l-7.9-4.44A.99.99 0 0 1 3 16.5v-9c0-.38.21-.71.53-.88l7.9-4.44c.16-.12.36-.18.57-.18s.41.06.57.18l7.9 4.44c.32.17.53.5.53.88zM12 4.15L6.04 7.5L12 10.85l5.96-3.35zM5 15.91l6 3.38v-6.71L5 9.21zm14 0v-6.7l-6 3.37v6.71z","swap":"m21 9l-4-4v3h-7v2h7v3M7 11l-4 4l4 4v-3h7v-2H7z","wrench":"m22.61 19l-9.08-9.09c.93-2.34.47-5.1-1.44-7C9.79.61 6.21.4 3.66 2.26L7.5 6.11L6.08 7.5L2.25 3.69C.39 6.23.6 9.82 2.9 12.11c1.86 1.86 4.57 2.35 6.89 1.48l9.11 9.11c.39.39 1.02.39 1.41 0l2.3-2.3c.39-.4.39-1.01 0-1.4m-3 1.59l-9.46-9.46c-.61.45-1.29.72-2 .82c-1.36.2-2.79-.21-3.83-1.25C3.37 9.76 2.93 8.5 3 7.26l3.09 3.09l4.24-4.24L7.24 3c1.26-.05 2.49.39 3.44 1.33a4.47 4.47 0 0 1 1.24 3.96a4.35 4.35 0 0 1-.88 1.96l9.46 9.45z","alert":"M11 15h2v2h-2zm0-8h2v6h-2zm1-5C6.47 2 2 6.5 2 12a10 10 0 0 0 10 10a10 10 0 0 0 10-10A10 10 0 0 0 12 2m0 18a8 8 0 0 1-8-8a8 8 0 0 1 8-8a8 8 0 0 1 8 8a8 8 0 0 1-8 8","check":"M12 2C6.5 2 2 6.5 2 12s4.5 10 10 10s10-4.5 10-10S17.5 2 12 2m0 18c-4.41 0-8-3.59-8-8s3.59-8 8-8s8 3.59 8 8s-3.59 8-8 8m4.59-12.42L10 14.17l-2.59-2.58L6 13l4 4l8-8z","restore":"M12 3a9 9 0 0 0-9 9H0l4 4l4-4H5a7 7 0 0 1 7-7a7 7 0 0 1 7 7a7 7 0 0 1-7 7c-1.5 0-2.91-.5-4.06-1.3L6.5 19.14A9.1 9.1 0 0 0 12 21a9 9 0 0 0 9-9a9 9 0 0 0-9-9m2 9a2 2 0 0 0-2-2a2 2 0 0 0-2 2a2 2 0 0 0 2 2a2 2 0 0 0 2-2","layers":"m12 18.54l7.37-5.74L21 14.07l-9 7l-9-7l1.62-1.26zM12 16L3 9l9-7l9 7zm0-11.47L6.26 9L12 13.47L17.74 9z","close":"M19 6.41L17.59 5L12 10.59L6.41 5L5 6.41L10.59 12L5 17.59L6.41 19L12 13.41L17.59 19L19 17.59L13.41 12z","docker":"M21.81 10.25c-.06-.04-.56-.43-1.64-.43c-.28 0-.56.03-.84.08c-.21-1.4-1.38-2.11-1.43-2.14l-.29-.17l-.18.27c-.24.36-.43.77-.51 1.19c-.2.8-.08 1.56.33 2.21c-.49.28-1.29.35-1.46.35H2.62c-.34 0-.62.28-.62.63c0 1.15.18 2.3.58 3.38c.45 1.19 1.13 2.07 2 2.61c.98.6 2.59.94 4.42.94c.79 0 1.61-.07 2.42-.22c1.12-.2 2.2-.59 3.19-1.16A8.3 8.3 0 0 0 16.78 16c1.05-1.17 1.67-2.5 2.12-3.65h.19c1.14 0 1.85-.46 2.24-.85c.26-.24.45-.53.59-.87l.08-.24zm-17.96.99h1.76c.08 0 .16-.07.16-.16V9.5c0-.08-.07-.16-.16-.16H3.85c-.09 0-.16.07-.16.16v1.58c.01.09.07.16.16.16m2.43 0h1.76c.08 0 .16-.07.16-.16V9.5c0-.08-.07-.16-.16-.16H6.28c-.09 0-.16.07-.16.16v1.58c.01.09.07.16.16.16m2.47 0h1.75c.1 0 .17-.07.17-.16V9.5c0-.08-.06-.16-.17-.16H8.75c-.08 0-.15.07-.15.16v1.58c0 .09.06.16.15.16m2.44 0h1.77c.08 0 .15-.07.15-.16V9.5c0-.08-.06-.16-.15-.16h-1.77c-.08 0-.15.07-.15.16v1.58c0 .09.07.16.15.16M6.28 9h1.76c.08 0 .16-.09.16-.18V7.25c0-.09-.07-.16-.16-.16H6.28c-.09 0-.16.06-.16.16v1.57c.01.09.07.18.16.18m2.47 0h1.75c.1 0 .17-.09.17-.18V7.25c0-.09-.06-.16-.17-.16H8.75c-.08 0-.15.06-.15.16v1.57c0 .09.06.18.15.18m2.44 0h1.77c.08 0 .15-.09.15-.18V7.25c0-.09-.07-.16-.15-.16h-1.77c-.08 0-.15.06-.15.16v1.57c0 .09.07.18.15.18m0-2.28h1.77c.08 0 .15-.07.15-.16V5c0-.1-.07-.17-.15-.17h-1.77c-.08 0-.15.06-.15.17v1.56c0 .08.07.16.15.16m2.46 4.52h1.76c.09 0 .16-.07.16-.16V9.5c0-.08-.07-.16-.16-.16h-1.76c-.08 0-.15.07-.15.16v1.58c0 .09.07.16.15.16","clock":"M12 20a8 8 0 0 0 8-8a8 8 0 0 0-8-8a8 8 0 0 0-8 8a8 8 0 0 0 8 8m0-18a10 10 0 0 1 10 10a10 10 0 0 1-10 10C6.47 22 2 17.5 2 12A10 10 0 0 1 12 2m.5 5v5.25l4.5 2.67l-.75 1.23L11 13V7z","refresh":"M17.65 6.35A7.96 7.96 0 0 0 12 4a8 8 0 0 0-8 8a8 8 0 0 0 8 8c3.73 0 6.84-2.55 7.73-6h-2.08A5.99 5.99 0 0 1 12 18a6 6 0 0 1-6-6a6 6 0 0 1 6-6c1.66 0 3.14.69 4.22 1.78L13 11h7V4z","dns":"M7 9a2 2 0 0 1-2-2a2 2 0 0 1 2-2a2 2 0 0 1 2 2a2 2 0 0 1-2 2m13-6H4a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h16a1 1 0 0 0 1-1V4a1 1 0 0 0-1-1M7 19a2 2 0 0 1-2-2a2 2 0 0 1 2-2a2 2 0 0 1 2 2a2 2 0 0 1-2 2m13-6H4a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h16a1 1 0 0 0 1-1v-6a1 1 0 0 0-1-1","devices":"M3 6h18V4H3c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h4v-2H3zm10 6H9v1.78c-.61.55-1 1.33-1 2.22s.39 1.67 1 2.22V20h4v-1.78c.61-.55 1-1.34 1-2.22s-.39-1.67-1-2.22zm-2 5.5c-.83 0-1.5-.67-1.5-1.5s.67-1.5 1.5-1.5s1.5.67 1.5 1.5s-.67 1.5-1.5 1.5M22 8h-6c-.5 0-1 .5-1 1v10c0 .5.5 1 1 1h6c.5 0 1-.5 1-1V9c0-.5-.5-1-1-1m-1 10h-4v-8h4z","key":"M22 18v4h-4v-3h-3v-3h-3l-2.26-2.26c-.55.17-1.13.26-1.74.26a6 6 0 0 1-6-6a6 6 0 0 1 6-6a6 6 0 0 1 6 6c0 .61-.09 1.19-.26 1.74zM7 5a2 2 0 0 0-2 2a2 2 0 0 0 2 2a2 2 0 0 0 2-2a2 2 0 0 0-2-2","cog":"M12 15.5A3.5 3.5 0 0 1 8.5 12A3.5 3.5 0 0 1 12 8.5a3.5 3.5 0 0 1 3.5 3.5a3.5 3.5 0 0 1-3.5 3.5m7.43-2.53c.04-.32.07-.64.07-.97s-.03-.66-.07-1l2.11-1.63c.19-.15.24-.42.12-.64l-2-3.46c-.12-.22-.39-.31-.61-.22l-2.49 1c-.52-.39-1.06-.73-1.69-.98l-.37-2.65A.506.506 0 0 0 14 2h-4c-.25 0-.46.18-.5.42l-.37 2.65c-.63.25-1.17.59-1.69.98l-2.49-1c-.22-.09-.49 0-.61.22l-2 3.46c-.13.22-.07.49.12.64L4.57 11c-.04.34-.07.67-.07 1s.03.65.07.97l-2.11 1.66c-.19.15-.25.42-.12.64l2 3.46c.12.22.39.3.61.22l2.49-1.01c.52.4 1.06.74 1.69.99l.37 2.65c.04.24.25.42.5.42h4c.25 0 .46-.18.5-.42l.37-2.65c.63-.26 1.17-.59 1.69-.99l2.49 1.01c.22.08.49 0 .61-.22l2-3.46c.12-.22.07-.49-.12-.64z","web":"M16.36 14c.08-.66.14-1.32.14-2s-.06-1.34-.14-2h3.38c.16.64.26 1.31.26 2s-.1 1.36-.26 2m-5.15 5.56c.6-1.11 1.06-2.31 1.38-3.56h2.95a8.03 8.03 0 0 1-4.33 3.56M14.34 14H9.66c-.1-.66-.16-1.32-.16-2s.06-1.35.16-2h4.68c.09.65.16 1.32.16 2s-.07 1.34-.16 2M12 19.96c-.83-1.2-1.5-2.53-1.91-3.96h3.82c-.41 1.43-1.08 2.76-1.91 3.96M8 8H5.08A7.92 7.92 0 0 1 9.4 4.44C8.8 5.55 8.35 6.75 8 8m-2.92 8H8c.35 1.25.8 2.45 1.4 3.56A8 8 0 0 1 5.08 16m-.82-2C4.1 13.36 4 12.69 4 12s.1-1.36.26-2h3.38c-.08.66-.14 1.32-.14 2s.06 1.34.14 2M12 4.03c.83 1.2 1.5 2.54 1.91 3.97h-3.82c.41-1.43 1.08-2.77 1.91-3.97M18.92 8h-2.95a15.7 15.7 0 0 0-1.38-3.56c1.84.63 3.37 1.9 4.33 3.56M12 2C6.47 2 2 6.5 2 12a10 10 0 0 0 10 10a10 10 0 0 0 10-10A10 10 0 0 0 12 2","download":"M5 20h14v-2H5zM19 9h-4V3H9v6H5l7 7z","email":"M22 6c0-1.1-.9-2-2-2H4c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2zm-2 0l-8 5l-8-5zm0 12H4V8l8 5l8-5z","plus":"M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6z","logout":"m17 7l-1.41 1.41L18.17 11H8v2h10.17l-2.58 2.58L17 17l5-5M4 5h8V3H4c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h8v-2H4z"};
export const SVC_ICON = {vaultwarden: 'shield', homepage: 'dashboard', speedtest: 'speed', immich: 'images', unifi: 'ap', npm: 'dns'};
// A service's icon: the image the agent downloaded and keeps (from its link,
// or dashboard-icons by name), else one of the page's own.
export const svcIcon = name => {
  const v = st?.services.find(x => x.name === name)?.icon_v;
  return v ? `<img class="icon svc-img" src="icons/${encodeURIComponent(name)}?v=${esc(v)}" alt="" draggable="false">` : icon(SVC_ICON[name] || 'cube');
};
export const STATES = {
  NORMAL: {label: 'No servidor', c: 'var(--success)'},
  FAILING_OVER: {label: 'Failover em curso', c: 'var(--warning)', busy: true},
  ACTIVE: {label: 'No TNAS', c: 'var(--info)'},
  RETURNING: {label: 'A regressar', c: 'var(--warning)', busy: true},
  ERROR: {label: 'Erro', c: 'var(--error)'},
};
export const EASE = 'cubic-bezier(.2, .8, .2, 1)';
export const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)');

// A service still in NORMAL but failing on the server is shown as failing everywhere.
export const look = s => s.state === 'NORMAL' && st.router_ok && !s.server_ok
  ? {label: 'Em falha', c: 'var(--warning)'} : STATES[s.state] || STATES.ERROR;
export const $ = id => document.getElementById(id);
export const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
// for search: no case, no accents
export const norm = s => String(s ?? '').normalize('NFD').replace(/\p{M}/gu, '').toLowerCase();
export const icon = (name, cls = '') => `<svg class="icon ${cls}" viewBox="0 0 24 24" aria-hidden="true"><path d="${ICONS[name] || ICONS.cube}"/></svg>`;
export const minsSince = t => (Date.now() - Date.parse(t)) / 60000;
export const dur = m => {
  if (m < 1) return 'menos de 1 min';
  m = Math.round(m);
  if (m < 60) return `${m} min`;
  if (m < 48 * 60) return `${Math.floor(m / 60)} h${m % 60 ? ` ${m % 60} min` : ''}`;
  return `${Math.round(m / 1440)} dias`;
};
export const ago = t => t ? `há ${dur(minsSince(t))}` : '';
// one formatter each, made once: creating one per call costs more than the formatting
const HM = new Intl.DateTimeFormat('pt-PT', {hour: '2-digit', minute: '2-digit'});
const DMY = new Intl.DateTimeFormat('pt-PT', {day: '2-digit', month: '2-digit', year: 'numeric'});
const FULL = new Intl.DateTimeFormat('pt-PT', {day: '2-digit', month: '2-digit', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit'});
const HMS = new Intl.DateTimeFormat('pt-PT', {hour: '2-digit', minute: '2-digit', second: '2-digit'});
export const clock = t => HM.format(new Date(t));
export const clockS = t => HMS.format(new Date(t));
export const when = t => FULL.format(new Date(t));
export const day = t => DMY.format(new Date(t));
export const bytes = n => n >= 1e9 ? `${(n / 1e9).toLocaleString('pt-PT', {maximumFractionDigits: 1})} GB` : `${Math.max(1, Math.round(n / 1e6))} MB`;
export const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;
export const cap = s => s ? s[0].toUpperCase() + s.slice(1) : s;
export const sentence = s => s ? cap(s) + (/[.!?]$/.test(s) ? '' : '.') : '';
export const chip = (label, c, cls = '') => `<span class="chip ${cls}" style="--c:${c}">${/busy|quiet/.test(cls) ? '<span class="dot"></span>' : ''}${esc(label)}</span>`;
// healthy on the server: nothing to look at
export const fine = s => s.state === 'NORMAL' && s.server_ok && st.router_ok;
export const mono = s => `<span class="mono">${esc(s)}</span>`;
export const kv = rows => `<dl class="kv">${rows.filter(Boolean).map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`).join('')}</dl>`;
export const sec = (title, html, side = '') => `<section class="sec"><div class="sec-head"><h3>${esc(title)}</h3>${side}</div>${html}</section>`;

export let st = null, lastOk = 0;
// refresh, in main.js, sets them: an imported binding cannot be assigned
export const setStatus = s => { st = s; };
export const setLastOk = t => { lastOk = t; };
// Requests in flight, by control: key → the value the user asked for. The
// agent may take a while to answer (it waits for a compose up to finish), so
// controls show what was asked for, disabled, until the answer arrives.
export const pending = new Map();
export const painted = {};
// Only touch the DOM when the markup changed, so focus and hover survive the 5 s refresh.
// a selector that finds the same control after a repaint: its id, else its tag and data-* attributes
const focusKey = e => {
  if (e.id) return `#${CSS.escape(e.id)}`;
  const data = [...e.attributes].filter(a => a.name.startsWith('data-')).map(a => `[${a.name}="${CSS.escape(a.value)}"]`).join('');
  return data && e.tagName.toLowerCase() + data; // a bare tag would focus the wrong element: restore nothing
};
export const paint = (id, html) => {
  if (painted[id] === html) return;
  painted[id] = html;
  const el = $(id), ae = document.activeElement, key = el.contains(ae) ? focusKey(ae) : '';
  el.innerHTML = html;
  if (key) el.querySelector(key)?.focus({preventScroll: true});
};

document.querySelectorAll('[data-icon]').forEach(el => { el.innerHTML = icon(el.dataset.icon); });

export function toast(msg, ok = true) {
  const el = document.createElement('div');
  el.className = 'toast';
  el.setAttribute('role', ok ? 'status' : 'alert');
  el.style.setProperty('--c', ok ? 'var(--success)' : 'var(--error)');
  el.innerHTML = `${icon(ok ? 'check' : 'alert')}<span>${esc(msg)}</span>${ok ? '' : `<button type="button" class="icon-btn toast-close" aria-label="Fechar">${icon('close')}</button>`}`;
  $('toasts').append(el);
  if (ok) setTimeout(() => el.remove(), 4000);
  else el.querySelector('.toast-close').addEventListener('click', () => el.remove());
}

export async function post(path, body, done, key, value = true) {
  pending.set(key, value);
  renderAll();
  let ok = false;
  try {
    const r = toLogin(await fetch(path, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)}));
    if (!r.ok) throw new Error((await readErr(r)).error);
    toast(done);
    ok = true;
  } catch (e) {
    toast(`Não foi possível: ${e.message}`, false);
  }
  pending.delete(key);
  // refresh(true) forces a render too, but only if the fetch works: this one un-sticks the control
  // when it fails (and shows the old state for the round trip)
  renderAll();
  await refresh(true);
  return ok;
}

export function confirmAction(title, body, ok) {
  $('dlgTitle').textContent = title;
  $('dlgBody').textContent = body;
  $('dlgOk').textContent = ok;
  const dlg = $('confirm');
  dlg.returnValue = '';
  dlg.showModal();
  return new Promise(res => dlg.addEventListener('close', () => res(dlg.returnValue === 'ok'), {once: true}));
}

// What an answer that failed says: {field, error} from JSON, else its text.
export async function readErr(r) {
  const t = (await r.text()).trim();
  try { const j = JSON.parse(t); if (j && typeof j === 'object') return {field: j.field, error: j.error || t}; } catch {}
  return {error: t || `HTTP ${r.status}`};
}

// The session ended (agent restarted, idle for a week, password changed elsewhere).
export const toLogin = r => { if (r.status === 401) location.href = 'login'; return r; };
