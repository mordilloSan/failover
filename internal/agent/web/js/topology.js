// The overview: the servers, the topology, its wires and telemetry, and the service pills.
import {$, EASE, ago, bytes, chip, clock, clockS, day, dur, esc, fine, icon, look, minsSince, mono, paint, painted, pending, plural, reduceMotion, sentence, st, svcIcon} from './core.js';

// What the card says under the name: the one thing worth knowing about the service now.
export function describe(s) {
  const maint = st.maint_until || s.maint_until;
  switch (s.state) {
    case 'NORMAL': {
      if (!st.router_ok) return {text: 'Sem verificação enquanto o router não responder.'};
      if (s.server_ok) return {text: 'A responder no servidor.'};
      if (maint) return {text: 'Em falha no servidor. A manutenção impede o failover.', tone: 'warning'};
      if (!s.fail_since) return {text: 'Em falha no servidor.', tone: 'warning'};
      const e = minsSince(s.fail_since), left = s.wait_min - e;
      if (left > 0) return {text: `Em falha no servidor há ${dur(e)}. O failover começa daqui a ${dur(left)}.`, pct: e / s.wait_min, tone: 'warning'};
      return {text: st.mode === 'auto' ? `Em falha no servidor. O failover começa na próxima verificação.`
        : `Em falha há ${dur(e)}. Em modo automático o failover já teria começado.`, pct: 1, tone: 'warning'};
    }
    case 'FAILING_OVER':
      return {text: sentence(s.msg) || `${nextStep(s)}.`, pct: minsSince(s.since) / st.start_timeout_min, tone: 'warning'};
    case 'ACTIVE': {
      if (s.forced) return {text: 'Failover forçado: fica no TNAS até Forçar regresso (ou até o arrastares para o servidor).'};
      if (!s.server_ok) return {text: 'A servir a partir do TNAS. Ainda não responde no servidor.'};
      const e = s.ok_since ? minsSince(s.ok_since) : 0, left = s.stability_min - e;
      if (left > 0) return {text: `Voltou a responder no servidor há ${dur(e)}. O regresso começa daqui a ${dur(left)}.`, pct: e / s.stability_min};
      return {text: st.mode === 'auto' ? `Estável no servidor. O regresso começa na próxima verificação.`
        : 'Estável no servidor. Em modo automático o regresso já teria começado.', pct: 1};
    }
    case 'RETURNING':
      return {text: sentence(s.msg) || 'A repor o DNS e a remover a cópia.', tone: 'warning'};
    default:
      return {text: `${sentence(s.msg) || 'O failover falhou.'} Volta ao normal quando o serviço responder no servidor.`, tone: 'error'};
  }
}

export function statusBlock(s) {
  const d = describe(s);
  const pct = d.pct == null ? null : Math.max(3, Math.min(100, d.pct * 100));
  return `<div class="status tone-${d.tone || 'ok'}"><p>${esc(d.text)}</p>
    ${pct == null ? '' : `<div class="bar" role="progressbar" aria-valuenow="${Math.round(pct)}" aria-valuemin="0" aria-valuemax="100"><span style="width:${pct}%"></span></div>`}</div>`;
}

// Images: what a failover of each stack needs, and whether the TNAS has it (O4).
export function imgCount(names) {
  const c = {known: !!st.images, unreadable: 0}, refs = new Map(); // stacks share images: count each once
  for (const n of names) {
    const stack = (st.images || {})[n];
    if (!stack) continue;
    if (stack.err) { c.unreadable++; continue; }
    for (const i of stack.images || []) refs.set(i.ref, i);
  }
  const imgs = [...refs.values()];
  c.total = imgs.length;
  c.present = imgs.filter(i => i.present).length;
  c.size = imgs.reduce((n, i) => n + (i.present ? i.size || 0 : 0), 0);
  c.missing = c.total - c.present;
  return c;
}
export const allStacks = () => ['npm', ...st.services.map(s => s.name)];

