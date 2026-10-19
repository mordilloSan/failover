// The settings page and the discovery.
import {$, cap, chip, confirmAction, day, esc, icon, kv, mono, paint, painted, pending, plural, post, readErr, sentence, st, toLogin, toast} from './core.js';
import {repaintSetup} from './setup.js';
import {currentTab, hashParts, refresh} from './main.js';

// Settings, as a page. Only values that rarely change go in the markup, so
// the 5 s refresh does not repaint it while the token is being typed.
// The sections with a form each: key (as in failover.yml), label, and n (a
// number with that minimum), u (unit), s (secret), t (input type), h (help).
export const SECTIONS = [
  {id: 'rede', title: 'Rede', locked: true, fields: [
    {k: 'server.ip', l: 'IP do servidor'},
    {k: 'server.npm_check_host', l: 'Endereço do NPM do servidor', h: 'O nome que o agente pede ao NPM do servidor em cada verificação.'},
    {k: 'tnas_ip', l: 'IP do TNAS'},
    {k: 'router_ip', l: 'IP do router'},
    {k: 'lan_iface', l: 'Interface da LAN', h: 'Para o arping que confirma que um IP está livre antes de uma cópia o usar.'}]},
  {id: 'verificacao', title: 'Verificação', fields: [
    {k: 'start_timeout_min', l: 'Tempo para a cópia ficar pronta', u: 'min', n: 1, h: 'Depois disto sem responder, o failover dá erro.'},
    {k: 'npm.alert_after_min', l: 'Avisar do NPM em falha após', u: 'min', n: 0},
    {k: 'maintenance.default_expiry_min', l: 'Manutenção por defeito', u: 'min', n: 1}]},
  {id: 'dns', title: 'DNS (Technitium)', locked: true, fields: [
    {k: 'dns.api_url', l: 'URL da API'},
    {k: 'dns.zone', l: 'Zona'},
    {k: 'dns.ttl', l: 'TTL do registo', u: 's', n: 1}]},
  {id: 'caminhos', title: 'Caminhos', locked: true, fields: [
    {k: 'paths.mirror_subvol', l: 'Subvolume do espelho'},
    {k: 'paths.mirror_root', l: 'Pasta dentro do espelho'},
    {k: 'paths.snapshots_dir', l: 'Pasta dos snapshots'},
    {k: 'paths.overrides_dir', l: 'Pasta dos overrides'},
    {k: 'npm.dir', l: 'Pasta do NPM no espelho'},
    {k: 'paths.mirror_stale_days', l: 'Avisar do espelho parado ao fim de', u: 'dias', n: 0, h: 'Sem mudanças no espelho durante estes dias: o backup do TOS parou? 0: 2 dias.'}]},
  {id: 'avisos', title: 'Avisos por email', fields: [
    {k: 'email.host', l: 'Servidor SMTP', h: 'Gmail: smtp.gmail.com · Outlook: smtp-mail.outlook.com. Vazio desliga os avisos.'},
    {k: 'email.port', l: 'Porta', n: 1},
    {k: 'email.security', l: 'Ligação', o: ['starttls', 'tls', 'none'], h: 'starttls na porta 587, tls na 465, none num relay da LAN sem login.'},
    {k: 'email.user', l: 'Utilizador', h: 'O email da conta.'},
    {k: 'email.password', l: 'Password', s: true, h: 'No Gmail, uma password de aplicação.'},
    {k: 'email.from', l: 'Remetente', h: 'Vazio: o utilizador.'},
    {k: 'email.to', l: 'Enviar para', h: 'Vazio: o utilizador.'}]},
  {id: 'noturna', title: 'Descarga noturna', fields: [
    {k: 'nightly.prepull_at', l: 'Hora', t: 'time', h: 'Descarrega as imagens em falta ou desatualizadas dos composes do espelho, a esta hora e ao arrancar o agente, e apaga as que substitui. Vazio desliga.'}]},
  {id: 'interface', title: 'Interface', fields: [
    {k: 'ui.listen', l: 'Endereço e porta', h: 'Por exemplo 0.0.0.0:8099. Muda depois de reiniciar o agente.'}]},
];
export const CHECKED = ['rede', 'dns', 'caminhos'];
export const secChecks = {}; // section → the checks shown after it was saved
// nothing of it runs on the TNAS: a failed failover counts once its copy is gone (as home() in Go)
export const atHome = s => s.state === 'NORMAL' || (s.state === 'ERROR' && !s.snapshot && !s.dns);
export const allHome = () => st.services.every(atHome) && !st.tnas_npm?.snapshot;

