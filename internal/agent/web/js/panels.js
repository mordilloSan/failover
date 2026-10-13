// The details sheet of a service, the server and the TNAS, and the service form.
import {$, ago, bytes, chip, clock, clockS, confirmAction, esc, icon, kv, look, mono, paint, painted, pending, plural, readErr, sec, sentence, st, svcIcon, toLogin, toast, when} from './core.js';
import {actions, allStacks, beatsBlock, fillBeats, imageList, imgCount, notesHtml, serverChip, serverHealth, statusBlock, stepsHtml, tnasChip, tnasHealth} from './topology.js';
import {evItem, eventsNow, loadEvents} from './events.js';
import {atHome, discOf, forgetDiscovery, getDiscovery} from './settings.js';
import {refresh} from './main.js';

// Details sheet: opened from a node, a pill or a card title; redrawn on every refresh while open.
export let panel = null;
export function openPanel(kind, name, prefill) {
  if (kind === 'svc') loadEvents();
  panel = {kind, name, prefill};
  if (kind === 'svcEdit') loadServiceForm(panel);
  painted.panelHead = painted.panelBody = painted.panelFoot = null;
  renderPanel();
  if (!$('panel').open) $('panel').showModal();
}

export function serverPanel() {
  const svcs = st.services.map(s => {
    const S = look(s);
    const onServer = !st.router_ok ? chip('Por verificar', 'var(--neutral)', 'xs')
      : s.server_ok ? chip('A responder', 'var(--success)', 'xs') : chip(`Em falha${s.fail_since ? ' ' + ago(s.fail_since) : ''}`, 'var(--warning)', 'xs');
    return `<li><span style="color:${S.c}">${svcIcon(s.name)}</span>
      <span class="grow"><button class="svc-open" data-panel="svc" data-svc="${esc(s.name)}">${esc(s.name)}</button> <span class="mono muted">${esc(s.host)}</span></span>
      ${s.state !== 'NORMAL' ? chip(S.label, S.c, 'xs') : ''}${onServer}</li>`;
  }).join('');
  return {
    head: `<span class="node-icon st st-${serverHealth().s}" title="${esc(serverHealth().why)}">${icon('server')}</span><div class="node-id"><h2 id="panelTitle">Servidor</h2>${mono(st.server_ip)}</div>${serverChip()}`,
    body: sec('Como é verificado', `<p>A cada ${st.check_interval_s} s o agente pede ${mono('https://' + st.npm_check_host)} e o endereço de cada serviço, ligado diretamente a ${mono(st.server_ip)}. Se o NPM falhar e o servidor responder a ping, só avisa: não há failover.</p>`)
      + sec('Estado', kv([
        ['NPM do servidor', st.server_npm_ok ? '<span class="ok-text">A responder</span>'
          : `<span class="${st.npm_alerted ? 'warn-text' : 'bad-text'}">Em falha ${esc(ago(st.npm_fail_since))}${st.npm_alerted ? ', com o servidor vivo: aviso enviado' : ''}</span>`],
        ['Servidor', !st.router_ok ? 'Por verificar' : st.server_up ? '<span class="ok-text">Responde</span>' : '<span class="bad-text">Não responde ao NPM nem a ping</span>'],
        ['Router', `${st.router_ok ? '<span class="ok-text">Acessível</span>' : '<span class="bad-text">Inacessível: o agente não decide nada</span>'} ${mono(st.router_ip)}`],
        ['Última verificação', `às ${esc(clockS(st.now))}`],
      ]))
      + sec('Últimas verificações do NPM', beatsBlock('npm'))
      + sec('Serviços', `<ul class="list">${svcs}</ul>`),
  };
}