export function imageList(name) {
  const stack = (st.images || {})[name];
  if (!stack) return `<p class="note-text">${st.images_scanning ? 'A verificar…' : 'Ainda não verificado.'}</p>`;
  if (stack.err) return `<p class="bad-text">O compose não se deixou ler: ${esc(stack.err)}</p>`;
  if (!stack.images?.length) return '<p class="note-text">Este compose não pede imagens.</p>';
  return `<ul class="list">${stack.images.map(i => `<li>
      <span class="${i.present ? 'ok-text' : 'bad-text'}">${icon(i.present ? 'check' : 'alert')}</span>
      <span class="grow mono" title="${esc(i.ref)}">${esc(i.ref)}</span>
      ${i.no_pull ? `<span class="side" title="A descarga noturna não a traz nem a atualiza: ${esc(i.no_pull)}">${esc(i.no_pull)}</span>` : ''}
      ${i.present ? `<span class="side">${bytes(i.size)}</span><span class="side" title="Data da imagem">${day(i.created)}</span>`
        : '<span class="side bad-text">Em falta</span>'}</li>`).join('')}</ul>${notesHtml(stack.notes)}`;
}
export const notesHtml = notes => notes?.length ? `<ul class="checks">${notes.map(n => `<li class="warn-text">${icon('alert')}<span>${esc(sentence(n))}</span></li>`).join('')}</ul>` : '';

export function imagesMeta() {
  const c = imgCount(allStacks());
  if (!c.known) return {icon: 'docker', text: st.images_scanning ? 'A verificar as imagens…' : 'Imagens por verificar'};
  const bad = c.missing + c.unreadable;
  if (bad) return {icon: 'alert', warn: true, text: `${plural(bad, 'imagem em falta', 'imagens em falta')}: o failover precisaria da internet`};
  return {icon: 'docker', text: `${plural(c.present, 'imagem pronta', 'imagens prontas')} para o failover, ${bytes(c.size)}`};
}

// "If the server fell now, would the failover work?", checked after every image scan.
export function readyMeta() {
  const v = st.verdict || {};
  if (!v.at) return null;
  const p = v.problems || [];
  return p.length ? {icon: 'alert', warn: true, text: `${plural(p.length, 'problema', 'problemas')} para um failover`, title: p.join('\n')}
    : {icon: 'check', text: `Pronto para failover · verificado ${new Date(v.at).toDateString() === new Date().toDateString() ? `às ${clock(v.at)}` : ago(v.at)}`};
}
// When the TOS backup last wrote to the mirror: a failover starts from it.
export function mirrorMeta() {
  if (!st.mirror_changed) return null;
  return st.mirror_stale ? {icon: 'layers', warn: true, text: `Espelho parado desde ${day(st.mirror_changed)}: o backup do TOS parou?`}
    : {icon: 'layers', text: `Espelho: última mudança ${ago(st.mirror_changed)}`, cls: 'sm-hide'};
}

// A failover's steps, as the agent records them.
export const STEPS = {'início': 'Início', NPM: 'NPM do TNAS arrancado', snapshot: 'Snapshot do espelho', imagens: 'Imagens descarregadas', arranque: 'Cópia arrancada',
  resposta: 'Cópia a responder pelo NPM do TNAS', DNS: 'DNS a apontar para o TNAS'};
export const secsTxt = n => n < 60 ? `${n} s` : `${Math.floor(n / 60)} min ${n % 60} s`;
// what the failover is doing now, from its last finished step
export function nextStep(s) {
  if (/descarregar/.test(s.msg || '')) return sentence(s.msg).replace(/\.$/, '');
  switch ((s.steps || []).at(-1)?.name) {
    case 'snapshot': case 'imagens': return 'A arrancar a cópia';
    case 'NPM': return 'A fazer o snapshot do espelho';
    case 'arranque': return 'À espera que a cópia responda';
    case 'resposta': return 'A mudar o DNS';
    default: return 'A preparar o failover';
  }
}
export function stepsHtml(s) {
  const steps = s.steps || [];
  const rows = steps.map((x, i) => `<li><span class="ok-text">${icon('check')}</span><span class="grow">${esc(STEPS[x.name] || x.name)}</span>
    <span class="side">${esc(clock(x.at))}${i ? ` · ${secsTxt(Math.round((Date.parse(x.at) - Date.parse(steps[i - 1].at)) / 1000))}` : ''}</span></li>`);
  if (s.state === 'FAILING_OVER') rows.push(`<li class="step-now"><span class="dot"></span><span class="grow">${esc(nextStep(s))}…</span></li>`);
  return `<ul class="list steps">${rows.join('')}</ul>`;
}