export function fieldHtml(fd, v = st.settings?.[fd.k]) {
  const input = fd.o ? `<select name="${fd.k}">${fd.o.map(o => `<option ${o === v ? 'selected' : ''}>${esc(o)}</option>`).join('')}</select>`
    : fd.s ? `<input type="password" name="${fd.k}" autocomplete="new-password" placeholder="${v ? 'Deixa vazio para manter' : 'Em falta'}">`
    : fd.n != null ? `<input type="number" name="${fd.k}" min="${fd.n}" value="${esc(v)}">`
    : `<input type="${fd.t || 'text'}" name="${fd.k}" value="${esc(v)}" spellcheck="false" autocomplete="off">`;
  return `<label class="set-field" data-k="${fd.k}">
    <span class="set-label">${esc(fd.l)}${fd.s ? chip(v ? 'Definido' : 'Em falta', v ? 'var(--success)' : 'var(--warning)', 'xs') : ''}</span>
    <span class="set-input">${input}${fd.u ? `<span class="unit">${fd.u}</span>` : ''}</span>
    ${fd.h ? `<small class="set-help">${esc(fd.h)}</small>` : ''}<small class="field-err" role="alert"></small></label>`;
}

export function interfaceExtra() {
  const c = st.cert || {}, run = st.ui_listen_running, saved = st.settings?.['ui.listen'];
  return kv([['Certificado', c.names?.length ? esc(c.names.join(', ')) : '—'],
      ['Válido até', c.not_after ? `${esc(day(c.not_after))}, renovado sozinho` : '—']])
    + (run && saved && run !== saved ? `<div class="banner" style="--c:var(--warning)">${icon('restore')}<div>Guardado ${mono(saved)}, mas a interface ainda está em ${mono(run)}.
        <button type="button" class="btn" data-restart>Reiniciar agora</button></div></div>` : '');
}

export function sectionHtml(sec) {
  const lock = sec.locked && !allHome();
  return `<h2>${esc(sec.title)}</h2>
    <form class="set-form" data-section="${sec.id}" novalidate>
      <fieldset ${lock ? 'disabled' : ''}>${sec.fields.map(fd => fieldHtml(fd)).join('')}</fieldset>
      ${lock ? '<p class="note-text">Só com todos os serviços no servidor e o NPM do TNAS parado.</p>' : ''}
      ${sec.id === 'interface' ? interfaceExtra() : ''}
      <p class="bad-text sec-err" role="alert"></p>
      <div class="form-foot">${sec.id === 'avisos' ? `<button type="button" class="btn" data-mail-test>${icon('email')}Enviar email de teste</button><span class="spacer"></span>` : ''}${sec.id === 'rede' ? `<button type="button" class="btn" data-detect title="Preenche com o que o agente encontrou; não guarda">${icon('refresh')}Detetar</button><span class="spacer"></span>` : ''}
        <button type="button" class="btn" data-reset disabled>Repor</button>
        <button type="submit" class="btn contained" data-save disabled>Guardar</button></div>
      <ul class="checks">${secChecks[sec.id] || ''}</ul>
    </form>
    ${sec.id === 'dns' ? `<div class="sec-sub">${kv([['Token', st.dns_token ? chip('Definido', 'var(--success)', 'xs') : chip('Em falta', 'var(--warning)', 'xs')]])}
      <form class="token-form"><label class="pw-field">${st.dns_token ? 'Substituir o token' : 'Token da API do Technitium'}
          <input type="password" class="dns-token" autocomplete="off" required>
          <small class="set-help">Cria-o no Technitium em Administração → Sessões → Criar token, para um utilizador com direitos na zona.</small></label>
        <p class="bad-text token-err" role="alert"></p>
        <div class="form-foot"><button type="submit" class="btn contained">Testar e guardar</button></div></form></div>
    ${failoverAppHtml()}` : ''}`;
}

