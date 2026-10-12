// The first-start guide.
import {$, esc, icon, mono, painted, plural, readErr, sentence, st, toLogin, toast} from './core.js';
import {NET_KEYS, SECTIONS, disc, discErr, discListHtml, discOf, discWaiting, discing, fieldHtml, forgetDiscovery, getDiscovery, paintGuarded, protectPicked} from './settings.js';
import {currentTab, refresh} from './main.js';

// The first start: one screen with what the agent found, to confirm and
// complete. Começar saves every block in order and says how each went; the
// blocks that failed keep what was typed.
export const MAIL = {gmail: {host: 'smtp.gmail.com', port: 587, security: 'starttls'}, outlook: {host: 'smtp-mail.outlook.com', port: 587, security: 'starttls'}};
export const setupRes = {}; // block → its result after Começar
export const SU = ['rede', 'paths', 'tech', 'mail', 'svcs'];
export const PATH_KEYS = () => SECTIONS.find(x => x.id === 'caminhos').fields;
export const suForm = id => document.querySelector(`.su-form[data-su="${id}"]`);
export function mailPreset() {
  const h = st.settings['email.host'];
  return !h ? 'gmail' : Object.keys(MAIL).find(k => MAIL[k].host === h) || 'outro';
}
export const byKey = k => SECTIONS.flatMap(x => x.fields).find(fd => fd.k === k);
export function suBlock(id, n, title, intro, body) {
  return `<h2><span class="wiz-n">${n}</span>${esc(title)}</h2><p class="note-text wiz-intro">${intro}</p>
    ${body}<div id="sures-${id}">${setupRes[id] || ''}</div>`;
}
export function setupHtml(id) {
  switch (id) {
    case 'rede': {
      // a new install proposes what was found; once set up, what is saved
      const found = k => st.setup_pending && disc?.network[NET_KEYS.find(x => x[1] === k)?.[0]] || st.settings[k];
      const fields = SECTIONS[0].fields.map(fd => fieldHtml(fd, found(fd.k))).join('') + fieldHtml({k: 'dns.zone', l: 'Zona DNS', h: 'Vazia: a que o Technitium tiver para os domínios do NPM.'}, st.setup_pending && disc?.dns.zone || st.settings['dns.zone']);
      return suBlock(id, 1, 'Rede e servidor', 'O router e a rede vêm do TNAS; o servidor, dos proxy hosts do NPM no espelho. Confirma ou corrige.',
        `${!disc ? discWaiting('A procurar…') : ''}<form class="su-form" data-su="rede" novalidate>${fields}</form>`);
    }
    case 'paths': {
      const where = !disc ? '' : disc.mirror.found ? `<p class="ok-text wiz-done" style="margin:0 0 8px">${icon('check')}<span>Encontrei o espelho em ${mono(disc.mirror.root)}, com ${plural(disc.services.length, 'pasta', 'pastas')} com docker-compose.yml.</span></p>`
        : `<p class="bad-text wiz-done" style="margin:0 0 8px">${icon('alert')}<span>Não encontrei o espelho em ${mono(disc.mirror.root)}: corrige o subvolume e a pasta.</span></p>`;
      return suBlock(id, 2, 'Caminhos', 'Onde o backup do TOS deixa o espelho do servidor, e onde ficam os snapshots de um failover (no mesmo volume btrfs do espelho). Os de fábrica servem um TNAS com o backup em /Volume1/ServerBackup.',
        `${where}<form class="su-form" data-su="paths" novalidate>${PATH_KEYS().map(fd => fieldHtml(fd)).join('')}</form>`);
    }
    case 'tech':
      return suBlock(id, 3, 'Technitium', 'Num failover o agente aponta o nome do serviço para o TNAS. Cria um token da API no Technitium (Administração → Sessões → Criar token, para um utilizador com direitos na zona) e cola-o aqui.',
        `${st.dns_token ? `<p class="ok-text wiz-done" style="margin:0 0 8px">${icon('check')}O agente já tem um token. Para outro, cola outro.</p>` : ''}
        <form class="su-form" data-su="tech"><label class="pw-field">Token da API do Technitium <input type="password" name="ttoken" autocomplete="off" spellcheck="false"></label></form>
        <p class="note-text">O Technitium em ${mono(st.settings['dns.api_url'])}. Outro endereço: <a href="#/definicoes/dns">Definições → DNS</a>.</p>`);
    case 'mail': {
      const pre = mailPreset(), opt = (v, l) => `<option value="${v}" ${v === pre ? 'selected' : ''}>${l}</option>`;
      const f = (k, over = {}) => fieldHtml({...byKey(k), ...over});
      return suBlock(id, 4, 'Avisos por email', 'Um email quando um serviço passa para o TNAS, volta, dá erro, ou o próprio agente falha.',
        `<form class="su-form" data-su="mail" novalidate>
          <label class="set-field"><span class="set-label">Email</span><span class="set-input"><select name="preset">
            ${opt('gmail', 'Gmail')}${opt('outlook', 'Outlook')}${opt('outro', 'Outro servidor')}${opt('none', 'Sem avisos')}</select></span></label>
          <p class="note-text" data-for="gmail">No Gmail a password é uma <strong>password de aplicação</strong>, não a da conta: com a verificação em 2 passos ligada,
            cria uma em <a href="https://myaccount.google.com/apppasswords" target="_blank" rel="noopener">myaccount.google.com/apppasswords</a> e cola aqui as 16 letras.</p>
          <p class="note-text" data-for="outlook">No Outlook, com a verificação em 2 passos ligada, usa uma password de aplicação da conta Microsoft.</p>
          <div class="su-form" data-for="outro">${f('email.host', {h: ''})}${f('email.port')}${f('email.security')}</div>
          <div class="su-form" data-for="gmail outlook outro">${f('email.user', {l: 'O teu email', h: ''})}${f('email.password', {h: ''})}
            ${f('email.to', {h: 'Vazio: para o teu email.'})}</div>
          <p class="note-text" data-for="none">Os eventos ficam só na interface.</p></form>`);
    }
    case 'svcs': {
      const intro = 'As pastas do espelho com docker-compose.yml, com o endereço que o NPM lhes dá. Escolhe as que passam para o TNAS quando o servidor falha.';
      if (!disc) return suBlock(id, 5, 'Serviços', intro, discWaiting('A procurar no espelho…'));
      if (!disc.mirror.found) return suBlock(id, 5, 'Serviços', intro, '<p class="note-text">Sem o espelho não há serviços: corrige os Caminhos e carrega em Começar.</p>');
      return suBlock(id, 5, 'Serviços', intro, `${disc.npm.found ? '' : '<p class="note-text">Não encontrei os proxy hosts do NPM no espelho: os endereços ficam por preencher.</p>'}
        ${discListHtml(disc, true)}<div class="sec-btns"><button class="btn" data-panel="svcEdit">+ Adicionar à mão</button></div>`);
    }
  }
}
// shows the fields of the chosen email
export function showPreset(form) {
  const v = form?.elements.preset?.value;
  form?.querySelectorAll('[data-for]').forEach(x => { x.hidden = !x.dataset.for.split(' ').includes(v); });
}
export function renderSetup() {
  if (currentTab() !== 'setup') return;
  if (!disc && !discErr && !discing) getDiscovery().then(repaintSetup, repaintSetup);
  SU.forEach(id => paintGuarded(`su-${id}`, setupHtml(id)));
  showPreset(suForm('mail'));
}
export function repaintSetup() { SU.forEach(id => { painted[`su-${id}`] = null; }); renderSetup(); }
document.addEventListener('input', e => { e.target.closest('.su-form')?.closest('.set-sec').querySelector('.su-form').setAttribute('data-dirty', ''); });
document.addEventListener('change', e => { if (e.target.name === 'preset') showPreset(e.target.form); });