export const pill = s => {
  const S = look(s);
  // failing on the server: a ring fills up until the failover starts
  const ring = s.state === 'NORMAL' && s.fail_since && !s.server_ok
    ? `<span class="ring" style="--p:${Math.min(100, Math.round(minsSince(s.fail_since) / s.wait_min * 100))}"></span>` : '';
  const b = st.beats?.[s.name], l = b?.[b.length - 1];
  const ms = !fine(s) ? S.label : l?.s === 'up' ? `${l.ms} ms` : l?.s === 'down' ? 'em falha' : '';
  return `<button class="pill ${S.busy ? 'busy' : ''} ${fine(s) ? 'quiet' : ''}" data-panel="svc" data-svc="${esc(s.name)}" data-flip="${esc(s.name)}"
    style="--c:${S.c}" title="${esc(S.label)}${s.state === 'FAILING_OVER' ? `: ${esc(nextStep(s))}` : ''}. Ver detalhes">${svcIcon(s.name)}${esc(s.name)}
    <span class="beats" data-beats="${esc(s.name)}" data-n="28" aria-hidden="true"></span><span class="pill-ms">${esc(ms)}</span>${S.busy ? '<span class="dot"></span>' : ring}</button>`;
};

// Heartbeat bars: the last checks of the server's NPM or of one service, newest on the right.
export const lastBeat = {};
// The height is the time, on a log scale: 5 ms is under half, 500 ms near full; a failure is full and red.
export const beatH = x => x.s === 'down' ? 100 : x.s === 'up' ? Math.round(25 + 75 * Math.min(1, Math.log10(1 + x.ms) / 3)) : 30;
export function beatsHtml(name, n = 40) {
  const b = ((st.beats || {})[name] || []).slice(-n);
  const isNew = b.length && lastBeat[name] !== b[b.length - 1].t;
  const tip = x => `${clockS(x.t)}: ${x.s === 'up' ? `${x.ms} ms` : x.s === 'down' ? 'em falha' : 'não verificado'}`;
  return '<i class="beat" style="--h:15%"></i>'.repeat(Math.max(0, n - b.length))
    + b.map((x, i) => `<i class="beat ${x.s}${x.s === 'up' && x.ms >= 1000 ? ' slow' : ''}${isNew && i === b.length - 1 ? ' new' : ''}" style="--h:${beatH(x)}%" title="${esc(tip(x))}"></i>`).join('');
}
export function beatsFoot(name) {
  const b = (st.beats || {})[name] || [];
  if (!b.length) return '<span>À espera da primeira verificação</span>';
  const up = b.filter(x => x.s === 'up'), last = b[b.length - 1];
  const avg = up.length ? Math.round(up.reduce((n, x) => n + x.ms, 0) / up.length) : 0;
  return `<span>${esc(ago(b[0].t))}</span><span>${last.s === 'up' ? `${last.ms} ms, média ${avg} ms` : last.s === 'down' ? 'em falha' : 'não verificado'}</span>`;
}
// Bars live outside the painted markup so a new check does not redraw the whole card.
export function fillBeats() {
  document.querySelectorAll('[data-beats]').forEach(el => {
    const html = beatsHtml(el.dataset.beats, +el.dataset.n || 40);
    if (el.__html !== html) { el.__html = html; el.innerHTML = html; }
  });
  document.querySelectorAll('[data-beats-foot]').forEach(el => {
    const html = beatsFoot(el.dataset.beatsFoot);
    if (el.__html !== html) { el.__html = html; el.innerHTML = html; }
  });
  for (const [name, b] of Object.entries(st.beats || {})) if (b.length) lastBeat[name] = b[b.length - 1].t;
}
export const beatsBlock = name => `<div class="beats" data-beats="${esc(name)}" role="img" aria-label="Últimas verificações"></div>
  <div class="beats-foot" data-beats-foot="${esc(name)}"></div>`;

// Each box's NPM: a dot and what it is doing, the colour says the state again and the title says why.
export const npmDot = (label, c, why, busy = false) => `<span title="${esc(why)}">${chip(label, c, `quiet${busy ? ' busy' : ''}`)}</span>`;
export const serverChip = () => st.server_npm_ok ? npmDot('NPM a responder', 'var(--success)', 'NPM do servidor a responder')
  : st.npm_alerted ? npmDot('NPM em falha', 'var(--warning)', 'NPM do servidor em falha, com o servidor vivo')
  : npmDot('NPM sem resposta', 'var(--error)', `NPM do servidor sem resposta ${ago(st.npm_fail_since)}`);