// The zone's * as a record of the Technitium's Failover app: what the readiness
// found wrong with it, and the button that writes it.
export function failoverAppHtml() {
  const fixes = (st.verdict?.problems || []).filter(p => p.includes('Definições → DNS'));
  const busy = pending.has('failoverApp');
  return `<div class="sec-sub"><p class="note-text">O ${mono('*.' + st.dns_zone)} é um registo da app Failover do Technitium: aponta para o servidor
      (${mono(st.server_ip)}) enquanto o NPM dele responde na porta 443, e para o TNAS (${mono(st.tnas_ip)}) quando não responde.</p>
    ${!st.verdict?.at ? '' : fixes.length ? `<ul class="checks">${fixes.map(p => `<li class="warn-text">${icon('alert')}<span>${esc(sentence(p))}</span></li>`).join('')}</ul>`
      : `<p class="ok-text">${icon('check')} A app está instalada e o ${mono('*.' + st.dns_zone)} está como deve.</p>`}
    <div class="form-foot"><button type="button" class="btn${busy ? ' busy' : ''}" data-failover-app ${busy ? 'disabled' : ''}>${icon('dns')}Configurar no Technitium</button></div></div>`;
}

// Settings, as a page, one card per section. A section with changes not yet
// saved, or with the focus in it, is left alone by the 5 s refresh.
export const INTERVALS = [10, 15, 30, 60, 120, 300];
export const secs = v => v % 60 ? `${v} s` : `${v / 60} min`;
// A form with changes not saved, a half-typed token or the focus inside is
// left alone by the refresh.
export function paintGuarded(elId, html) {
  const el = $(elId), f = document.activeElement;
  const typing = f && el.contains(f) && f.matches('input, select, textarea'); // a button keeps the focus after a dialog: no reason to wait
  if (el.querySelector('form[data-dirty]') || [...el.querySelectorAll('input[type=password]')].some(x => x.value) || typing) return;
  paint(elId, html);
}
export const paintSec = (id, html) => paintGuarded(`set-${id}`, html);
export function repaintSec(id) { painted[`set-${id}`] = null; renderSettings(); repaintSetup(); }
// One section at a time: the one in the address (#/definicoes/dns), Geral by default.
export function showSec() {
  const sub = $(`set-${hashParts()[1]}`) ? hashParts()[1] : 'geral';
  document.querySelectorAll('.set-sec').forEach(s => { s.hidden = s.id !== `set-${sub}`; });
}
export function renderSettings() {
  if (!$('set-geral')) {
    $('settingsBody').innerHTML = ['geral', 'servicos', ...SECTIONS.map(x => x.id), 'conta'].map(id => `<section class="card set-sec" id="set-${id}"></section>`).join('');
    showSec();
  }
  const mode = pending.has('mode') ? pending.get('mode') : st.mode;
  const iv = pending.has('interval') ? pending.get('interval') : st.check_interval_s;
  const ivs = [...new Set([...INTERVALS, iv])].sort((a, b) => a - b);
  paintSec('geral', `<h2>Geral</h2>
      <fieldset class="seg" ${pending.has('mode') ? 'disabled' : ''}><legend>Modo</legend>
        <label><input type="radio" name="mode" value="observe" ${mode === 'observe' ? 'checked' : ''}><strong>Observação</strong><small>Só regista o que faria</small></label>
        <label><input type="radio" name="mode" value="auto" ${mode === 'auto' ? 'checked' : ''}><strong>Automático</strong><small>Faz failover e regresso sozinho</small></label>
      </fieldset>
      <label class="field">Verificar o servidor a cada <select id="interval" ${pending.has('interval') ? 'disabled' : ''}>
        ${ivs.map(v => `<option value="${v}" ${v === iv ? 'selected' : ''}>${secs(v)}</option>`).join('')}</select></label>`);
  if (currentTab() === 'settings' && !disc && !discErr && !discing) getDiscovery().then(() => repaintSec('servicos'), () => repaintSec('servicos'));
  const discNew = disc?.services.filter(x => !x.configured && !x.ignored) || [];
  paintSec('servicos', `<h2>Serviços</h2>
      <p class="note-text wiz-intro">As pastas do espelho com docker-compose.yml, com o endereço que o NPM lhes dá. "Adicionar" abre o formulário já preenchido; um serviço que não está no espelho adiciona-se à mão.</p>
      <div class="sec-btns" style="margin-bottom:8px"><button type="button" class="btn contained" data-panel="svcEdit" aria-haspopup="dialog">${icon('plus')}Adicionar à mão</button></div>
      ${!disc ? discWaiting('A procurar no espelho…') : `${disc.npm.found ? '' : '<p class="note-text">Não encontrei os proxy hosts do NPM no espelho: os endereços ficam por preencher.</p>'}
        ${discListHtml(disc, false)}
        <div class="sec-btns"><span class="muted">${discNew.length ? plural(discNew.length, 'por proteger', 'por proteger') : 'Todos protegidos'}</span>
          <button type="button" class="btn" data-rediscover>${icon('refresh')}Procurar outra vez</button></div>`}`);
  SECTIONS.forEach(x => paintSec(x.id, sectionHtml(x)));
  paintSec('conta', `<h2>Conta</h2>
      ${kv([['Utilizador', mono(st.user)]])}
      <div class="sec-btns"><button class="btn" data-pw>${icon('key')}Mudar password</button></div>
    <div class="sec-sub"><p class="note-text">Para um painel como o Homepage: ${mono('GET ' + location.origin + '/api/')} com o cabeçalho
        ${mono('Authorization: Bearer <token>')}. Só lê o resumo; nada muda com ele.</p>
      ${kv([['Token da API', st.api_token ? chip('Definido', 'var(--success)', 'xs') : chip('Em falta', 'var(--warning)', 'xs')]])}
      ${apiToken ? `<label class="pw-field">Copia-o agora: não volta a ser mostrado<input readonly class="mono" value="${esc(apiToken)}" data-select></label>` : ''}
      <div class="sec-btns"><button type="button" class="btn" data-api-token>${icon('key')}${st.api_token ? 'Gerar outro' : 'Gerar token'}</button></div></div>`);
}