export function tnasPanel() {
  const n = st.tnas_npm;
  const copies = st.services.filter(s => s.snapshot || s.state === 'ACTIVE' || s.state === 'FAILING_OVER');
  const c = imgCount(allStacks());
  const scanning = st.images_scanning || pending.has('images');
  const scanBtn = scanning ? '<button class="btn busy" disabled><span class="dot"></span>A verificar…</button>'
    : `<button class="btn" data-scan>${icon('refresh')}Verificar agora</button>`;
  const summary = !c.known ? '<p class="note-text">As imagens ainda não foram verificadas.</p>'
    : `<p class="${c.missing + c.unreadable ? 'warn-text' : ''}">${c.missing + c.unreadable
        ? `${plural(c.missing + c.unreadable, 'imagem em falta', 'imagens em falta')}. Sem elas, o failover desse serviço precisa da internet para arrancar.`
        : c.present === 1 ? `A imagem de que os failovers precisam está no TNAS, ${bytes(c.size)}.`
        : `As ${c.present} imagens de que os failovers precisam estão no TNAS, ${bytes(c.size)} no total.`}</p>
      <p class="note-text">Verificado ${esc(ago(st.images_at))}, a partir dos composes do espelho, com os overrides aplicados.</p>`;
  const stacks = allStacks().map(name => `<div class="stack"><div class="stack-name">${svcIcon(name)}${name === 'npm' ? 'NPM do TNAS' : esc(name)}</div>${imageList(name)}</div>`).join('');
  return {
    head: `<span class="node-icon st st-${tnasHealth().s}" title="${esc(tnasHealth().why)}">${icon('nas')}</span><div class="node-id"><h2 id="panelTitle">TNAS</h2>${mono(st.tnas_ip)}</div>${tnasChip()}`,
    body: (st.verdict?.at ? sec('Pronto para failover', (st.verdict.problems?.length
        ? notesHtml(st.verdict.problems) : `<p class="ok-text">${icon('check')} Nada em falta.</p>`)
        + `<p class="note-text">Verificado às ${esc(clock(st.verdict.at))}: o token do Technitium, a app Failover e o *.${esc(st.dns_zone)}, um snapshot de teste do espelho, as imagens e o que o TNAS precisa para cada compose (redes externas, portas livres).</p>`) : '')
      + sec('Estado', kv([
        ['Agente', `<span class="ok-text">A correr</span>, versão ${mono(st.version)}. É ele que serve esta página`],
        st.mirror_changed && ['Espelho', st.mirror_stale ? `<span class="warn-text">Parado desde ${esc(when(st.mirror_changed))}: o backup do TOS parou?</span>`
          : `Última mudança ${esc(ago(st.mirror_changed))}`],
        ['IP da LAN', st.tnas_up ? `<span class="ok-text">Responde</span> ${mono(st.tnas_ip)}` : `<span class="bad-text">Não responde</span> ${mono(st.tnas_ip)}`],
        ['Router', st.router_ok ? `<span class="ok-text">Acessível</span> ${mono(st.router_ip)}` : `<span class="bad-text">Inacessível</span> ${mono(st.router_ip)}`],
      ]) + '<p class="note-text">O TNAS está na LAN quando o próprio IP responde e chega ao router. Um ping ao próprio IP não sai da máquina: sozinho só confirma que o IP está ativo.</p>')
      + sec('NPM do TNAS', kv([
        ['Estado', !n.snapshot && !n.stand_in ? 'Parado, à espera de um failover ou do NPM do servidor em falha' : n.ok ? '<span class="ok-text">A servir</span>' : '<span class="warn-text">A arrancar</span>'],
        n.stand_in && ['Porquê', `O NPM do servidor não responde: serve o que a app Failover do Technitium manda para o TNAS${st.dns_zone ? ` (${mono('*.' + st.dns_zone)})` : ''}`],
        n.msg && ['Problema', `<span class="warn-text">${esc(n.msg)}</span>`],
        n.snapshot && ['Snapshot', mono(n.snapshot)],
        n.snapshot && ['Desde', esc(when(n.since))],
      ]) + '<p class="note-text">Arranca a partir de um snapshot do espelho com o primeiro failover, ou com o NPM do servidor sem resposta há 1 min: a app Failover do Technitium manda então o resto dos nomes para o TNAS. Para quando o NPM do servidor responde e já nenhuma cópia precisa dele.</p>')
      + sec('Cópias a correr', copies.length ? `<ul class="list">${copies.map(s => `<li><span style="color:${look(s).c}">${svcIcon(s.name)}</span>
          <span class="grow"><button class="svc-open" data-panel="svc" data-svc="${esc(s.name)}">${esc(s.name)}</button>
          <span class="mono muted">${esc((s.snapshot || '').split('/').pop())}</span></span>
          ${s.dns ? chip('DNS → TNAS', 'var(--info)', 'xs') : ''}${chip(look(s).label, look(s).c, 'xs')}</li>`).join('')}</ul>`
        : '<p class="note-text">Nenhuma cópia a correr. Cada failover arranca de um snapshot Btrfs do espelho.</p>')
      + sec('Imagens para o failover', summary + stacks, scanBtn)
      + sec('Descarga noturna', `<p>${st.prepull_at ? `Todas as noites às ${esc(st.prepull_at)} e ao arrancar o agente, a partir dos composes do espelho; as imagens substituídas são apagadas.` : 'Desligada.'}
          Última: ${esc(st.last_pull || 'ainda nenhuma')}.</p>`),
  };
}