export const tnasChip = () => {
  const n = st.tnas_npm;
  return !n.snapshot && !n.stand_in ? npmDot('NPM parado', 'var(--neutral)', 'NPM do TNAS em espera: arranca com o primeiro failover ou com o NPM do servidor em falha')
    : n.ok ? npmDot('NPM a servir', 'var(--info)', n.stand_in ? 'NPM do TNAS a servir: o NPM do servidor não responde' : 'NPM do TNAS a servir')
    : npmDot('NPM a arrancar', 'var(--warning)', n.msg || 'NPM do TNAS a arrancar', true);
};

// Machine state for the node icons. The agent runs on the TNAS, so the page
// answering already says the TNAS is on; the pings say whether the LAN sees it.
export const serverHealth = () => !st.router_ok ? {s: 'unknown', why: 'Sem router, não dá para saber'}
  : st.server_npm_ok ? {s: 'up', why: 'O servidor e o NPM respondem'}
  : st.server_up ? {s: 'warn', why: 'O servidor responde a ping mas o NPM não'}
  : {s: 'down', why: 'O servidor não responde'};
export const tnasHealth = () => !st.tnas_up ? {s: 'down', why: 'O TNAS não responde no próprio IP da LAN'}
  : !st.router_ok ? {s: 'warn', why: 'O TNAS está ligado mas não chega ao router'}
  : {s: 'up', why: 'O TNAS está ligado e na LAN'};