// The API token just made: shown until the page is reloaded, never sent again.
let apiToken = '';
async function makeApiToken() {
  if (st.api_token && !await confirmAction('Gerar outro token?', 'O token atual deixa de funcionar: o painel que o usa tem de receber o novo.', 'Gerar')) return;
  try {
    const r = toLogin(await fetch('api/token', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{}'}));
    if (!r.ok) throw new Error((await readErr(r)).error);
    apiToken = (await r.json()).token;
    toast(await navigator.clipboard?.writeText(apiToken).then(() => 'Token gerado e copiado', () => 'Token gerado') ?? 'Token gerado');
    refresh();
  } catch (err) {
    toast(sentence(err.message), false);
  }
}

// What the agent discovered (GET /api/discover): proposals only. It runs
// docker compose on every folder of the mirror, so it is asked on demand and
// kept a minute; saving something clears it.
export let disc = null, discAt = 0, discing = null, discGen = 0, discErr = '';
export function getDiscovery(force = false) {
  if (!force && disc && Date.now() - discAt < 60e3) return Promise.resolve(disc);
  const gen = discGen;
  discing ??= fetch('api/discover', {cache: 'no-store'}).then(async r => { if (!toLogin(r).ok) throw new Error((await readErr(r)).error); return r.json(); })
    .then(d => { if (gen === discGen) { disc = d; discAt = Date.now(); discErr = ''; } return d; }, err => { discErr = err.message; throw err; })
    .finally(() => { discing = null; });
  return discing;
}
// something was saved: what was discovered is stale, and a fetch already on
// its way must not bring it back
export const forgetDiscovery = () => { disc = null; discGen++; };
export const discWaiting = what => discErr ? `<p class="bad-text">Não consegui descobrir: ${esc(sentence(discErr))}</p><div class="sec-btns"><button class="btn" data-rediscover>Tentar outra vez</button></div>`
  : `<p class="note-text">${what}</p>`;
// the discovered service of a folder, if any
export const discOf = dir => disc?.services.find(x => x.dir === dir);

// The network settings the discovery finds: its key, the setting's key.
export const NET_KEYS = [['router_ip', 'router_ip'], ['tnas_ip', 'tnas_ip'], ['lan_iface', 'lan_iface'],
  ['server_ip', 'server.ip'], ['npm_check_host', 'server.npm_check_host']];

// The discovered services, to protect with one click each (or all at once).
export function discListHtml(d, pickable) {
  const ignored = d.services.filter(x => x.ignored);
  const rows = d.services.filter(x => !x.ignored).map(x => {
    const can = !x.configured && !x.error && x.host;
    const notes = [x.error, ...(x.notes || [])].filter(Boolean);
    return `<li class="disc-row ${x.configured ? 'is-done' : ''}">
      ${pickable ? `<input type="checkbox" data-pick="${esc(x.dir)}" ${can ? 'checked' : ''} ${x.configured || x.error ? 'disabled' : ''} aria-label="Proteger ${esc(x.name)}">` : ''}
      <span class="grow"><strong>${esc(x.name)}</strong> <span class="mono muted">${esc(x.host || 'sem endereço')}</span>
        ${notes.map(n => `<small class="disc-notes">${esc(sentence(n))}</small>`).join('')}</span>
      ${x.configured ? chip('Já protegido', 'var(--success)', 'xs') : ''}
      ${pickable ? '' : x.configured ? `<button class="btn" data-panel="svcEdit" data-svc="${esc(x.name)}">${icon('wrench')}Editar</button>`
        : `${x.host || !d.npm.found ? `<button class="btn" data-panel="svcEdit" data-prefill="${esc(x.dir)}">Adicionar</button>` : ''}<button class="btn" data-ignore="1" data-dir="${esc(x.dir)}" title="Uma pasta antiga, de um container que já não existe: sai da lista e a vigia do espelho deixa de avisar por ela">Ignorar</button>`}</li>`;
  }).join('');
  // folders left out by hand (an old container's, still in the backup), out of the way
  const away = pickable || !ignored.length ? '' : `<details class="disc-ignored"><summary class="muted">${plural(ignored.length, 'pasta ignorada', 'pastas ignoradas')}</summary>
    <ul class="list">${ignored.map(x => `<li><span class="grow mono">${esc(x.dir)}</span><button class="btn" data-ignore="0" data-dir="${esc(x.dir)}">Deixar de ignorar</button></li>`).join('')}</ul></details>`;
  return `<ul class="list disc-list">${rows || '<li class="empty">Não encontrei nenhuma pasta com docker-compose.yml no espelho.</li>'}</ul>${away}`;
}
// Adds the discovered services picked; says how many and what failed.
export async function protectPicked(picked) {
  const failed = [];
  for (const x of picked) {
    const r = toLogin(await fetch('api/service', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({
      new: true, name: x.name, dir: x.dir, host: x.host, wait_min: 2, stability_min: 2, icon: '', override_yaml: x.override_yaml || '',
      require_free_ip: x.require_free_ip || ''})}));
    if (!r.ok) failed.push(`${x.name}: ${(await readErr(r)).error}`);
  }
  return {n: picked.length - failed.length, failed};
}
document.addEventListener('click', async e => {
  const ign = e.target.closest('button[data-ignore]');
  if (ign) {
    const {dir} = ign.dataset, ignore = ign.dataset.ignore === '1';
    ign.disabled = true;
    if (await post('api/mirror/ignore', {dir, ignore}, ignore ? `Pasta ${dir} ignorada` : `Pasta ${dir} de volta à descoberta`, 'ignore')) {
      const x = disc?.services.find(s => s.dir === dir);
      if (x && ignore) x.ignored = true;
      else forgetDiscovery(); // an ignored folder was never read: discover it again
    }
    painted['set-servicos'] = null;
    renderSettings();
    return;
  }
  if (e.target.closest('[data-rediscover]')) {
    discErr = '';
    forgetDiscovery();
    repaintSetup();
    painted['set-servicos'] = null;
    renderSettings();
    return;
  }
  const test = e.target.closest('[data-mail-test]');
  if (test) {
    if (test.closest('form[data-dirty]')) { toast('Guarda primeiro: o teste usa o que está guardado', false); return; }
    test.disabled = true;
    try {
      const r = toLogin(await fetch('api/email/test', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{}'}));
      if (!r.ok) throw new Error((await readErr(r)).error);
      toast('Email de teste enviado: vê se chegou');
    } catch (x) {
      toast(sentence(x.message), false);
    } finally {
      test.disabled = false;
    }
    return;
  }
  const det = e.target.closest('[data-detect]');
  if (det) detectInto(det.closest('form.set-form'));
  if (e.target.closest('[data-failover-app]')) {
    if (!await confirmAction(`Configurar o *.${st.dns_zone} no Technitium?`, `Instala a app Failover em cada Technitium que responda e guarda o *.${st.dns_zone} como registo da app: o servidor ${st.server_ip} enquanto o NPM dele responder na porta 443, o TNAS ${st.tnas_ip} quando não. Um registo A no * sai a seguir.`, 'Configurar')) return;
    await post('api/technitium/failover', {}, `*.${st.dns_zone} configurado no Technitium`, 'failoverApp');
    painted['set-dns'] = null;
    renderSettings();
  }
});
// Definições → Rede → Detetar: fills the form with what was found, not saved.
export async function detectInto(form) {
  const d = await getDiscovery(true);
  let n = 0;
  for (const [dk, sk] of NET_KEYS) {
    const el = form.elements[sk];
    if (el && d.network[dk] && el.value !== d.network[dk]) { el.value = d.network[dk]; n++; }
  }
  markSection(form);
  toast(n ? `${plural(n, 'valor detetado', 'valores detetados')}: revê e guarda` : 'Nada de novo: os valores são os detetados');
}
// A section's form: dirty when a field differs from what the agent has.
export function formDirty(form) {
  const sec = SECTIONS.find(x => x.id === form.dataset.section);
  return sec.fields.some(fd => {
    const el = form.elements[fd.k];
    return el && (fd.s ? el.value !== '' : String(el.value) !== String(st.settings?.[fd.k] ?? ''));
  });
}
export function markSection(form) {
  const d = formDirty(form);
  form.toggleAttribute('data-dirty', d);
  form.querySelector('[data-save]').disabled = !d;
  form.querySelector('[data-reset]').disabled = !d;
}
export async function saveSection(form) {
  const id = form.dataset.section, sec = SECTIONS.find(x => x.id === id), values = {};
  form.querySelectorAll('.field-err, .sec-err').forEach(x => { x.textContent = ''; });
  for (const fd of sec.fields) {
    const el = form.elements[fd.k];
    if (fd.s && !el.value) continue;
    if (fd.n != null && el.value.trim() === '') { // Number('') would save 0
      form.querySelector(`[data-k="${CSS.escape(fd.k)}"] .field-err`).textContent = 'Obrigatório.';
      el.focus();
      return;
    }
    values[fd.k] = fd.n != null ? Number(el.value) : el.value;
  }
  form.querySelector('[data-save]').disabled = true;
  try {
    const r = toLogin(await fetch('api/config/section', {method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({section: id, values})}));
    if (r.ok) {
      toast('Guardado');
      forgetDiscovery(); // the mirror, the network or the zone may be others now
      form.removeAttribute('data-dirty');
      document.activeElement?.blur();
      secChecks[id] = CHECKED.includes(id) ? '<li class="muted">A verificar…</li>' : '';
      await refresh();
      repaintSec(id);
      if (CHECKED.includes(id)) runChecks(id);
      return;
    }
    const j = await readErr(r);
    const at = j.field && form.querySelector(`[data-k="${CSS.escape(j.field)}"] .field-err`);
    if (at) at.textContent = sentence(j.error);
    else form.querySelector('.sec-err').textContent = sentence(j.error);
  } catch (err) {
    form.querySelector('.sec-err').textContent = sentence(err.message);
  }
  form.querySelector('[data-save]').disabled = false;
}
// After a save: the section's checks, as warnings that never undo it.
export async function runChecks(id) {
  let list = [];
  try {
    const r = toLogin(await fetch('api/config/check', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({section: id})}));
    if (r.ok) list = await r.json();
  } catch {}
  secChecks[id] = list.map(c => `<li class="${c.ok ? 'ok-text' : 'warn-text'}">${icon(c.ok ? 'check' : 'alert')}<span>${esc(sentence(c.msg))}</span></li>`).join('');
  repaintSec(id);
}
// Restart for a new address: Docker starts the agent again; open it where it now listens.
export async function restartAgent() {
  const port = String(st.settings['ui.listen']).split(':').pop();
  const target = `https://${location.hostname}:${port}`;
  if (!await confirmAction('Reiniciar o agente?', `A interface volta em ${target}. Uma verificação em curso acaba primeiro.`, 'Reiniciar')) return;
  const r = toLogin(await fetch('api/restart', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{}'}));
  if (!r.ok) { toast(`Não foi possível: ${(await readErr(r)).error}`, false); return; }
  toast('A reiniciar…');
  await new Promise(res => setTimeout(res, 3000));
  for (const end = Date.now() + 90e3; Date.now() < end; await new Promise(res => setTimeout(res, 2000))) {
    try { await fetch(`${target}/healthz`, {mode: 'no-cors', cache: 'no-store'}); location.href = `${target}/${location.hash}`; return; } catch {}
  }
  toast(`Não consegui confirmar o regresso em 90 s. Abre ${target} e, se o browser pedir, aceita outra vez o certificado (o Firefox pede-o por porta).`, false);
}
document.addEventListener('input', e => { const f = e.target.closest('form.set-form'); if (f) markSection(f); });
document.addEventListener('submit', e => {
  const f = e.target.closest('form.set-form');
  if (!f) return;
  e.preventDefault();
  saveSection(f);
});
document.addEventListener('click', e => {
  const reset = e.target.closest('[data-reset]');
  if (reset) {
    const f = reset.closest('form.set-form');
    f.removeAttribute('data-dirty');
    document.activeElement?.blur();
    repaintSec(f.dataset.section);
    return;
  }
  if (e.target.closest('[data-restart]')) restartAgent();
  if (e.target.closest('[data-api-token]')) makeApiToken();
  e.target.closest('[data-select]')?.select();
});
addEventListener('beforeunload', e => {
  if (document.querySelector('form.set-form[data-dirty]') || [...document.querySelectorAll('.dns-token')].some(x => x.value)) e.preventDefault();
});

// The token form is in the settings panel, drawn after this runs.
document.addEventListener('submit', async e => {
  const f = e.target.closest('.token-form');
  if (!f) return;
  e.preventDefault();
  const input = f.querySelector('.dns-token'), save = f.querySelector('[type=submit]'), err = f.querySelector('.token-err');
  save.disabled = true;
  err.textContent = '';
  try {
    const r = toLogin(await fetch('api/dns', {method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({token: input.value})}));
    if (!r.ok) throw new Error((await readErr(r)).error);
    const {warning} = await r.json().catch(() => ({}));
    input.value = '';
    input.blur();
    toast('Token testado e guardado');
    if (warning) toast(cap(warning), false);
    refresh();
  } catch (x) {
    err.textContent = sentence(x.message);
  } finally {
    save.disabled = false;
  }
});
