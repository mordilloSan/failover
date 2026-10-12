// The events list: loading, filtering and the CSV export.
import {$, cap, clock, esc, icon, norm, paint, painted, reduceMotion, st, toLogin, when} from './core.js';
import {currentTab, renderAll} from './main.js';

export function evTone(m) {
  if (/^ERRO/.test(m)) return 'var(--error)';
  if (/^\[observação\]/.test(m)) return 'var(--neutral)';
  if (/inacessível|sem internet|falha|por concluir|por parar|manutenção/.test(m)) return 'var(--warning)';
  if (/de volta|recuperou|voltou|acessível|descarregadas|todas as imagens/.test(m)) return 'var(--success)';
  if (/failover|regresso|TNAS/.test(m)) return 'var(--info)';
  return 'var(--neutral)';
}
export const evItem = (e, fresh = false, withSvc = true) => `<li class="ev ${fresh ? 'fresh' : ''} ${/^ERRO/.test(e.msg) ? 'err' : ''}" style="--c:${evTone(e.msg)}">
    <span class="ev-dot"></span>
    <time datetime="${esc(e.t)}" title="${esc(when(e.t))}">${esc(evWhen(e.t))}</time>
    ${withSvc ? `<span class="ev-svc">${esc(e.svc || 'global')}</span>` : ''}
    <p>${esc(cap(e.msg))}</p></li>`;

// Every event (30 days) is fetched once, when the tab or a service panel
// first needs it; after that the ones in each status are merged on top.
export let evAll = null, evShown = 100, seen = null, evLoading = false, evMatch = [];
export const evKey = e => e.t + e.svc + e.msg;
// force: fetch again even when loaded (the status skipped some).
export async function loadEvents(force = false) {
  if ((evAll && !force) || evLoading) return;
  evLoading = true;
  try {
    const r = toLogin(await fetch('api/events', {cache: 'no-store'}));
    // seen = null: what the list gains now is history, not news
    if (r.ok) { evAll = await r.json(); seen = null; if (st) renderAll(); }
  } catch {
    // no answer: the tab shows the recent ones and the next visit tries again
  } finally { evLoading = false; }
}
export function mergeEvents() {
  if (!evAll) return;
  const known = new Set(evAll.map(evKey));
  const add = (st.events || []).filter(e => !known.has(evKey(e))).reverse();
  if (add.length) { evAll = [...add, ...evAll]; if (evAll.length > 5000) evAll.length = 5000; }
  // none of the status's events was known: more happened than it carries (a
  // laptop asleep, the agent unreachable), so fetch the whole list again
  if (evAll.length > add.length && add.length === (st.events || []).length && add.length >= 50) loadEvents(true);
}
// newest first, from whichever list is loaded
export const eventsNow = () => evAll || [...(st.events || [])].reverse();

const DM = new Intl.DateTimeFormat('pt-PT', {day: '2-digit', month: '2-digit'});
export const evWhen = t => {
  const d = new Date(t), today = new Date(), y = new Date(today - 864e5), hm = clock(t);
  if (d.toDateString() === today.toDateString()) return `hoje ${hm}`;
  if (d.toDateString() === y.toDateString()) return `ontem ${hm}`;
  return `${DM.format(d)} ${hm}`;
};

export function renderEvents() {
  mergeEvents();
  if (currentTab() !== 'overview') { seen = null; return; } // back on the page, nothing flashes as new
  const f = $('evSvc')?.value ?? '', rawQ = $('evQuery')?.value ?? '', q = norm(rawQ.trim());
  const names = [...new Set([...st.services.map(s => s.name), ...eventsNow().map(e => e.svc).filter(Boolean)])];
  const opts = `<option value="">Todos os serviços</option><option value="-">Global</option>${names.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('')}`;
  if (painted.evOpts !== opts) {
    painted.evOpts = opts;
    const wasTyping = document.activeElement?.id === 'evQuery', wasCaret = document.activeElement?.selectionStart ?? rawQ.length;
    paint('evFilters', `<select id="evSvc" aria-label="Serviço">${opts}</select>
      <input type="search" id="evQuery" placeholder="Procurar nos eventos…" aria-label="Procurar nos eventos">
      <button type="button" class="icon-btn" id="evExport" aria-label="Exportar CSV" title="Exportar CSV: os eventos deste filtro, para abrir numa folha de cálculo">${icon('download')}</button>`);
    const typing = wasTyping, caret = wasCaret;
    $('evSvc').value = f;
    $('evQuery').value = rawQ;
    if (typing) { $('evQuery').focus(); $('evQuery').setSelectionRange(caret, caret); }
  }
  const all = eventsNow();
  const match = all.filter(e => (!f || (f === '-' ? !e.svc : e.svc === f)) && (!q || norm(e.msg).includes(q) || norm(e.svc).includes(q)));
  evMatch = match;
  if ($('evExport')) { $('evExport').disabled = !evAll; $('evExport').title = evAll ? 'Exportar CSV: os eventos deste filtro, para abrir numa folha de cálculo' : 'A carregar o histórico…'; }
  const evs = match.slice(0, evShown);
  $('evCount').textContent = `${match.length} ${match.length === 1 ? 'evento' : 'eventos'} · 30 dias`;
  paint('events', evs.length ? evs.map(e => evItem(e, seen && !seen.has(evKey(e)) && !reduceMotion.matches)).join('')
    : `<li class="empty">${all.length ? 'Nenhum evento com esse filtro.' : 'Ainda sem eventos. As falhas, os failovers e os regressos aparecem aqui.'}</li>`);
  paint('evMore', match.length > evShown ? `<button class="btn" id="evMoreBtn">Carregar mais</button>` : '');
  seen = new Set(all.map(evKey));
}

document.addEventListener('input', e => {
  if (e.target.id === 'evQuery' || e.target.id === 'evSvc') { evShown = 100; renderEvents(); }
});
document.addEventListener('click', e => {
  if (e.target.id === 'evMoreBtn') { evShown += 100; renderEvents(); }
  if (e.target.closest('#evExport')) exportEvents();
});
// Exports what the filter shows (all of it, not only the rows drawn), as CSV
// with ; and a BOM, which a Portuguese Excel opens as columns.
export function exportEvents() {
  // a cell Excel would read as a formula (=, +, -, @) gets a ' first: messages carry outside text
  const q = v => `"${String(v ?? '').replace(/^[=+\-@\t\r]/, "'$&").replace(/"/g, '""')}"`;
  const rows = evMatch.map(e => [new Date(e.t).toLocaleString('sv-SE'), e.svc || 'global', e.msg].map(q).join(';'));
  const csv = '\ufeff' + ['"data";"serviço";"mensagem"', ...rows].join('\r\n') + '\r\n';
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([csv], {type: 'text/csv;charset=utf-8'}));
  a.download = `failover-eventos-${new Date().toLocaleDateString('sv-SE')}.csv`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}