export let firstTopo = true;
export function renderTopology() {
  const at = {server: [], moving: [], tnas: []};
  for (const s of st.services) {
    if (s.state === 'ACTIVE') at.tnas.push(s);
    else if (s.state === 'FAILING_OVER' || s.state === 'RETURNING') at.moving.push(s);
    else at.server.push(s);
  }
  const node = (kind, ic, title, ip, chipHtml, meta, list, empty, health, extra = '') => `
    <button class="node-head" data-panel="${kind}" aria-haspopup="dialog" title="Ver detalhes do ${title}">
      <span class="node-icon st st-${health.s}" title="${esc(health.why)}">${icon(ic)}</span>
      <span class="node-id"><span class="node-title">${title}</span><span class="mono muted">${esc(ip)}</span></span>
      <span class="node-end">${extra}${chipHtml}</span></button>
    ${[meta].flat().filter(Boolean).map(m => `<div class="node-meta ${m.warn ? 'warn' : ''} ${m.cls || ''}" ${m.title ? `title="${esc(m.title)}"` : ''}>${icon(m.icon)}<span>${esc(m.text)}</span></div>`).join('')}
    <div class="pills" data-drop="${kind}">${list.length ? list.map(pill).join(' ') : `<span class="empty">${empty}</span>`}</div>
    ${kind === 'server' ? `<div class="node-beats">${beatsBlock('npm')}</div>` : ''}`;

  // FLIP: remember where every pill was, redraw, then slide each moved pill from there.
  const before = new Map([...document.querySelectorAll('.topo [data-flip]')].map(el => [el.dataset.flip, el.getBoundingClientRect()]));
  paint('nodeServer', node('server', 'server', 'Servidor', st.server_ip, serverChip(),
    {icon: 'clock', text: `Verificado a cada ${st.check_interval_s} s, pelo NPM do servidor`, cls: 'sm-hide'}, at.server,
    st.services.length ? 'Nenhum serviço no servidor' : 'Nenhum serviço protegido. Adiciona-os em <a href="#/definicoes/servicos">Definições → Serviços</a>.', serverHealth()));
  paint('nodeTnas', node('tnas', 'nas', 'TNAS', st.tnas_ip, tnasChip(), [readyMeta(), imagesMeta(), mirrorMeta()], at.tnas, 'Nenhuma cópia a servir', tnasHealth(),
    `<span class="radar" style="--period:${st.check_interval_s}s" title="Uma volta por verificação, a cada ${st.check_interval_s} s"><i></i></span>`));
  const lane = $('lane'), back = at.moving.length > 0 && at.moving.every(s => s.state === 'RETURNING') && !at.tnas.length;
  // the link shows the checks (probePulse marks each one) and services on their way; client traffic is on the wires
  lane.classList.toggle('active', at.moving.length > 0 || st.router_ok);
  lane.classList.toggle('moving', at.moving.length > 0);
  lane.classList.toggle('down', !st.server_npm_ok);
  lane.classList.toggle('back', at.moving.length ? back : true);
  $('nodeTnas').classList.toggle('lit', at.tnas.length > 0);
  const auto = st.mode === 'auto';
  paint('lane', `<span class="flow"></span><span class="flow-pkt"></span>
    <div class="lane-under"><span class="lane-label">verifica o servidor a cada ${st.check_interval_s} s</span><a class="chip mode-chip" href="#/definicoes/geral" style="--c:${auto ? 'var(--success)' : 'var(--warning)'}"
      title="${auto ? 'Faz failover e regresso sozinho' : 'Só regista o que faria'}. Mudar em Definições → Geral">${auto ? 'Automático' : 'Modo observação'}</a>
      <div class="lane-pills">${at.moving.map(pill).join('')}</div></div>`);
  paint('clients', `<span class="node-icon">${icon('devices')}</span><span><strong>Clientes</strong>
    <small>LAN e WireGuard${st.dns_zone ? `, *.${esc(st.dns_zone)}` : ''}</small></span>`);
  // one internet: a box whose Technitium cannot be asked does not count against it
  const nets = netState(), boxes = s => [nets.server === s && 'servidor', nets.tnas === s && 'TNAS'].filter(Boolean);
  const off = boxes('down'), on = boxes('up'), ns = st.internet.status;
  const ntext = off.length ? `sem acesso: ${off.join(' e ')}` : on.length ? `${on.join(' e ')} com acesso` : 'por verificar';
  paint('internet', `<span class="node-icon st st-${ns}">${icon('web')}</span><span><strong>Internet</strong><small>${ntext}</small></span>`);
  $('internet').title = `A cada ${st.check_interval_s} s, cada caixa pede ao seu Technitium um nome que nenhuma cache tem: a resposta tem de vir da internet`;
  // the Technitium runs on both, in a cluster: a dot for each, asked for its own zone
  const dm = dnsMembers(), ds = dm.every(x => x.s === 'up') ? 'up' : dm.some(x => x.s === 'up') ? 'warn' : dm.some(x => x.s === 'down') ? 'down' : 'unknown';
  paint('dns', `<span class="node-icon st st-${ds}">${icon('dns')}</span><span><strong>DNS em cluster</strong>
    <small>${dm.map(x => `<i class="m ${x.s}"></i>.${esc(x.ip.split('.').pop())}`).join('')}</small></span>`);
  $('dns').title = `O Technitium em cluster: ${dm.map(x => `${x.who} (${x.ip}) ${{up: 'responde', down: 'não responde', unknown: 'por verificar'}[x.s]}`).join(', ')}`;
  paint('routerNode', `<span class="node-icon st st-${st.router_ok ? 'up' : 'down'}">${icon('router')}</span><span><strong>Router</strong>
    <small><span class="mono">${esc(st.router_ip)}</span><span class="sm-hide"> · ${st.router_ok ? `${st.router_ms} ms` : 'sem resposta'}</span></small></span>`);
  renderHud();
  fillBeats();
  renderWires();

  if (!reduceMotion.matches) {
    document.querySelectorAll('.topo [data-flip]').forEach((el, i) => {
      if (firstTopo) {
        el.animate([{opacity: 0, transform: 'translateY(6px) scale(.9)'}, {opacity: 1, transform: 'none'}],
          {duration: 420, delay: 120 + i * 70, easing: EASE, fill: 'backwards'});
        return;
      }
      const b = before.get(el.dataset.flip);
      if (!b) return;
      const a = el.getBoundingClientRect(), dx = b.left - a.left, dy = b.top - a.top;
      if (Math.abs(dx) + Math.abs(dy) < 1) return;
      el.animate([{transform: `translate(${dx}px, ${dy}px)`}, {transform: 'none'}], {duration: 800, easing: EASE});
    });
  }
  firstTopo = false;
  probePulse();
}

// Each box's internet and the two members of the DNS cluster, as the agent
// judged them (netDNS, also in GET /api/).
export const netState = () => ({server: st.internet.server, tnas: st.internet.tnas});
export const dnsMembers = () => st.dns.members.map(m => ({who: m.who, ip: m.ip, s: m.status}));