// Wait and stability as lists; a value from the file that is not in the list still shows.
export const MINUTES = [0, 1, 2, 3, 5, 10, 15, 20, 30, 45, 60];
export function minuteSelect(s, field, min) {
  const key = `${field}:${s.name}`, v = pending.has(key) ? pending.get(key) : s[field];
  return `<select data-min="${field}" data-svc="${esc(s.name)}" ${pending.has(key) ? 'disabled' : ''}>${optMinutes(v, min)}</select>`;
}

// The service form: new (no name) or editing one. Its data (the mirror's
// folders, the override) is fetched once when it opens.
export async function loadServiceForm(p) {
  try {
    const [dirs, override] = await Promise.all([
      fetch('api/mirror', {cache: 'no-store'}).then(r => toLogin(r).json()),
      p.name ? fetch(`api/service/override?name=${encodeURIComponent(p.name)}`, {cache: 'no-store'}).then(r => toLogin(r).text()) : ''],
    );
    p.data = {dirs, override};
  } catch (err) {
    p.data = {dirs: [], override: '', err: err.message};
  }
  if (panel === p) { painted.panelHead = painted.panelBody = painted.panelFoot = null; renderPanel(); }
}
export const optMinutes = (v, min) => [...new Set([...MINUTES.filter(m => m >= min), v])].sort((a, b) => a - b)
  .map(m => `<option value="${m}" ${m === v ? 'selected' : ''}>${m} min</option>`).join('');
