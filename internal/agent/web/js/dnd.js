// Dragging a service between the server and the TNAS.
import {$, EASE, look, reduceMotion, st, toast} from './core.js';
import {canFail, canReturn} from './topology.js';
import {forceAction} from './main.js';

// Dragging a service from one box to the other: to the TNAS forces a failover,
// to the server a return, with the same confirmation. A copy follows the
// pointer; the box it may go to lights up. A plain click still opens the panel.
export let drag = null, dragClick = false;
// With a finger the drag starts after holding the pill for half a second (a
// short buzz says so); a finger that moves first is a scroll.
document.addEventListener('pointerdown', e => {
  const p = e.target.closest('.node .pill');
  const s = p && st.services.find(x => x.name === p.dataset.svc);
  if (!s || e.button !== 0) return;
  drag = {p, s, from: p.closest('[data-drop]').dataset.drop, x: e.clientX, y: e.clientY, touch: e.pointerType === 'touch'};
  if (drag.touch) {
    const d = drag;
    d.timer = setTimeout(() => {
      if (drag !== d) return;
      d.held = true;
      navigator.vibrate?.(15);
      addEventListener('touchmove', noScroll, {passive: false}); // only now: a listener like this slows every scroll
      startGhost(d.x, d.y);
    }, 450);
  }
});
// no scroll while a held pill is dragged (the listener must not be passive to say so)
export const noScroll = e => e.preventDefault();
export function startGhost(x, y) {
  const r = drag.p.getBoundingClientRect(), g = drag.p.cloneNode(true);
  g.classList.add('pill-ghost');
  Object.assign(g.style, {left: `${r.left}px`, top: `${r.top}px`, width: `${r.width}px`});
  document.body.append(g);
  drag.p.classList.add('dragging');
  Object.assign(drag, {ghost: g, home: r,
    act: drag.from === 'server' ? 'failover' : 'return', to: $(drag.from === 'server' ? 'nodeTnas' : 'nodeServer')});
  drag.ok = drag.act === 'failover' ? canFail(drag.s) : canReturn(drag.s);
  drag.to.classList.toggle('drop-ok', drag.ok);
  moveGhost(x, y);
}
export function moveGhost(x, y) {
  drag.ghost.style.transform = `translate(${x - drag.x}px, ${y - drag.y}px) rotate(-2deg)`; // placed once at the pill: moving left/top lays out every frame
  const t = drag.to.getBoundingClientRect();
  drag.over = x >= t.left && x <= t.right && y >= t.top && y <= t.bottom;
  drag.to.classList.toggle('over', drag.over && drag.ok);
}
document.addEventListener('pointermove', e => {
  if (!drag) return;
  const moved = Math.hypot(e.clientX - drag.x, e.clientY - drag.y);
  if (drag.touch && !drag.held) { // moved before the hold: a scroll, not a drag
    if (moved > 8) { clearTimeout(drag.timer); drag = null; }
    return;
  }
  if (!drag.ghost) {
    if (moved < 6) return;
    startGhost(e.clientX, e.clientY);
    return;
  }
  moveGhost(e.clientX, e.clientY);
});
export const endDrag = async e => {
  const d = drag;
  drag = null;
  clearTimeout(d?.timer);
  removeEventListener('touchmove', noScroll);
  if (!d?.ghost) return;
  dragClick = true; // a click right after is the drop, not a request for the panel
  setTimeout(() => { dragClick = false; }, 0);
  d.to.classList.remove('drop-ok', 'over');
  const settle = r => d.ghost.animate([{transform: d.ghost.style.transform, opacity: 1}, {transform: `translate(${r.left - d.home.left}px, ${r.top - d.home.top}px) rotate(-2deg)`, opacity: .2}],
    {duration: reduceMotion.matches ? 0 : 260, easing: EASE}).finished.then(() => d.ghost.remove());
  const back = () => { settle(d.home); document.querySelector(`.node .pill[data-svc="${CSS.escape(d.s.name)}"]`)?.classList.remove('dragging'); };
  if (!d.over || e.type !== 'pointerup') { back(); return; }
  if (!d.ok) { back(); toast(`${d.s.name} não pode ${d.act === 'failover' ? 'ir para o TNAS' : 'voltar ao servidor'} agora: ${look(d.s).label.toLowerCase()}`, false); return; }
  if (!await forceAction(d.s.name, d.act)) { back(); return; } // the copy waits where it was dropped
  d.ghost.remove();
  document.querySelector(`.node .pill[data-svc="${CSS.escape(d.s.name)}"]`)?.classList.remove('dragging');
};
document.addEventListener('pointerup', endDrag);
document.addEventListener('pointercancel', endDrag);
document.addEventListener('click', e => { if (dragClick) { dragClick = false; e.stopPropagation(); e.preventDefault(); } }, true);