// Telemetry in the top corners of the topology; the mirror's age moves on every second.
export function renderHud() {
  if (!st) return;
  const v = x => `<b>${esc(x)}</b>`;
  const home = st.services.filter(s => s.state === 'NORMAL').length, img = imgCount(allStacks());
  const set = (cls, html) => { const el = $('hud').querySelector(cls); if (el.__html !== html) { el.__html = html; el.innerHTML = html; } };
  set('.hud-tl', `Modo ${v(st.mode === 'auto' ? 'auto' : 'observação')} · Manut ${v(st.maint_until ? `até ${clock(st.maint_until)}` : 'off')}\nEm casa ${v(`${home}/${st.services.length}`)}`);
  set('.hud-tr', `Espelho ${v(st.mirror_changed ? ago(st.mirror_changed) : '—')}\nImagens ${v(img.known ? `${img.present}/${img.total}` : '—')}`);
}

// Every wire goes through the DNS: the clients ask it, it sends them to the
// server unless a service's A record points at the TNAS; above it the internet,
// under it the router. Packets travel along the wires that carry traffic.
export function renderWires() {
  if ($('page-overview').hidden) return;
  const svg = $('wires'), topo = $('topo');
  if (getComputedStyle(svg).display === 'none') { if ($('wireFx').childElementCount) stopWires(svg); return; }
  const box = topo.getBoundingClientRect();
  const r = id => { const b = $(id).getBoundingClientRect(); return {x: b.left - box.left, y: b.top - box.top, w: b.width, h: b.height}; };
  const c = r('clients'), s = r('nodeServer'), t = r('nodeTnas');
  const n = r('internet'), dn = r('dns'), ro = r('routerNode');
  const right = b => ({x: b.x + b.w, y: b.y + b.h / 2}), left = (b, f = .5) => ({x: b.x, y: b.y + b.h * f});
  // across: a curve leaving and arriving level; up or down: a straight line from a to b
  const d = (f, to) => { const k = (to.x - f.x) * .5; return `M ${f.x} ${f.y} C ${f.x + k} ${f.y}, ${to.x - k} ${to.y}, ${to.x} ${to.y}`; };
  const vert = (a, b) => a.y < b.y ? `M ${a.x + a.w / 2} ${a.y + a.h} L ${b.x + b.w / 2} ${b.y}` : `M ${a.x + a.w / 2} ${a.y} L ${b.x + b.w / 2} ${b.y + b.h}`;
  const viaTnas = st.services.filter(x => x.dns).length, viaServer = st.services.length - viaTnas;
  // n: how much traffic the wire carries; up to 3 packets travel along it
  const wire = (id, path, color, n) => `<g style="--wc:${color}"><path id="${id}" class="wire ${n > 0 ? 'on' : ''}" d="${path}" data-n="${n}"/></g>`;
  const nets = Object.values(netState()), net = nets.includes('up') ? 'up' : nets.includes('down') ? 'down' : 'unknown';
  const tone = x => x === 'up' ? 'var(--success)' : x === 'down' ? 'var(--error)' : 'var(--neutral)';
  const dm = dnsMembers(), dnsUp = dm.some(x => x.s === 'up');
  const html = wire('wire-lan', d(right(c), left(dn)), tone(dnsUp ? 'up' : 'down'), dnsUp ? st.services.length : 0)
    + wire('wire-server', d(right(dn), left(s)), st.server_npm_ok ? 'var(--success)' : 'var(--error)', viaServer)
    + wire('wire-tnas', d(right(dn), left(t)), 'var(--info)', viaTnas)
    + wire('wire-dns-net', vert(dn, n), tone(net), net === 'unknown' ? 0 : 1)
    + wire('wire-dns-router', vert(dn, ro), tone(st.router_ok ? 'up' : 'down'), 1);
  svg.setAttribute('viewBox', `0 0 ${box.width} ${box.height}`);
  if (svg.__html !== html) { svg.__html = html; svg.innerHTML = html; wireMotion(svg); }
}