export function svcEditPanel() {
  const s = st.services.find(x => x.name === panel.name), isNew = !s;
  const head = `<span class="svc-icon">${isNew ? icon('cube') : svcIcon(s.name)}</span>
    <div class="node-id"><h2 id="panelTitle">${isNew ? 'Novo serviço' : `Editar ${esc(s.name)}`}</h2><span class="muted">${isNew ? 'Uma pasta do espelho com docker-compose.yml' : mono(s.host)}</span></div>`;
  if (!panel.data) return {head, body: '<p class="note-text" style="padding-top:20px">A carregar…</p>'};
  const pre = !s && panel.prefill ? discOf(panel.prefill) : null;
  const v = s || {name: pre?.name || '', dir: pre?.dir || '', host: pre?.host || '', wait_min: 2, stability_min: 2, icon: '', require_free_ip: pre?.require_free_ip || ''};
  if (pre && !panel.data.override) panel.data.override = pre.override_yaml || '';
  const home = isNew || atHome(s);
  const dirs = panel.data.dirs.map(d => d.dir);
  if (v.dir && !dirs.includes(v.dir)) dirs.unshift(v.dir);
  const field = (k, l, input, h = '') => `<label class="set-field" data-k="${k}"><span class="set-label">${l}</span>
    <span class="set-input">${input}</span>${h ? `<small class="set-help">${h}</small>` : ''}<small class="field-err" role="alert"></small></label>`;
  const auto = pre ? {name: pre.name, host: pre.host || '', override_yaml: pre.override_yaml || '', require_free_ip: pre.require_free_ip || ''} : {};
  const body = `<form id="svcForm" novalidate autocomplete="off" data-auto="${esc(JSON.stringify(auto))}">
    ${panel.data.err ? `<p class="bad-text">Não consegui ler o espelho: ${esc(panel.data.err)}</p>` : ''}
    <div class="sec"><fieldset>
      ${field('name', 'Nome', `<input name="name" value="${esc(v.name)}" ${isNew ? 'required' : 'disabled'} spellcheck="false" pattern="[a-z0-9][a-z0-9_-]*">`,
        isNew ? 'Minúsculas, números, - e _. Não muda depois: entra nos nomes dos projetos e dos snapshots.' : 'Não muda. Para outro nome, remove e adiciona.')}
      ${field('icon', 'Ícone', `<span class="icon-url"><span class="svc-icon">${isNew ? icon('cube') : svcIcon(s.name)}</span>
        <input name="icon" value="${esc(v.icon)}" placeholder="${esc(v.name || 'speedtest-tracker')}" spellcheck="false" autocapitalize="off"></span>`,
        'O nome de um ícone do <a href="https://dashboardicons.com" target="_blank" rel="noopener">dashboardicons.com</a> (por exemplo speedtest-tracker), ou o link da sua página. O agente descarrega-o uma vez e guarda-o; a página nunca vai à internet. Vazio: o do nome do serviço ou da pasta, se existir.')}
    </fieldset></div>
    <div class="sec"><h3>No failover</h3><fieldset ${home ? '' : 'disabled'}>
      ${home ? '' : '<p class="note-text">A pasta, o endereço, o override e o IP só mudam com o serviço no servidor.</p>'}
      ${field('dir', 'Pasta no espelho', `<select name="dir">${dirs.length ? '' : '<option value="">Nenhuma pasta com docker-compose.yml</option>'}${dirs.map(d => `<option ${d === v.dir ? 'selected' : ''}>${esc(d)}</option>`).join('')}</select>`)}
      ${field('host', 'Endereço', `<input name="host" value="${esc(v.host)}" placeholder="nome.${esc(st.dns_zone)}" spellcheck="false">`, 'O nome que os clientes usam. No failover passa a apontar para o TNAS.')}
      ${field('override_yaml', 'Override (opcional)', `<textarea name="override_yaml" rows="6" spellcheck="false" placeholder="services:&#10;  nome-no-compose:&#10;    profiles: [&quot;disabled&quot;]">${esc(panel.data.override)}</textarea>`,
        'Junta-se ao compose do espelho no failover, por exemplo para desligar um contentor. É verificado com docker compose antes de guardar.')}
      ${field('require_free_ip', 'IP que tem de estar livre (opcional)', `<input name="require_free_ip" value="${esc(v.require_free_ip)}" spellcheck="false">`, 'Para serviços com IP próprio na LAN: a cópia não arranca se alguém já o usa.')}
    </fieldset></div>
    <div class="sec"><h3>Quando</h3><fieldset>
      ${field('wait_min', 'Espera antes do failover', `<select name="wait_min">${optMinutes(v.wait_min, 1)}</select>`)}
      ${field('stability_min', 'Estabilidade antes do regresso', `<select name="stability_min">${optMinutes(v.stability_min, 0)}</select>`)}
    </fieldset></div>
    <p class="note-text" id="svcDisc">${pre ? 'Preenchido com o que a descoberta encontrou: revê antes de adicionar.' : ''}</p>
    ${notesHtml(discOf(v.dir)?.notes)}
    <p class="bad-text sec-err" id="svcErr" role="alert"></p>
  </form>`;
  const foot = `${isNew ? '' : `<button type="button" class="btn danger" data-remove="${esc(s.name)}" ${home ? '' : 'disabled title="Só com o serviço no servidor"'}>Remover serviço</button>`}
    <span class="dlg-foot" style="margin:0 0 0 auto">${isNew ? '<button type="button" class="btn" data-close>Cancelar</button>'
      : `<button type="button" class="btn" data-panel="svc" data-svc="${esc(s.name)}">Cancelar</button>`}
    <button type="submit" form="svcForm" class="btn contained" id="svcSave">${isNew ? 'Adicionar' : 'Guardar'}</button></span>`;
  return {head, body, foot};
}
export async function saveService(f) {
  const isNew = !panel.name, name = isNew ? f.elements.name.value.trim() : panel.name;
  const body = {new: isNew, name, dir: f.elements.dir.value, host: f.elements.host.value, wait_min: Number(f.elements.wait_min.value),
    stability_min: Number(f.elements.stability_min.value), icon: f.elements.icon.value, override_yaml: f.elements.override_yaml.value,
    require_free_ip: f.elements.require_free_ip.value};
  f.querySelectorAll('.field-err, .sec-err').forEach(x => { x.textContent = ''; });
  $('svcSave').disabled = true;
  try {
    const r = toLogin(await fetch('api/service', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)}));
    if (r.ok) {
      toast(isNew ? `Serviço ${name} adicionado` : 'Guardado');
      forgetDiscovery();
      await refresh();
      openPanel('svc', name);
      return;
    }
    const j = await readErr(r);
    const at = j.field && f.querySelector(`[data-k="${CSS.escape(j.field)}"] .field-err`);
    if (at) { at.textContent = sentence(j.error); at.closest('.set-field').scrollIntoView({block: 'nearest'}); }
    else $('svcErr').textContent = sentence(j.error);
  } catch (err) {
    $('svcErr').textContent = sentence(err.message);
  }
  $('svcSave').disabled = false;
}
export async function removeService(name) {
  if (!await confirmAction(`Remover ${name}?`, 'Sai da configuração e do estado. O override, se houver, fica na pasta dos overrides. Os eventos ficam no histórico.', 'Remover')) return;
  try {
    const r = toLogin(await fetch('api/service/remove', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({name})}));
    if (!r.ok) { toast(`Não foi possível: ${(await readErr(r)).error}`, false); return; }
  } catch (err) {
    toast(`Não foi possível: ${err.message}`, false);
    return;
  }
  $('panel').close();
  toast(`Serviço ${name} removido`);
  refresh();
}
// Choosing a folder in a new service's form fills what is still empty with
// what the discovery found for it.
document.addEventListener('change', async e => {
  if (e.target.name !== 'dir' || !e.target.closest('#svcForm') || panel?.name) return;
  const f = e.target.form, x = (await getDiscovery()).services.find(y => y.dir === e.target.value);
  const auto = JSON.parse(f.dataset.auto || '{}'); // what the discovery filled for the folder before
  for (const [k, v] of Object.entries(auto)) if (f.elements[k].value === v) f.elements[k].value = '';
  f.dataset.auto = '{}';
  if (!x) { $('svcDisc').textContent = ''; return; }
  const filled = {};
  const fill = (k, v) => { if (v && !f.elements[k].value.trim()) { f.elements[k].value = v; filled[k] = v; return true; } return false; };
  const got = [fill('name', x.name) && 'nome', fill('host', x.host) && 'endereço', fill('override_yaml', x.override_yaml) && 'override',
    fill('require_free_ip', x.require_free_ip) && 'IP'].filter(Boolean);
  f.dataset.auto = JSON.stringify(filled);
  $('svcDisc').textContent = got.length ? `Da descoberta: ${got.join(', ')}. ${(x.notes || []).map(sentence).join(' ')}` : '';
});
document.addEventListener('submit', e => {
  if (e.target.id !== 'svcForm') return;
  e.preventDefault();
  saveService(e.target);
});
document.addEventListener('click', e => {
  if (e.target.closest('[data-close]')) { $('panel').close(); return; }
  const rm = e.target.closest('[data-remove]');
  if (rm) removeService(rm.dataset.remove);
});

