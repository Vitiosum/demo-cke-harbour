// Harbour: the page unloads one container every 300 ms (POST /api/unload), and draws
// what Kubernetes says about the cranes (GET /api/fleet) and what the ledger holds (GET /api/ledger).
'use strict';

(() => {
  const $ = (sel) => document.querySelector(sel);
  const NF = new Intl.NumberFormat('en');
  const PAINTS = ['var(--paint-v1)', 'var(--paint-v2)', 'var(--paint-v3)', 'var(--paint-v4)'];
  const BOXES = ['#2f6fb5', '#b8402f', '#3f8f5a', '#c9922e', '#6b7286', '#7b3f8c', '#c8ccd6', '#1f8a8a'];
  const STACK_MAX = 12;

  const state = {
    unloaded: 0,
    lost: 0,
    paused: false,
    inflight: 0,
    byPod: {},      // pod name -> version, learned from answers and from the ledger
    byHash: {},     // pod-template-hash -> version (all pods of a ReplicaSet share it)
    cranes: new Map(),
    versions: new Set(),
    events: new Set(),
    lastLedgerId: 0,
    fleetNames: new Set(),
    servedBy: '',
  };

  // ---------- helpers ----------
  const hashString = (s) => [...s].reduce((h, c) => (h * 31 + c.charCodeAt(0)) >>> 0, 7);
  const paintFor = (version) => {
    if (!version) return 'var(--cc-muted)';
    const m = /^v(\d+)/.exec(version);
    return PAINTS[(m ? Number(m[1]) - 1 : hashString(version)) % PAINTS.length];
  };
  const parts = (pod) => pod.split('-');
  const suffixOf = (pod) => { const p = parts(pod); return p.length > 2 ? p[p.length - 1] : pod.slice(-6); };
  const hashOf = (pod) => { const p = parts(pod); return p.length > 2 ? p[p.length - 2] : ''; };
  const versionOf = (pod) => state.byPod[pod] || state.byHash[hashOf(pod)] || '';
  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  };
  const ago = (iso) => {
    const s = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 1000));
    if (s < 60) return `${s} s`;
    if (s < 3600) return `${Math.floor(s / 60)} min`;
    return `${Math.floor(s / 3600)} h`;
  };
  async function getJSON(url, options = {}) {
    const res = await fetch(url, { cache: 'no-store', signal: AbortSignal.timeout(5000), ...options });
    if (!res.ok) throw new Error(`${url}: ${res.status}`);
    return res.json();
  }

  function learn(pod, version) {
    if (!pod || !version) return;
    state.byPod[pod] = version;
    const h = hashOf(pod);
    if (h) state.byHash[h] = version;
    if (!state.versions.has(version)) {
      state.versions.add(version);
      renderLegend();
    }
  }

  // ---------- the cranes ----------
  const FRAME = `
    <svg class="crane__frame" viewBox="0 0 100 160" preserveAspectRatio="none" aria-hidden="true">
      <polyline class="paint-line" points="30,26 50,5 70,26"/>
      <line class="paint-line" x1="50" y1="5" x2="50" y2="26"/>
      <rect class="paint" x="2" y="26" width="96" height="9"/>
      <path class="lattice" d="M8 26 L16 35 L24 26 L32 35 L40 26 M60 26 L68 35 L76 26 L84 35 L92 26"/>
      <rect class="house" x="39" y="16" width="22" height="10"/>
      <rect class="paint" x="12" y="35" width="8" height="125"/>
      <rect class="paint" x="80" y="35" width="8" height="125"/>
      <rect class="paint" x="12" y="58" width="76" height="6"/>
      <line class="brace" x1="20" y1="64" x2="80" y2="98"/>
      <line class="brace" x1="80" y1="64" x2="20" y2="98"/>
      <ellipse class="beacon" cx="50" cy="5" rx="2.4" ry="1.6"/>
    </svg>`;

  function craneFor(name) {
    let li = state.cranes.get(name);
    if (li) return li;
    li = el('li', 'crane is-new');
    li.dataset.pod = name;
    const rig = el('div', 'crane__rig');
    rig.innerHTML = FRAME;
    rig.append(el('div', 'crane__hoist'), el('ul', 'crane__stack'));
    const plate = el('div', 'plate');
    const cpu = el('span', 'plate__cpu');
    cpu.append(el('i'));
    plate.append(el('span', 'plate__id', suffixOf(name)), el('span', 'plate__ver', '…'), el('span', 'plate__state'), cpu);
    li.append(rig, plate);
    const empty = $('#quay .quay__empty');
    if (empty) empty.remove();
    $('#quay').append(li);
    state.cranes.set(name, li);
    setTimeout(() => li.classList.remove('is-new'), 1000);
    return li;
  }

  function paintCrane(li) {
    const v = versionOf(li.dataset.pod);
    li.style.setProperty('--paint', paintFor(v));
    li.querySelector('.plate__ver').textContent = v || '…';
  }

  function dropBox(entry) {
    const li = state.cranes.get(entry.crane);
    if (!li) return;
    const stack = li.querySelector('.crane__stack');
    const box = el('li', 'box is-new');
    box.style.setProperty('--c', BOXES[hashString(entry.goods || '') % BOXES.length]);
    box.title = `${entry.container}: ${entry.goods} from ${entry.origin}`;
    box.append(el('i'));
    stack.append(box);
    while (stack.children.length > STACK_MAX) stack.firstElementChild.remove();
    li.classList.add('is-busy');
    setTimeout(() => li.classList.remove('is-busy'), 320);
  }

  function splash() {
    const s = el('div', 'splash');
    s.style.left = `${10 + Math.random() * 80}%`;
    s.append(el('i'));
    $('#splashes').append(s);
    setTimeout(() => s.remove(), 1500);
  }

  // ---------- unloading: one container every 300 ms ----------
  async function unloadOne() {
    if (state.paused || document.hidden || state.inflight >= 3) return;
    state.inflight++;
    try {
      const entry = await getJSON('/api/unload', { method: 'POST' });
      learn(entry.crane, entry.version);
      const li = state.cranes.get(entry.crane);
      if (li) paintCrane(li);
      state.unloaded++;
      $('#unloaded').textContent = NF.format(state.unloaded);
      dropBox(entry);
    } catch {
      state.lost++;
      const g = $('#gauge-lost');
      $('#lost').textContent = NF.format(state.lost);
      g.classList.add('is-hit');
      g.classList.remove('is-flash');
      void g.offsetWidth;
      g.classList.add('is-flash');
      splash();
    } finally {
      state.inflight--;
    }
  }

  // ---------- what Kubernetes says ----------
  async function refreshFleet() {
    if (document.hidden) return;
    let f;
    try {
      f = await getJSON('/api/fleet');
    } catch {
      $('#rollout-note').textContent = 'the Kubernetes API did not answer this time';
      return;
    }
    $('#k8s-version').textContent = f.local ? 'Local run' : `Kubernetes ${f.kubernetes || ''}`.trim();
    $('#kube-sub').textContent = f.local ? 'Not running in a cluster' : `Events of the namespace ${f.namespace}`;
    renderCranes(f);
    renderRollout(f);
    renderAutoscaler(f);
    renderRush(f);
    renderEvents(f);
    const me = f.cranes.find((c) => c.name === state.servedBy);
    if (me && me.image) $('#self-image').textContent = me.image;
  }

  function renderCranes(f) {
    const names = new Set(f.cranes.map((c) => c.name));
    state.fleetNames = names;
    for (const c of f.cranes) {
      const li = craneFor(c.name);
      li.classList.toggle('is-starting', !c.ready && !c.terminating);
      li.classList.toggle('is-leaving', c.terminating);
      paintCrane(li);
      let label = 'On duty';
      if (c.terminating) label = 'Signing off';
      else if (!c.ready) label = c.restarts > 0 ? `Not ready, ${c.restarts} restarts` : 'Rigging';
      li.querySelector('.plate__state').textContent = label;
      const bar = li.querySelector('.plate__cpu');
      if (c.cpu != null && c.request) {
        const pct = Math.round((c.cpu / c.request) * 100);
        bar.firstElementChild.style.width = `${Math.min(100, pct)}%`;
        bar.title = `CPU ${c.cpu}m, ${pct}% of its ${c.request}m request`;
      }
      const target = f.hpa && f.hpa.target ? f.hpa.target : 60;
      bar.style.setProperty('--target', `${target}%`);
      li.setAttribute('aria-label', `Crane ${c.name}, ${versionOf(c.name) || 'version unknown'}, ${label}`);
    }
    for (const [name, li] of state.cranes) {
      if (names.has(name)) continue;
      state.cranes.delete(name);
      li.classList.add('is-gone');
      setTimeout(() => li.remove(), 700);
    }
    if (f.cranes.length === 0 && !$('#quay .quay__empty')) {
      $('#quay').append(el('li', 'quay__empty', 'No crane on duty'));
    }
  }

  function setGauge(id, value, note, mode) {
    $(`#${id}`).textContent = value;
    $(`#${id}-note`).textContent = note;
    const g = $(`#gauge-${id}`);
    g.classList.toggle('is-alert', mode === 'alert');
    g.classList.toggle('is-busy', mode === 'busy');
  }

  function renderRollout(f) {
    const d = f.deployment;
    if (f.local) return setGauge('rollout', 'Local', 'not running in a cluster: no Deployment');
    if (!d) return setGauge('rollout', '–', 'no Deployment named harbour in this namespace');
    if (d.stalled) return setGauge('rollout', 'Stalled', 'the new crane never became ready, so the old cranes keep all the traffic', 'alert');
    const newest = [...f.cranes].reverse().find((c) => !c.terminating);
    const v = newest ? versionOf(newest.name) : '';
    if (d.rolling) {
      const waiting = f.cranes.find((c) => !c.ready && !c.terminating && Date.now() - Date.parse(c.started) > 25000);
      const note = waiting
        ? 'a new crane is not ready yet: it gets no traffic until it is'
        : `revision ${d.revision}${v ? ` (${v})` : ''} rolling out, ${d.ready} cranes ready`;
      return setGauge('rollout', `${d.updated}/${d.desired}`, note, waiting ? 'alert' : 'busy');
    }
    setGauge('rollout', `Rev ${d.revision}`, `steady${v ? ` on ${v}` : ''}, ${d.ready} of ${d.desired} cranes ready`);
  }

  function renderAutoscaler(f) {
    const h = f.hpa;
    if (f.local) return setGauge('hpa', '–', 'not running in a cluster');
    if (!h) return setGauge('hpa', '–', 'no HorizontalPodAutoscaler named harbour');
    const value = h.desired !== h.current ? `${h.current} → ${h.desired}` : `${h.current}`;
    let note = `waiting for CPU metrics, ${h.min} to ${h.max} cranes`;
    if (!f.metrics) note = 'no CPU metrics: install metrics-server';
    else if (h.cpu != null) note = `CPU at ${h.cpu}% of requests, target ${h.target}%, ${h.min} to ${h.max} cranes`;
    const busy = h.desired !== h.current || (h.cpu != null && h.cpu > h.target);
    setGauge('hpa', value, note, busy ? 'busy' : '');
  }

  function renderRush(f) {
    const ships = $('#ships');
    const label = $('#rush-label');
    const active = f.rush && f.rush.running > 0;
    const wanted = active ? Math.min(6, Math.max(1, Math.ceil((Number(f.rush.rps) || 10) / 10))) : 0;
    while (ships.children.length > wanted) ships.lastElementChild.remove();
    while (ships.children.length < wanted) {
      const i = ships.children.length;
      const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
      s.setAttribute('viewBox', '0 0 100 34');
      s.setAttribute('class', 'ship');
      const deck = [0, 1, 2, 3, 4].map((k) => `<rect x="${12 + k * 12}" y="${k % 2 ? 6 : 10}" width="11" height="${k % 2 ? 12 : 8}" fill="${BOXES[(i + k) % BOXES.length]}"/>`).join('');
      s.innerHTML = `<path class="hull" d="M2 18 H98 L90 32 H10 Z"/>${deck}<rect class="hull" x="78" y="2" width="11" height="16"/>`;
      s.style.left = `${4 + i * 15}%`;
      s.style.animationDelay = `${-i * 0.7}s`;
      ships.append(s);
    }
    label.hidden = !active;
    if (active) label.textContent = `Rush hour: ${f.rush.rps || '?'} requests per second`;
  }

  function renderEvents(f) {
    const list = $('#events');
    list.replaceChildren();
    if (!f.events.length) {
      list.append(el('li', 'events__empty', f.local ? 'Run it in a cluster to see Kubernetes events.' : 'No recent event.'));
      return;
    }
    const first = state.events.size === 0;
    for (const e of f.events) {
      const key = `${e.time}|${e.reason}|${e.object}|${e.message}`;
      const li = el('li', e.type === 'Warning' ? 'is-warning' : '');
      if (!first && !state.events.has(key)) li.classList.add('is-new');
      state.events.add(key);
      const top = el('div', 'ev-top');
      top.append(el('span', 'ev-reason', e.reason), el('span', 'ev-object', e.object), el('span', 'ev-age', ago(e.time)));
      const msg = e.message.length > 180 ? `${e.message.slice(0, 177)}…` : e.message;
      li.append(top, el('div', 'ev-msg', e.count > 1 ? `${msg} (×${e.count})` : msg));
      list.append(li);
    }
  }

  // ---------- the ledger ----------
  async function refreshLedger() {
    if (document.hidden) return;
    let v;
    try {
      v = await getJSON('/api/ledger');
    } catch {
      $('#ledger-sum').textContent = 'The ledger did not answer this time. The page will try again in 2 seconds.';
      return;
    }
    for (const t of v.cranes || []) learn(t.crane, t.version);
    const body = $('#ledger-rows');
    const rows = (v.recent || []).map((e) => {
      const tr = el('tr', state.lastLedgerId && e.id > state.lastLedgerId ? 'is-new' : '');
      const time = el('td', 'l-time', new Date(e.at).toLocaleTimeString('en-GB'));
      const code = el('td', 'l-code', `${e.container.slice(0, 4)} ${e.container.slice(4, 10)} `);
      code.append(el('b', '', e.container.slice(10)));
      const cargo = el('td', 'l-cargo', e.goods);
      cargo.append(el('small', '', `from ${e.origin}, ${e.tonnes} t`));
      const crane = el('td', 'l-crane');
      const chip = el('span', 'chip', e.version);
      chip.style.setProperty('--paint', paintFor(e.version));
      crane.append(chip, document.createTextNode(suffixOf(e.crane)));
      tr.append(time, code, cargo, crane);
      return tr;
    });
    if (rows.length) body.replaceChildren(...rows);
    if (v.recent && v.recent.length) state.lastLedgerId = v.recent[0].id;

    const names = new Set((v.cranes || []).map((t) => t.crane));
    const gone = [...names].filter((n) => !state.fleetNames.has(n)).length;
    const sum = $('#ledger-sum');
    sum.replaceChildren();
    const b = (text) => el('b', '', text);
    sum.append(b(NF.format(v.total)), ' containers and ', b(`${NF.format(Math.round(v.tonnes))} t`),
      ' of cargo, unloaded by ', b(NF.format(names.size)), names.size === 1 ? ' crane. ' : ' cranes. ');
    if (state.fleetNames.size && gone > 0) {
      sum.append(b(NF.format(gone)), gone === 1 ? ' of them no longer exists' : ' of them no longer exist',
        ': their lines stay here, in PostgreSQL, outside the cluster.');
    } else if (state.fleetNames.size) {
      sum.append('All of them are still on duty.');
    }
  }

  function renderLegend() {
    const legend = $('#legend');
    legend.replaceChildren();
    for (const v of [...state.versions].sort()) {
      const li = el('li', '', `${v} cranes`);
      li.style.setProperty('--paint', paintFor(v));
      legend.append(li);
    }
    legend.append(el('li', 'legend__lost', 'lost container'));
  }

  // ---------- start ----------
  $('#pause').addEventListener('click', (ev) => {
    state.paused = !state.paused;
    ev.currentTarget.setAttribute('aria-pressed', String(state.paused));
    ev.currentTarget.textContent = state.paused ? 'Resume unloading' : 'Pause unloading';
  });

  getJSON('/api/whoami').then((me) => {
    state.servedBy = me.pod;
    $('#self-pod').textContent = me.pod;
    $('#self-node').textContent = me.node;
    $('#self-ns').textContent = me.namespace;
    $('#self-version').textContent = me.version;
    $('#self-go').textContent = me.go;
    $('#self-image').textContent = '…';
    learn(me.pod, me.version);
  }).catch(() => {});

  renderLegend();
  refreshFleet();
  refreshLedger();
  setInterval(unloadOne, 300);
  setInterval(refreshFleet, 1000);
  setInterval(refreshLedger, 2000);
})();