// The motion along the wires, on the GPU: every dash and packet is an element
// that only transform moves, on keyframes sampled from its wire (a moving SVG
// dash or <animateMotion> is repainted on the CPU every frame). The animations
// run on the page's clock, so a redraw keeps them where they were.
const DASH = 6, PERIOD = 14; // as the SVG's stroke-dasharray: 6 8
// the dashes and packets are thrown away with their animations: an animation of a detached
// element still runs, on the main thread, until the page is reloaded
function stopWires(svg) {
  const fx = $('wireFx');
  fx.getAnimations({subtree: true}).forEach(a => a.cancel());
  fx.replaceChildren();
  svg.__html = null;
}
function wireMotion(svg) {
  const fx = $('wireFx');
  fx.getAnimations({subtree: true}).forEach(a => a.cancel());
  fx.replaceChildren();
  svg.classList.toggle('gpu', !reduceMotion.matches);
  if (reduceMotion.matches) return; // the dashes stay still, on the SVG
  const run = (el, frames, opts) => { el.animate(frames, {iterations: Infinity, ...opts}).startTime = 0; };
  for (const path of svg.querySelectorAll('.wire.on')) {
    const L = path.getTotalLength(), g = document.createElement('div');
    g.setAttribute('style', path.parentNode.getAttribute('style')); // its --wc
    const pt = x => path.getPointAtLength(Math.max(0, Math.min(L, x)));
    const angle = x => { const a = pt(Math.min(x, L - .5)), b = pt(Math.min(x, L - .5) + .5); return Math.atan2(b.y - a.y, b.x - a.x); };
    // a dash starting at x along the wire, cut where the wire starts and ends, as the stroke is
    const dashAt = x => {
      const from = Math.max(0, x), len = Math.max(0, Math.min(L, x + DASH) - from), p = pt(from);
      return {transform: `translate(${p.x}px, ${p.y}px) rotate(${angle(from)}rad) scaleX(${len / DASH})`};
    };
    for (let x0 = -PERIOD; x0 < L; x0 += PERIOD) { // each moves one period a second, as the dash offset did
      const el = document.createElement('i');
      el.className = 'dash';
      run(el, Array.from({length: 9}, (_, i) => dashAt(x0 + PERIOD * i / 8)), {duration: 1000});
      g.append(el);
    }
    const steps = Math.max(12, Math.ceil(L / 10)), stops = Array.from({length: steps + 1}, (_, i) => { const p = pt(L * i / steps); return {transform: `translate(${p.x}px, ${p.y}px)`}; });
    for (let i = 0; i < Math.min(3, +path.dataset.n); i++) { // at an even pace, 0.8 s apart, as <animateMotion> did
      const el = document.createElement('i');
      el.className = 'pkt';
      run(el, stops, {duration: 2400, delay: -i * 800});
      g.append(el);
    }
    fx.append(g);
  }
}

// Every check the agent makes (it runs on the TNAS) crosses the link to the
// server once, green if the server's NPM answered, red if it did not.
export let lastTick = null;
export function probePulse() {
  const tick = st.now;
  if (tick === lastTick) return;
  const first = lastTick === null;
  lastTick = tick;
  if (first || reduceMotion.matches || !st.router_ok) return;
  const radar = document.querySelector('#nodeTnas .radar i');
  const an = radar?.getAnimations()[0];
  if (an) an.currentTime = 0;
  const lane = $('lane');
  const color = st.server_npm_ok ? 'var(--success)' : 'var(--error)';
  const dot = document.createElement('span');
  dot.className = 'probe';
  dot.style.setProperty('--pc', color);
  lane.append(dot);
  const len = lane.clientHeight;
  const at = p => `translate(-50%, ${p}px)`;
  // the ring is a layer with a fixed shadow: only transform and opacity change
  const ripple = el => {
    if (!el) return;
    const r = document.createElement('span');
    r.className = 'ripple';
    r.style.setProperty('--pc', color);
    el.append(r);
    r.animate([{transform: 'scale(1)', opacity: 1}, {transform: 'scale(1.6)', opacity: 0}], {duration: 700, easing: 'ease-out'}).onfinish = () => r.remove();
  };
  ripple(document.querySelector('#nodeTnas .node-icon'));
  dot.animate([{transform: at(len), opacity: 0}, {opacity: 1, offset: .15}, {opacity: 1, offset: .85}, {transform: at(0), opacity: 0}],
    {duration: 1100, easing: 'cubic-bezier(.45, 0, .55, 1)'}).onfinish = () => {
      dot.remove();
      ripple(document.querySelector('#nodeServer .node-icon'));
    };
}