export function svcPanel(s) {
  if (!s) return {head: '', body: ''};
  const S = look(s);
  const events = eventsNow().filter(e => e.svc === s.name).slice(0, 25);
  return {
    head: `<span class="svc-icon" style="--accent:${S.c}">${svcIcon(s.name)}</span>
      <div class="node-id"><h2 id="panelTitle">${esc(s.name)}</h2>${mono(s.host)}</div>${chip(S.label, S.c, S.busy ? 'busy' : '')}`,
    body: `<div style="--accent:${S.c}">${sec('Agora', statusBlock(s))}</div>`
      + sec('Configuração', kv([
        ['Pasta no espelho', mono(s.dir)],
        ['Override', s.override ? mono(s.override) : 'Nenhum, usa o compose tal como está'],
        s.require_free_ip && ['IP que tem de estar livre', mono(s.require_free_ip)],
        ['Espera antes do failover', minuteSelect(s, 'wait_min', 1)],
        ['Estabilidade antes do regresso', minuteSelect(s, 'stability_min', 0)],
        ['Manutenção', s.maint_until ? `Ligada até ${esc(clock(s.maint_until))}` : st.maint_until ? 'Global ligada' : 'Desligada'],
      ]), `<button class="btn" data-panel="svcEdit" data-svc="${esc(s.name)}">${icon('wrench')}Editar</button>`)
      + (s.snapshot || s.dns ? sec('Cópia no TNAS', kv([
        s.snapshot && ['Snapshot', mono(s.snapshot)],
        ['DNS', s.dns ? `${mono(s.host)} aponta para o TNAS` : 'Sem mudança'],
        s.since && ['Neste estado desde', esc(when(s.since))],
      ])) : '')
      + (s.steps?.length ? sec(s.state === 'FAILING_OVER' ? 'Failover em curso' : 'Último failover', stepsHtml(s)) : '')
      + sec('Últimas verificações no servidor', beatsBlock(s.name))
      + sec('Imagens', imageList(s.name))
      + sec('Eventos', events.length ? `<ol class="timeline">${events.map(e => evItem(e, false, false)).join('')}</ol>` : '<p class="note-text">Ainda sem eventos deste serviço.</p>'),
    foot: actions(s),
  };
}
export function renderPanel() {
  if (!panel || !st) return;
  if (panel.kind === 'svcEdit' && panel.drawn) return; // the form is drawn once: the refresh never undoes what is typed
  const found = st.services.find(s => s.name === panel.name);
  if (panel.kind === 'svc' && !found) { $('panel').close(); return; } // removed meanwhile
  const p = panel.kind === 'server' ? serverPanel() : panel.kind === 'tnas' ? tnasPanel()
    : panel.kind === 'svcEdit' ? svcEditPanel() : svcPanel(found);
  paint('panelHead', p.head);
  // a list being chosen from must not be replaced under the pointer by the refresh
  const choosing = document.activeElement?.tagName === 'SELECT' && $('panelBody').contains(document.activeElement);
  if (!choosing) paint('panelBody', p.body);
  paint('panelFoot', p.foot || '');
  $('panelFoot').hidden = !p.foot;
  if (panel.kind === 'svcEdit' && panel.data) panel.drawn = true;
  fillBeats();
}


$('panel').addEventListener('close', () => { panel = null; });
$('panel').addEventListener('click', e => { if (e.target === e.currentTarget) e.currentTarget.close(); }); // click on the backdrop
$('panelClose').addEventListener('click', () => $('panel').close());