export async function startSetup() {
  const btn = $('suStart');
  btn.disabled = true;
  let bad = 0;
  const res = (id, ok, msg) => {
    if (ok === false) bad++;
    else suForm(id)?.removeAttribute('data-dirty'); // saved: the refresh may repaint it
    setupRes[id] = `<p class="${ok ? 'ok-text' : ok === false ? 'bad-text' : 'note-text'} wiz-done">${ok == null ? '' : icon(ok ? 'check' : 'alert')}<span>${esc(sentence(msg))}</span></p>`;
    $(`sures-${id}`).innerHTML = setupRes[id];
  };
  // a failed answer with a field shows beside it too
  const send = async (id, path, body) => {
    const r = toLogin(await fetch(path, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)}));
    if (r.ok) return r;
    const j = await readErr(r), at = j.field && suForm(id)?.querySelector(`[data-k="${CSS.escape(j.field)}"] .field-err`);
    if (at) at.textContent = sentence(j.error);
    throw new Error(j.field ? `${byKey(j.field)?.l || (j.field === 'dns.zone' ? 'Zona DNS' : j.field)}: ${j.error}` : j.error);
  };
  const val = (id, k) => suForm(id)?.elements[k]?.value.trim() ?? '';
  // the picks now: a login below makes the discovery stale and a refresh may repaint the list
  const picked = [...$('su-svcs').querySelectorAll('[data-pick]:checked')].map(x => discOf(x.dataset.pick)).filter(Boolean);
  document.querySelectorAll('#page-setup .field-err').forEach(x => { x.textContent = ''; });
  try {
    // Technitium first: the zone comes with its token
    const ttoken = suForm('tech').elements.ttoken;
    if (ttoken.value.trim()) {
      try {
        const r = await send('tech', 'api/dns', {token: ttoken.value});
        const {warning} = await r.json().catch(() => ({}));
        ttoken.value = '';
        forgetDiscovery();
        res('tech', true, `Token testado e guardado${warning ? `; ${warning}` : ''}`);
      } catch (x) { res('tech', false, x.message); }
    } else {
      res('tech', st.dns_token, st.dns_token ? 'O agente já tinha um token' : 'Falta o token: sem ele o DNS não muda num failover');
    }

    // the paths before the network: the server is found in the mirror
    const paths = {};
    for (const fd of PATH_KEYS()) if (val('paths', fd.k) !== String(st.settings[fd.k] ?? '')) paths[fd.k] = val('paths', fd.k);
    const moved = Object.keys(paths).length > 0;
    try {
      if (moved) {
        await send('paths', 'api/config/section', {section: 'caminhos', values: paths});
        forgetDiscovery();
      }
      const r = toLogin(await fetch('api/config/check', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({section: 'caminhos'})}));
      const bad = r.ok ? (await r.json()).filter(c => !c.ok).map(c => c.msg) : [];
      res('paths', !bad.length, bad.length ? bad.join('; ') : moved ? 'Caminhos guardados e confirmados' : 'Caminhos confirmados');
    } catch (x) { res('paths', false, x.message); }

    const rede = {};
    for (const [, k] of NET_KEYS) rede[k] = val('rede', k);
    const zone = val('rede', 'dns.zone') || (await getDiscovery(true).catch(() => null))?.dns.zone || '';
    try {
      await send('rede', 'api/config/section', {section: 'rede', values: rede});
      if (zone && zone !== st.settings['dns.zone']) await send('rede', 'api/config/section', {section: 'dns', values: {'dns.zone': zone}});
      res('rede', true, `Rede guardada${zone ? `, zona ${zone}` : ''}`);
    } catch (x) { res('rede', false, x.message); }

    const preset = suForm('mail').elements.preset.value, pw = suForm('mail').elements['email.password'];
    if (preset === 'none') {
      try {
        if (st.settings['email.host']) await send('mail', 'api/config/section', {section: 'avisos', values: {'email.host': ''}});
        res('mail', null, 'Sem avisos por email');
      } catch (x) { res('mail', false, x.message); }
    } else if (!val('mail', 'email.user') && preset !== 'outro') {
      res('mail', null, 'Sem avisos: não indicaste o teu email');
    } else {
      const p = MAIL[preset] || {host: val('mail', 'email.host'), port: Number(val('mail', 'email.port')), security: val('mail', 'email.security')};
      const values = {'email.host': p.host, 'email.port': p.port, 'email.security': p.security, 'email.user': val('mail', 'email.user'), 'email.to': val('mail', 'email.to')};
      if (pw.value) values['email.password'] = pw.value;
      try {
        await send('mail', 'api/config/section', {section: 'avisos', values});
        pw.value = '';
        suForm('mail').removeAttribute('data-dirty'); // saved, even if the test fails: the repaint shows it
        await send('mail', 'api/email/test', {});
        res('mail', true, `Email de teste enviado para ${values['email.to'] || values['email.user']}: vê se chegou`);
      } catch (x) { res('mail', false, x.message); }
    }

    if (moved) {
      res('svcs', false, 'Os caminhos mudaram: revê a lista de serviços e carrega outra vez em Começar');
    } else if (picked.length) {
      const {n, failed} = await protectPicked(picked);
      res('svcs', !failed.length, failed.length ? `Não consegui: ${failed.join('; ')}` : plural(n, 'serviço protegido', 'serviços protegidos'));
    } else {
      res('svcs', null, st.services.length ? plural(st.services.length, 'serviço protegido', 'serviços protegidos') : 'Nenhum escolhido: adiciona-os depois na Visão geral');
    }

    forgetDiscovery();
    await refresh();
    if (bad) { toast('Falta resolver o que está a vermelho', false); return; }
    await send('', 'api/setup/done', {});
    toast('Pronto: o agente começa em observação, sem agir. Passa a automático em Definições → Geral.');
    await refresh();
    location.hash = '#/';
  } catch (x) {
    toast(`Não foi possível: ${x.message}`, false);
  } finally {
    btn.disabled = false;
    repaintSetup();
  }
}
$('suStart').addEventListener('click', startSetup);