export function renderGlobal() {
  const rel = /^v\d/.test(st.version) ? `/releases/tag/${encodeURIComponent(st.version)}` : ''; // a dev build has no release
  if ($('version').textContent !== st.version) $('version').textContent = st.version;
  $('version').href = `https://github.com/mordilloSan/failover${rel}`;
  const m = $('maintBtn'), busy = pending.has('maint:'), on = busy ? pending.get('maint:') : !!st.maint_until;
  const label = busy ? 'A aplicar…' : on ? `Manutenção do servidor até ${clock(st.maint_until)}. Carrega para desligar` : 'Manutenção do servidor';
  m.classList.toggle('on', on);
  m.disabled = busy;
  m.setAttribute('aria-pressed', String(on));
  m.title = label;
  m.setAttribute('aria-label', label);
  paint('maintBtn', `${icon('wrench')}${on ? `<span>${busy ? 'A aplicar…' : `até ${esc(clock(st.maint_until))}`}</span>` : ''}`);
  const banner = !st.router_ok ? 'O router não responde. O agente não decide nada até ele voltar.'
    : !st.dns_token ? 'Falta o token do Technitium: sem ele um failover dá erro logo no início. Põe-no em Definições.'
    : st.maint_until ? `Manutenção do servidor até ${clock(st.maint_until)}. Nenhum failover começa; os regressos continuam.` : '';
  $('banner').hidden = !banner;
  $('banner').style.setProperty('--c', st.router_ok ? 'var(--warning)' : 'var(--error)');
  if ($('bannerText').textContent !== banner) $('bannerText').textContent = banner; // a role=alert is announced again when rewritten
  $('setupBanner').hidden = !st.setup_pending;
  const mn = st.mirror_new || [];
  $('mirrorBanner').hidden = !mn.length;
  paint('mirrorText', mn.length ? `${mn.length === 1 ? 'Pasta nova' : `${mn.length} pastas novas`} no espelho: ${mn.map(mono).join(', ')}.
    <a href="#/definicoes/servicos" data-mirror-seen>Proteger em Definições → Serviços</a> · <a href="#/" data-mirror-seen>Dispensar</a>` : '');
  // the tab and its icon say the state, seen from the list of tabs
  const away = st.services.filter(x => x.state !== 'NORMAL'), err = away.some(x => x.state === 'ERROR');
  const nErr = away.filter(x => x.state === 'ERROR').length, nAway = away.length - nErr;
  document.title = away.length ? `● ${[nErr && `${nErr} com erro`, nAway && `${nAway} em failover`].filter(Boolean).join(' · ')} · Failover` : 'Failover';
  const fav = err ? '%23f54b3f' : away.length ? '%23e3a008' : '%232052c2';
  if (fav !== painted.fav) {
    painted.fav = fav;
    document.querySelector('link[rel=icon]').href = `data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24'%3E%3Crect width='24' height='24' rx='6' fill='${fav}'/%3E%3Cpath fill='%23fff' transform='translate(2.4 2.4) scale(.8)' d='m21 9l-4-4v3h-7v2h7v3M7 11l-4 4l4 4v-3h7v-2H7z'/%3E%3C/svg%3E`;
  }
}

export function maintSwitch(s) {
  const key = `maint:${s.name}`, busy = pending.has(key);
  const on = busy ? pending.get(key) : !!s.maint_until;
  const text = busy ? 'A aplicar…' : s.maint_until ? `Manutenção até ${clock(s.maint_until)}` : 'Manutenção';
  return `<label class="toggle"><input type="checkbox" role="switch" class="switch" data-maint="${esc(s.name)}" ${on ? 'checked' : ''} ${busy ? 'disabled' : ''}>
    <span>${text}</span></label>`;
}

export function actionButton(s, act, allowed) {
  const busy = pending.has(`${act}:${s.name}`);
  if (!allowed && !busy) return '';
  const cls = act === 'return' ? 'btn danger' : 'btn';
  if (busy) return `<button class="${cls} busy" disabled><span class="dot"></span>${act === 'failover' ? 'A forçar failover…' : 'A forçar regresso…'}</button>`;
  return `<button class="${cls}" data-act="${act}" data-svc="${esc(s.name)}">${icon(act === 'failover' ? 'swap' : 'restore')}${act === 'failover' ? 'Forçar failover' : 'Forçar regresso'}</button>`;
}

export const canFail = s => s.state === 'NORMAL' || (s.state === 'ERROR' && !s.snapshot && !s.dns);
export const canReturn = s => ['FAILING_OVER', 'ACTIVE', 'ERROR'].includes(s.state);
export const actions = s => `${maintSwitch(s)}<span>${actionButton(s, 'failover', canFail(s))} ${actionButton(s, 'return', canReturn(s))}</span>`;
