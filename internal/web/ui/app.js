/* juggler web UI. Vanilla JS over the REST API (see README "Web UI and REST
   API"). State lives in S; render() redraws the list, renderDetail() the
   selected workstream. An EventSource on /api/v1/events triggers refreshes. */
'use strict';

const S = {
  status: null, items: [], groups: [], sources: [], config: null, repos: [],
  selected: null, detail: null, todo: null, session: null, cands: null, candsAll: false,
  filter: '', onlyLive: false, onlyMine: localStorage.getItem('jug.mine') === '1', showFinished: localStorage.getItem('jug.finished') === '1', live: false,
  view: localStorage.getItem('jug.view') || 'grouped',
  collapsed: JSON.parse(localStorage.getItem('jug.collapsed') || '{}'),
};
const NOGROUP = '\u0000nogroup';

// ------------------------------------------------------------------ api

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  const res = await fetch(path, opt);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  if (!res.ok) {
    const err = new Error((data && data.error) || `${res.status} ${res.statusText}`);
    err.status = res.status; err.code = data && data.code; throw err;
  }
  return data;
}
const GET = (p) => api('GET', p);
const POST = (p, b) => api('POST', p, b || {});
const PUT = (p, b) => api('PUT', p, b || {});
const PATCH = (p, b) => api('PATCH', p, b || {});
const DEL = (p) => api('DELETE', p);
const wsPath = (name) => `/api/v1/workstreams/${encodeURIComponent(name)}`;

// ------------------------------------------------------------------ helpers

const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];
function esc(s) { return String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }
function h(strings, ...vals) { return strings.reduce((out, str, i) => out + str + (i < vals.length ? (vals[i] && vals[i].__raw ? vals[i].s : esc(vals[i])) : ''), ''); }
const raw = (s) => ({ __raw: true, s });
function toast(msg, kind = '') {
  const t = document.createElement('div'); t.className = 'toast ' + kind; t.textContent = msg;
  $('#toasts').appendChild(t); setTimeout(() => t.remove(), kind === 'err' ? 9000 : 4000);
}
function ago(iso) {
  if (!iso) return '';
  const d = (Date.now() - new Date(iso).getTime()) / 1000;
  if (d < 60) return 'just now'; if (d < 3600) return `${Math.floor(d / 60)}m ago`;
  if (d < 86400) return `${Math.floor(d / 3600)}h ago`; return `${Math.floor(d / 86400)}d ago`;
}
function short(p) { return S.config && S.config.home && p && p.startsWith(S.config.home) ? '~' + p.slice(S.config.home.length) : p; }
function busy(btn, fn) {
  return async (...a) => { if (btn) btn.disabled = true; try { return await fn(...a); } catch (e) { toast(e.message, 'err'); } finally { if (btn) btn.disabled = false; } };
}
function fmtDate(d) {
  if (!d) return '';
  const [y, m, day] = d.split('-').map(Number);
  return new Date(y, m - 1, day).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}
function dateRange(g) {
  if (g.start && g.end) return `${fmtDate(g.start)} → ${fmtDate(g.end)}`;
  if (g.end) return `until ${fmtDate(g.end)}`;
  if (g.start) return `from ${fmtDate(g.start)}`;
  return '';
}
function daysLeftText(g) {
  if (g.days_left === undefined || g.days_left === null) return '';
  if (g.days_left < 0) return `ended ${-g.days_left}d ago`;
  if (g.days_left === 0) return 'ends today';
  if (g.days_left === 1) return 'ends tomorrow';
  return `${g.days_left}d left`;
}
// reportSummary mirrors Report.Summary() in internal/app/sync.go (minus the
// source name and duration) so the toast and the chip read like `jug sync`.
function reportSummary(r) {
  const parts = [];
  const add = (a, w) => { if (a && a.length) parts.push(`${a.length} ${w}`); };
  add(r.created, 'created'); add(r.adopted, 'adopted'); add(r.updated, 'updated'); add(r.regrouped, 'regrouped'); add(r.tombstoned, 'tombstoned'); add(r.refs, 'refs refreshed'); add(r.groups_registered, 'groups registered'); add(r.groups_updated, 'groups updated'); add(r.errors, 'errors');
  const tail = [`${r.unchanged} unchanged`]; if (r.finished && r.finished.length) tail.push(`${r.finished.length} finished skipped`); if (r.prunable && r.prunable.length) tail.push(`${r.prunable.length} prunable`);
  return (parts.length ? parts.join(', ') : 'nothing to do') + ` (${tail.join(', ')})`;
}
function stateGlyph(st) { return st === 'displayed' ? '●' : st === 'parked' ? '◐' : '○'; }
function refBadge(r) {
  const label = r.key || (r.type === 'url' ? new URL(r.url).host : r.type);
  return h`<a class="badge ref" href="${r.url}" target="_blank" rel="noopener" title="${r.title || r.url}">${r.type}:${label}${r.status ? raw(h`<span class="st">${r.status}</span>`) : ''}</a>`;
}

// ------------------------------------------------------------------ data loading

let refreshTimer = null;
function scheduleRefresh(ms = 120) { clearTimeout(refreshTimer); refreshTimer = setTimeout(refreshAll, ms); }

async function refreshAll() {
  try {
    const [status, items, groups, sources] = await Promise.all([GET('/api/v1/status'), GET('/api/v1/workstreams'), GET('/api/v1/groups?all=true'), GET('/api/v1/sources')]);
    S.status = status; S.items = items; S.groups = groups; S.sources = sources;
    if (S.selected && !items.find(i => i.name === S.selected)) { S.selected = null; S.detail = null; }
    render();
    if (S.selected) await loadDetail(S.selected, { keepTodo: true });
  } catch (e) { toast('refresh failed: ' + e.message, 'err'); }
}

async function loadConfig() {
  try {
    S.config = await GET('/api/v1/config');
    S.repos = S.config.repos || [];
    S.config.home = (S.config.root || '').replace(/\/workstreams\/?$/, '') || null;
    const envHome = S.config.root && S.config.root.match(/^(\/home\/[^/]+|\/Users\/[^/]+)/);
    if (envHome) S.config.home = envHome[1];
  } catch (e) { toast('config: ' + e.message, 'err'); }
}

async function loadDetail(name, { keepTodo = false } = {}) {
  const [detail, todo] = await Promise.all([GET(wsPath(name)), keepTodo && S.todo && S.todo.name === name && S.todo.dirty ? Promise.resolve(S.todo) : GET(wsPath(name) + '/todo').then(t => ({ name, text: t.text, open: t.open, dirty: false }))]);
  S.detail = detail; S.todo = todo;
  renderDetail();
  // session is slow (starts an opencode server): load lazily, after the pane is drawn
  loadSession(name);
}

const sessionCache = {};
async function loadSession(name, force = false) {
  if (!force && sessionCache[name] && Date.now() - sessionCache[name].t < 60000) { S.session = sessionCache[name].v; renderSession(); return; }
  S.session = { loading: true }; renderSession();
  try { const v = await GET(wsPath(name) + '/session'); sessionCache[name] = { t: Date.now(), v }; if (S.selected === name) { S.session = v; renderSession(); } }
  catch (e) { if (S.selected === name) { S.session = { error: e.message }; renderSession(); } }
}

// ------------------------------------------------------------------ render: header + list

function rowHTML(it) {
  const g = it.git;
  const branch = g ? (g.branch || ('@' + g.head)) + (g.dirty ? '*' : '') : '';
  const sess = it.opencode_session ? it.opencode_session.slice(-8) : '';
  const groups = S.view === 'flat' ? (it.groups || []).map(x => h`<span class="badge group">${x}</span>`).join('') : '';
  const jiraTitle = ((it.refs || []).find(r => r.type === 'jira' && r.title) || {}).title || '';
  // "no code" matters for something you work on by hand; a synced ticket you
  // have not started simply has none yet
  const nocode = !it.has_code && !it.source ? raw(' <span class="badge nocode" title="no code dir: terminals start in the workstream dir">no code</span>') : '';
  const finishedWhy = it.finished ? (it.completed ? 'completed by you ' + ago(it.completed) : 'ticket closed') + (it.state === 'none' ? '' : ' — still has windows; close them and it leaves the default view') : '';
  return h`<tr data-name="${it.name}" class="${it.state} ${S.selected === it.name ? 'selected' : ''} ${it.finished ? 'done' : ''}">
    <td class="state" title="${it.state}${finishedWhy ? ' · ' + finishedWhy : ''}">${it.finished ? '✓' : stateGlyph(it.state)}</td>
    <td class="id">${it.id}</td>
    <td><span class="badge cat-${it.category}">${it.category}</span>${raw(groups)}</td>
    <td class="desc" title="${jiraTitle}">${it.desc}${nocode}</td>
    <td class="owner">${it.owner ? raw(h`<span class="badge owner" title="assigned to ${it.owner}">${it.owner}</span>`) : ''}</td>
    <td>${raw((it.refs || []).map(refBadge).join(''))}</td>
    <td class="mono">${it.todos_open || ''}</td>
    <td class="mono" title="${g ? short(it.code_path) : ''}">${branch}${g && g.dirty ? raw(' <span class="badge dirty" title="uncommitted changes">dirty</span>') : ''}</td>
    <td class="mono dim" title="${it.opencode_session || ''}">${sess}</td>
    <td class="actions sm">${it.state === 'displayed' ? raw('<button data-act="focus" title="focus opencode">focus</button>') : raw('<button data-act="show" class="primary" title="show in the slot">show</button>')}${it.completed ? raw('<button data-act="reopen" class="icon" title="reopen (clear your completion mark)">↺</button>') : it.finished ? '' : raw('<button data-act="complete" class="icon" title="complete: mark done in juggler (the ticket is not touched)">✓</button>')}</td>
  </tr>`;
}

function groupRowHTML(g, n, collapsed) {
  const key = g ? g.name : NOGROUP;
  const meta = g ? [dateRange(g), daysLeftText(g)].filter(Boolean).join(' · ') : 'workstreams in no group';
  const cls = ['group-row', collapsed ? 'collapsed' : '', g && g.current ? 'current' : '', g && g.days_left !== undefined && g.days_left !== null && g.days_left < 0 ? 'past' : ''].join(' ');
  return h`<tr class="${cls}" data-group="${key}"><td colspan="10"><div>
    <span class="chev">▾</span><span class="gname">${g ? g.name : 'no group'}</span>
    ${g && g.url ? raw(h`<a class="glink" href="${g.url}" target="_blank" rel="noopener" title="${g.url}">↗</a>`) : ''}
    ${g && g.kind ? raw(h`<span class="badge kind">${g.kind}</span>`) : ''}
    ${g && g.source ? raw(h`<span class="badge src" title="maintained by source ${g.source}">${g.source}</span>`) : ''}
    ${g && g.current ? raw('<span class="badge current">current</span>') : ''}
    ${g && !g.current && g.days_left !== undefined && g.days_left !== null && g.days_left < 0 ? raw('<span class="badge ended">ended</span>') : ''}
    <span class="gmeta">${meta}</span>${g && g.desc ? raw(h`<span class="gmeta">— ${g.desc}</span>`) : ''}
    <span class="gcount">${n}</span>
    ${g ? raw('<button class="sm" data-gact="edit" title="edit this group">edit</button>') : ''}
  </div></td></tr>`;
}

// A filter toggle that would change nothing is greyed out and says why, so
// it cannot be mistaken for a broken one: "mine only" needs teammates' rows
// (an owner), "show finished" needs finished rows without windows (a
// finished workstream that still has windows is always listed, so that its
// windows can be closed from here).
function renderToggles() {
  const owned = S.items.filter(i => i.owner).length;
  const mine = $('#only-mine'), mineLabel = mine.closest('label');
  mine.disabled = owned === 0;
  mineLabel.classList.toggle('off', owned === 0);
  mineLabel.title = owned ? `hide the ${owned} teammates' ticket(s) (those with an owner)` : 'nothing to hide: no teammates\' tickets are loaded (the source pulls only yours — set assignee = "any" to pull the team\'s sprint)';
  const hideable = S.items.filter(i => i.finished && i.state === 'none').length;
  const fin = $('#show-finished'), finLabel = fin.closest('label');
  fin.disabled = hideable === 0;
  finLabel.classList.toggle('off', hideable === 0);
  $('#show-finished-n').textContent = hideable ? ` (${hideable})` : '';
  finLabel.title = hideable ? `${hideable} finished workstream(s) are hidden from this view (ticket closed, or completed by you)` : 'nothing to show: every finished workstream still has windows, so it is already listed (with a ✓) — close its windows and it drops out of this view';
}

function render() {
  renderStatus();
  const q = S.filter.trim().toLowerCase();
  let hiddenFinished = 0;
  const match = (it) => {
    if (S.onlyLive && it.state === 'none') return false;
    if (S.onlyMine && it.owner) return false;
    // finished (ticket closed, or completed by you) is hidden unless it still has windows
    if (!S.showFinished && it.finished && it.state === 'none') { hiddenFinished++; return false; }
    if (!q) return true;
    const hay = [it.id, it.desc, it.category, it.name, it.owner, it.source, it.git && it.git.branch, ...(it.groups || []), ...(it.refs || []).map(r => r.key + ' ' + r.status + ' ' + r.title)].join(' ').toLowerCase();
    return q.split(/\s+/).every(t => hay.includes(t));
  };
  const order = { displayed: 0, parked: 1, none: 2 };
  // live first; then yours before teammates'; open before finished; newest first
  const sortRows = (rows) => rows.sort((a, b) => (order[a.state] - order[b.state]) || (b.shown || '').localeCompare(a.shown || '') || ((a.owner ? 1 : 0) - (b.owner ? 1 : 0)) || ((a.finished ? 1 : 0) - (b.finished ? 1 : 0)) || (b.created || '').localeCompare(a.created || ''));
  const rows = sortRows(S.items.filter(match));
  const tb = $('#list tbody');
  if (S.view === 'flat') {
    tb.innerHTML = rows.map(rowHTML).join('');
  } else {
    const byName = Object.fromEntries(rows.map(r => [r.name, r]));
    let html = '';
    for (const g of S.groups) {
      const members = (g.members || []).map(n => byName[n]).filter(Boolean);
      if (!members.length) continue; // empty (or fully filtered out) groups are not shown
      const ended = g.days_left !== undefined && g.days_left !== null && g.days_left < 0;
      const collapsed = S.collapsed[g.name] === undefined ? ended : !!S.collapsed[g.name];
      html += groupRowHTML(g, members.length, collapsed);
      if (!collapsed) html += sortRows(members).map(rowHTML).join('');
    }
    const loose = rows.filter(r => !(r.groups || []).length);
    if (loose.length) {
      const collapsed = !!S.collapsed[NOGROUP];
      html += groupRowHTML(null, loose.length, collapsed);
      if (!collapsed) html += loose.map(rowHTML).join('');
    }
    tb.innerHTML = html;
  }
  $('#count').textContent = `${rows.length}/${S.items.length}` + (hiddenFinished ? ` · ${hiddenFinished} finished hidden` : '');
  renderToggles();
  $('#btn-view').textContent = S.view;
  $('#empty').classList.toggle('hidden', S.items.length > 0);
  $('#list').classList.toggle('hidden', S.items.length === 0);
}

function renderStatus() {
  const st = S.status; const el = $('#status');
  if (!st) { el.innerHTML = ''; return; }
  const chips = [];
  if (!st.sway) chips.push(h`<span class="chip err" title="${st.sway_error}">sway unavailable</span>`);
  if (st.slot) chips.push(h`<span class="chip ${st.on_slot ? 'blue' : ''}" title="original name ${st.slot.original_name}">slot: workspace <b>${st.slot.num}</b></span>`);
  else chips.push(h`<span class="chip warn">no slot — focus a workspace and press slot</span>`);
  const disp = S.items.find(i => i.name === st.displayed);
  chips.push(disp ? h`<span class="chip blue">displayed: <b>${disp.id}</b> ${disp.desc}</span>` : h`<span class="chip">nothing displayed</span>`);
  const parked = S.items.filter(i => i.state === 'parked').length;
  if (parked) chips.push(h`<span class="chip">lot: ${parked} parked</span>`);
  for (const g of S.groups.filter(g => g.current)) chips.push(h`<span class="chip current" title="${g.desc || g.kind || ''}">${g.kind || 'group'}: <b>${g.name}</b> · ${daysLeftText(g)}</span>`);
  for (const src of S.sources) {
    const bad = src.error || src.last_error;
    const when = src.running ? 'syncing…' : src.last_run ? ago(src.last_run) : 'never';
    const next = src.next_run && !src.running ? ` · next ${new Date(src.next_run).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })}` : '';
    const prunable = src.last_report && src.last_report.prunable && src.last_report.prunable.length;
    const tip = (src.describe || '') + (bad ? '\n' + bad : '') + (src.last_report ? '\nlast: ' + reportSummary(src.last_report) : '') + (prunable ? `\n${prunable} untouched leftover(s) — jug source prune ${src.name}` : '') + '\nclick to sync now';
    chips.push(h`<button class="chip sync ${bad ? 'err' : ''} ${src.running ? 'busy' : ''}" data-sync="${src.name}" title="${tip}">⟳ ${src.name}: ${when}${next}</button>`);
  }
  if (st.focused_workspace) chips.push(h`<span class="chip" title="focused workspace">on ${st.focused_workspace}</span>`);
  el.innerHTML = chips.join('');
  $('#btn-park').disabled = !st.displayed;
  $('#btn-lot').disabled = !st.lot;
  $('#btn-toggle').textContent = st.slot && st.on_slot ? 'release slot' : 'slot here';
}

// ------------------------------------------------------------------ render: detail

function renderDetail() {
  const d = S.detail, pane = $('#detail-pane');
  if (!d) { pane.classList.add('hidden'); pane.innerHTML = ''; return; }
  pane.classList.remove('hidden');
  const g = d.git;
  const jira = (d.refs || []).find(r => r.type === 'jira');
  const pr = (d.refs || []).find(r => r.type === 'pr');
  const live = d.state !== 'none';
  pane.innerHTML = h`
    <div class="d-head"><h2>${d.id}</h2><span class="desc">${d.desc}</span>
      <span class="badge cat-${d.category}">${d.category}</span>
      ${raw((d.groups || []).map(x => h`<span class="badge group">${x}</span>`).join(''))}
      <span class="badge ${d.state}">${stateGlyph(d.state)} ${d.state}${d.shown ? ' · ' + ago(d.shown) : ''}</span>${d.finished ? raw(h`<span class="badge done" title="${d.completed ? 'completed by you ' + ago(d.completed) : 'the ticket is closed'}">✓ ${d.completed ? 'completed' : 'ticket closed'}</span>`) : ''}</div>
    <div class="d-sub">${short(d.dir)}${d.source ? raw(h` · <span class="badge src" title="created/maintained by source ${d.source}: ticket status, title and sprint tags are refreshed by it">source: ${d.source}</span>`) : ''}${d.owner ? raw(h` · <span class="badge owner">assigned to ${d.owner}</span>`) : ''}</div>
    <div class="d-actions sm">
      ${d.state === 'displayed' ? raw('<button data-act="focus">focus opencode</button><button data-act="park">park</button>') : raw('<button data-act="show" class="primary">show</button>')}
      <button data-act="term" title="terminal in the right stack">term</button>
      <button data-act="notes" title="TODO.md in the editor">notes</button>
      <button data-act="review" title="uncommitted diff in the editor" ${g ? '' : 'disabled'}>review</button>
      ${jira ? raw('<button data-act="open-jira">open jira</button>') : ''}
      ${pr ? raw('<button data-act="open-pr">open pr</button>') : ''}
      <button data-act="dictate" title="toggle dictation notes">dictate</button>
      ${live ? raw('<button data-act="close" title="kill its windows; files are kept">close windows</button>') : ''}
      ${d.completed ? raw('<button data-act="reopen" title="clear your completion mark">reopen</button>') : d.finished ? '' : raw('<button data-act="complete" title="mark done in juggler; the ticket is not touched. Hidden from the picker and the default view">✓ complete</button>')}
    </div>

    <fieldset><legend>edit</legend>
      <form id="f-edit" class="grid">
        <label>category</label><input name="category" list="cats" value="${d.category}">
        <label>id</label><input name="id" value="${d.id}">
        <label>description</label><input name="desc" value="${d.desc}">
        <label>jira key</label><input name="jira" value="${jira ? jira.key : ''}" placeholder="attach a ticket (sets id + work unless given)">
        <span></span><div class="row"><button class="primary">save</button><span class="muted small">changing these renames the directory; windows are closed and reopened${d.source ? '; the source keeps the ticket ref and sprint tags, the rest is yours' : ''}</span></div>
      </form>
    </fieldset>

    <fieldset><legend>code</legend>
      ${g ? raw(h`<dl class="kv">
          <dt>path</dt><dd>${short(d.code_path)}${d.code_inside ? ' (inside)' : ''}</dd>
          ${g.root && g.root !== d.code_path ? raw(h`<dt>checkout</dt><dd title="the code dir is a subdirectory; seeds, the dirty check and removal address the checkout">${short(g.root)}</dd>`) : ''}
          <dt>branch</dt><dd>${g.branch || '(detached)'} @ ${g.head}${g.dirty ? raw(' <span class="badge dirty">dirty</span>') : ''}</dd>
          ${g.upstream ? raw(h`<dt>upstream</dt><dd>${g.upstream} ${g.ahead > 0 ? '↑' + g.ahead : ''} ${g.behind > 0 ? '↓' + g.behind : ''}</dd>`) : ''}
          ${g.linked ? raw(h`<dt>worktree of</dt><dd>${short(g.main)}</dd>`) : ''}
        </dl>
        <form id="f-subdir" class="row sm" style="margin-top:8px"><span class="muted">start windows in</span><input name="subdir" value="${subdirOf(d, g)}" placeholder="." style="width:280px" title="a path inside the checkout; . = its root"><button>move</button>${d.state !== 'none' ? raw('<span class="muted small">open windows keep the old dir until closed and shown again</span>') : ''}</form>
        ${d.code_inside ? raw('<div class="row sm" style="margin-top:8px"><button data-act="seed">re-seed files</button><button data-act="seed-force" title="overwrite existing seeded files">re-seed (force)</button></div>') : ''}`)
        : raw(h`<div class="muted small">${d.has_code ? h`code dir ${short(d.code_path)} is not a git checkout` : 'no code dir — terminals and opencode start in the workstream directory'}</div>`)}
      ${!d.code_inside ? raw(worktreeForm()) : ''}
    </fieldset>

    <fieldset><legend>refs</legend>
      <ul class="reflist">${raw((d.refs || []).map(r => h`<li><span class="t">${r.type}</span><a class="k" href="${r.url}" target="_blank" rel="noopener">${r.key || r.url}</a><span class="ttl" title="${r.title}">${r.status ? '[' + r.status + '] ' : ''}${r.title || ''}</span>
          <button class="sm" data-act="open-ref" data-what="${r.type}" title="open in the workstream's right stack">open</button>
          <button class="sm icon danger" data-act="ref-rm" data-type="${r.type}" data-key="${r.key || r.url}" title="remove">×</button></li>`).join(''))}
        ${(d.refs || []).length ? '' : raw('<li class="muted small">none</li>')}</ul>
      <form id="f-ref" class="row sm">
        <select name="type"><option>jira</option><option>pr</option><option>issue</option><option>url</option></select>
        <input name="value" placeholder="AISW-123 or https://…" required>
        <input name="title" placeholder="title (optional)" style="width:140px">
        <input name="status" placeholder="status" style="width:80px">
        <button class="primary">add</button>
      </form>
    </fieldset>

    <fieldset><legend>groups</legend>
      <div class="gchecks">${raw(S.groups.map(g => h`<label><input type="checkbox" data-gcheck="${g.name}" ${(d.groups || []).includes(g.name) ? 'checked' : ''}> <span class="mono">${g.name}</span> <span class="muted">${[g.kind, dateRange(g), g.current ? 'current' : ''].filter(Boolean).join(' · ')}</span></label>`).join(''))}
        ${S.groups.length ? '' : raw('<span class="muted small">no groups yet</span>')}</div>
      <form id="f-group" class="row sm"><input name="name" placeholder="new group name" style="flex:1;min-width:160px"><button class="primary">add to group</button></form>
    </fieldset>

    <fieldset><legend>TODO.md <span class="muted" id="todo-open"></span></legend>
      <textarea id="todo" spellcheck="false"></textarea>
      <div class="row sm" style="margin-top:6px"><button id="todo-save" class="primary">save</button><button id="todo-reload">reload</button><span id="todo-state" class="muted small"></span></div>
    </fieldset>

    <fieldset><legend>opencode session</legend><div id="session"></div></fieldset>

    <fieldset><legend>remove</legend>
      <div class="row sm"><button data-act="plan" class="danger">remove workstream…</button><span class="muted small">closes windows, removes the worktree (branch kept), deletes the directory</span></div>
      <div id="plan"></div>
    </fieldset>
    <datalist id="cats">${raw(((S.config && S.config.categories) || ['work', 'personal']).map(c => h`<option value="${c}">`).join(''))}</datalist>`;

  // TODO editor
  const ta = $('#todo'); ta.value = S.todo ? S.todo.text : '';
  $('#todo-open').textContent = S.todo ? `(${S.todo.open} open)` : '';
  $('#todo-state').textContent = S.todo && S.todo.dirty ? 'unsaved changes' : '';
  ta.addEventListener('input', () => { S.todo.text = ta.value; S.todo.dirty = true; $('#todo-state').textContent = 'unsaved changes'; });
  renderSession();
}

// subdirOf is the code dir's path inside its checkout ("." at the root).
function subdirOf(d, g) {
  if (!g || !g.root || !d.code_path || !d.code_path.startsWith(g.root)) return '.';
  const rel = d.code_path.slice(g.root.length).replace(/^\/+/, '');
  return rel || '.';
}

// repoSubdir is the configured default subdir of a repo, for the forms' placeholder.
function repoSubdir(name) { const r = S.repos.find(x => x.name === name); return (r && r.subdir) || ''; }

function worktreeForm() {
  if (!S.repos.length) return '<div class="muted small" style="margin-top:8px">no repos configured ([repos.&lt;name&gt;] in config.toml) — add a worktree from the CLI with a path: <code>jug repo add --repo /path/to/checkout</code></div>';
  return h`<form id="f-wt" class="row sm" style="margin-top:8px">
    <span class="muted">add worktree of</span>
    <select name="repo">${raw(S.repos.map(r => h`<option value="${r.name}" ${r.ok ? '' : 'disabled'}>${r.name}${r.ok ? '' : ' (missing)'}</option>`).join(''))}</select>
    <input name="branch" placeholder="branch (default: id or user/slug)" style="width:220px">
    <input name="base" placeholder="base (default: remote HEAD)" style="width:170px">
    <input name="subdir" placeholder="${repoSubdir(S.repos[0].name) ? 'subdir (default ' + repoSubdir(S.repos[0].name) + ')' : 'subdir (default: root)'}" style="width:200px" title="where windows start inside the worktree; . = the root">
    <label class="chk"><input type="checkbox" name="no_fetch"> no fetch</label>
    <button class="primary">add</button></form>`;
}

function renderSession() {
  const el = $('#session'); if (!el || !S.detail) return;
  const s = S.session, pinned = S.detail.opencode_session;
  let body = '';
  if (!s || s.loading) body = `<div class="muted small">${pinned ? 'pinned ' + esc(pinned) + ' — asking opencode…' : 'no session pinned (one is created on the next show)'}</div>`;
  else if (s.error) body = h`<div class="sess"><span class="id">${pinned || '(none)'}</span> <span class="err">${s.error}</span></div>`;
  else if (!s.pinned) body = '<div class="muted small">no session pinned (one is created on the next show)</div>';
  else if (!s.session) body = h`<div class="sess"><span class="id">${s.pinned}</span> <span class="warn">not found in opencode</span></div>`;
  else body = h`<div class="sess"><div><b>${s.session.title}</b></div><div class="muted small"><span class="id">${s.session.id}</span> · ${short(s.session.directory)} · updated ${ago(new Date(s.session.time.updated).toISOString())}</div></div>`;
  el.innerHTML = body + `
    <div class="row sm" style="margin-top:8px">
      <button data-act="cands">choose existing…</button>
      <button data-act="sess-new">new session</button>
      ${pinned ? '<button data-act="relaunch" title="restart the opencode window on the pinned session">relaunch opencode</button><button data-act="unpin">unpin</button>' : ''}
      <button data-act="sess-refresh" class="icon" title="refresh">↻</button>
    </div><div id="cands"></div>`;
  if (S.cands && S.cands.name === S.detail.name) renderCands();
}

function renderCands() {
  const el = $('#cands'); if (!el) return;
  const c = S.cands;
  if (c.loading) { el.innerHTML = '<div class="muted small" style="margin-top:6px">listing sessions (starts an opencode server)…</div>'; return; }
  el.innerHTML = `<div class="row sm" style="margin-top:8px"><label class="chk"><input type="checkbox" id="cands-all" ${S.candsAll ? 'checked' : ''}> all sessions (not only those mentioning this workstream)</label>
    <label class="chk"><input type="checkbox" id="cands-relaunch" checked> relaunch opencode on pick</label></div>
    <ul class="cands">${c.sessions.length ? c.sessions.map(s => h`<li data-id="${s.id}" class="${s.id === S.detail.opencode_session ? 'pinned' : ''}" title="${s.id}"><span class="when">${new Date(s.time.updated).toLocaleString(undefined, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })}</span><span>${s.title}<div class="muted small">${short(s.directory)}</div></span></li>`).join('') : '<li class="muted small" style="display:block">nothing matches — try "all sessions"</li>'}</ul>`;
}

// ------------------------------------------------------------------ actions

async function act(name, what, el) {
  const p = wsPath(name);
  const run = busy(el, async () => {
    switch (what) {
      case 'show': await POST(p + '/show', { no_switch: false }); toast(`showing ${name}`, 'ok'); break;
      case 'focus': await POST(p + '/focus'); break;
      case 'park': await POST('/api/v1/slot/park'); toast('parked', 'ok'); break;
      case 'close': if (!confirm(`Close all windows of ${name}? Files are kept.`)) return; await POST(p + '/close'); toast('windows closed', 'ok'); break;
      case 'term': await POST(p + '/term', {}); toast('terminal opened', 'ok'); break;
      case 'notes': await POST(p + '/notes'); break;
      case 'review': await POST(p + '/review'); break;
      case 'dictate': await POST(p + '/dictate'); toast('dictation toggled', 'ok'); break;
      case 'open-jira': { const r = await POST(p + '/open', { what: 'jira' }); toast(r.queued ? 'queued: opens when the workstream is shown' : 'opened', 'ok'); break; }
      case 'open-pr': { const r = await POST(p + '/open', { what: 'pr' }); toast(r.queued ? 'queued: opens when the workstream is shown' : 'opened', 'ok'); break; }
      case 'open-ref': { const r = await POST(p + '/open', { what: el.dataset.what }); toast(r.queued ? 'queued: opens when the workstream is shown' : 'opened', 'ok'); break; }
      case 'ref-rm': if (!confirm(`Remove ${el.dataset.type} ref ${el.dataset.key}?`)) return; await DEL(p + `/refs/${encodeURIComponent(el.dataset.type)}?key=${encodeURIComponent(el.dataset.key)}`); break;
      case 'seed': { const r = await POST(p + '/seed', { force: false }); toast(r.seeded.length ? 'seeded ' + r.seeded.join(', ') : 'nothing to seed (files already present)', 'ok'); break; }
      case 'seed-force': { const r = await POST(p + '/seed', { force: true }); toast('re-seeded ' + r.seeded.join(', '), 'ok'); break; }
      case 'cands': S.cands = { name, loading: true, sessions: [] }; renderCands(); S.cands = { name, sessions: (await GET(p + '/session/candidates?all=' + S.candsAll)).sessions || [] }; renderCands(); return;
      case 'sess-new': if (!confirm('Create a fresh opencode session for this workstream and pin it?')) return; await PUT(p + '/session', { new: true, relaunch: S.detail.state === 'displayed' }); delete sessionCache[name]; toast('new session pinned', 'ok'); break;
      case 'unpin': await DEL(p + '/session'); delete sessionCache[name]; toast('unpinned', 'ok'); break;
      case 'relaunch': await POST(p + '/session/relaunch'); toast('opencode relaunched', 'ok'); break;
      case 'sess-refresh': await loadSession(name, true); return;
      case 'complete': await POST(p + '/complete'); toast(`${name}: completed`, 'ok'); break;
      case 'reopen': await POST(p + '/reopen'); toast(`${name}: reopened`, 'ok'); break;
      case 'plan': { const plan = await GET(p + '/plan'); showPlan(plan); return; }
    }
    scheduleRefresh(50);
  });
  await run();
}

function showPlan(plan) {
  const el = $('#plan');
  el.innerHTML = h`<pre class="plan">would remove ${plan.name}
  windows:   closed
${plan.has_worktree ? raw(h`  worktree:  ${short(plan.worktree)} (branch ${plan.branch}${plan.dirty ? raw(h`, <span class="warn">DIRTY: ${plan.dirty} — needs force</span>`) : ''}) removed; the branch is kept\n`) : ''}  directory: ${short(plan.dir)} deleted (TODO.md, notes/, …)</pre>
    <div class="row sm"><label class="chk"><input type="checkbox" id="rm-force" ${plan.dirty ? '' : 'disabled'}> force (discard uncommitted changes)</label>
    <button id="rm-go" class="danger">remove ${plan.name}</button><button id="rm-cancel">cancel</button></div>`;
  $('#rm-cancel').onclick = () => { el.innerHTML = ''; };
  $('#rm-go').onclick = busy($('#rm-go'), async () => {
    if (!confirm(`Really remove ${plan.name}? This deletes the directory.`)) return;
    await DEL(wsPath(plan.name) + '?force=' + ($('#rm-force').checked ? 'true' : 'false'));
    toast(`removed ${plan.name}`, 'ok'); S.selected = null; S.detail = null; renderDetail(); scheduleRefresh(50);
  });
}

async function select(name) {
  S.selected = name; S.cands = null; render();
  try { await loadDetail(name); } catch (e) { toast(e.message, 'err'); }
  if (window.innerWidth <= 1100) $('#detail-pane').scrollIntoView({ behavior: 'smooth' });
}

// ------------------------------------------------------------------ create modal

function openModal(title, bodyHTML, wide = false) { $('#modal-title').textContent = title; $('#modal-body').innerHTML = bodyHTML; $('#modal .modal-box').classList.toggle('wide', wide); $('#modal').classList.remove('hidden'); }
function closeModal() { $('#modal').classList.add('hidden'); $('#modal-body').innerHTML = ''; }

function openCreate() {
  const repos = S.repos.filter(r => r.ok);
  openModal('new workstream', `
    <form id="f-new" class="grid">
      <label>description</label><input name="desc" required autofocus placeholder="what this is about">
      <label>ticket key</label><input name="jira" placeholder="AISW-123 (becomes the id and a jira ref; implies work)">
      <label>category</label><input name="category" list="cats2" placeholder="${esc(S.config ? S.config.default_category : 'personal')} unless a ticket is given">
      <label>id</label><input name="id" placeholder="optional; default: ticket key or ${esc(S.config ? S.config.id_prefix : 'me')}-NNNN">
      <label>pull request</label><input name="pr" type="url" placeholder="https://… (optional)">
      <label>groups</label><div class="row sm">${S.groups.map(g => `<label class="chk"><input type="checkbox" name="groups" value="${esc(g.name)}" ${g.current ? 'checked' : ''}> <span class="mono">${esc(g.name)}</span></label>`).join('')}<input name="newgroup" placeholder="or a new group" style="width:160px"></div>
      <label>code</label>
      <div class="radio-group">
        <label><input type="radio" name="code" value="repo" ${repos.length ? 'checked' : 'disabled'}> worktree of
          <select name="repo" ${repos.length ? '' : 'disabled'}>${repos.map(r => `<option value="${esc(r.name)}">${esc(r.name)}</option>`).join('')}</select></label>
        <div class="sub row sm"><input name="branch" placeholder="branch (default: id or user/slug)" style="width:220px"><input name="base" placeholder="base (default: remote HEAD)" style="width:170px"><input name="subdir" placeholder="${repos.length && repoSubdir(repos[0].name) ? 'subdir (default ' + esc(repoSubdir(repos[0].name)) + ')' : 'subdir (default: root)'}" style="width:200px" title="where windows start inside the worktree; . = the root"><label class="chk"><input type="checkbox" name="no_fetch"> no fetch</label></div>
        <label><input type="radio" name="code" value="dir"> existing directory <input name="code_dir" placeholder="~/src/thing" style="flex:1"></label>
        <label><input type="radio" name="code" value="none" ${repos.length ? '' : 'checked'}> no code — notes only</label>
      </div>
      <span></span><label class="chk"><input type="checkbox" name="show" checked> show it now</label>
    </form>
    <datalist id="cats2">${((S.config && S.config.categories) || ['work', 'personal']).map(c => `<option value="${esc(c)}">`).join('')}</datalist>
    <div class="modal-foot"><button id="new-cancel">cancel</button><button id="new-go" class="primary">create</button></div>`);
  $('#new-cancel').onclick = closeModal;
  const go = busy($('#new-go'), async () => {
    const f = $('#f-new'); const fd = new FormData(f);
    const body = { desc: fd.get('desc'), jira: fd.get('jira'), category: fd.get('category'), id: fd.get('id'), pr: fd.get('pr'), show: fd.get('show') === 'on' };
    body.groups = fd.getAll('groups'); if ((fd.get('newgroup') || '').trim()) body.groups.push(fd.get('newgroup').trim());
    const code = fd.get('code');
    if (code === 'repo') { body.repo = fd.get('repo'); body.branch = fd.get('branch'); body.base = fd.get('base'); body.subdir = (fd.get('subdir') || '').trim(); body.no_fetch = fd.get('no_fetch') === 'on'; }
    if (code === 'dir') { body.code_dir = fd.get('code_dir'); if (!body.code_dir) throw new Error('directory is required'); }
    if (!body.desc.trim()) throw new Error('a description is required');
    const r = await POST('/api/v1/workstreams', body);
    closeModal();
    (r.warnings || []).forEach(w => toast(w, 'err'));
    toast(`created ${r.workstream.name}` + (r.worktree ? ` — ${r.worktree.what}` : ''), 'ok');
    await refreshAll(); await select(r.workstream.name);
  });
  $('#new-go').onclick = go;
  $('#f-new').onsubmit = (e) => { e.preventDefault(); go(); };
  const repoSel = $('#f-new [name=repo]');
  if (repoSel) repoSel.onchange = () => { const d = repoSubdir(repoSel.value); $('#f-new [name=subdir]').placeholder = d ? 'subdir (default ' + d + ')' : 'subdir (default: root)'; };
  setTimeout(() => $('#f-new [name=desc]').focus(), 30);
}

// ------------------------------------------------------------------ groups modal

function groupEditRow(g) {
  const isNew = !g;
  g = g || { name: '', kind: '', start: '', end: '', desc: '', count: 0 };
  return h`<tr data-gname="${g.name}" class="${isNew ? 'gnew' : ''}">
    <td>${isNew ? raw('<input class="gname" name="name" placeholder="name (e.g. a sprint)">') : raw(h`<span class="mono">${g.name}</span>${g.current ? raw(' <span class="badge current">current</span>') : ''}`)}</td>
    <td><input class="gkind" name="kind" value="${g.kind || ''}" placeholder="sprint" list="kinds"></td>
    <td><input type="date" name="start" value="${g.start || ''}"></td>
    <td><input type="date" name="end" value="${g.end || ''}"></td>
    <td><input class="gdesc" name="desc" value="${g.desc || ''}" placeholder="description"></td>
    <td><input class="gurl" name="url" type="url" value="${g.url || ''}" placeholder="https://… (external reference)"></td>
    <td class="mono muted" title="${isNew ? '' : g.source ? 'maintained by source ' + g.source : 'made by hand'}">${isNew ? '' : (g.source || '')}</td>
    <td class="mono muted" title="members">${isNew ? '' : g.count}</td>
    <td class="actions sm">${isNew ? raw('<button class="primary" data-gact="create">create</button>') : raw(h`<button class="primary" data-gact="save">save</button><button class="danger icon" data-gact="delete" title="forget this group's metadata${g.count ? ' (members keep the tag unless you confirm untagging)' : ''}">×</button>`)}</td>
  </tr>`;
}

function openGroups(focusName) {
  openModal('groups', `
    <p class="hint" style="margin:0 0 10px">A group is any bucket — a sprint, a project, a date range. Membership is a tag on each workstream (tick groups in a workstream's detail pane); this is the metadata: kind, dates (groups are ordered by end date), description, link. Groups without members are hidden from the list but kept here. Groups with a source are rewritten by it on every sync — edits here last until then.</p>
    <table class="groups"><thead><tr><th>name</th><th>kind</th><th>start</th><th>end</th><th>description</th><th>link</th><th>source</th><th>#</th><th></th></tr></thead>
    <tbody>${S.groups.map(g => groupEditRow(g)).join('')}${groupEditRow(null)}</tbody></table>
    <datalist id="kinds"><option value="sprint"><option value="project"><option value="bucket"><option value="dates"></datalist>
    <div class="modal-foot"><button id="groups-close">close</button></div>`, true);
  $('#groups-close').onclick = closeModal;
  if (focusName) { const row = $(`#modal tr[data-gname="${CSS.escape(focusName)}"]`); if (row) { row.querySelector('input[name=end]').focus(); } }
}

async function groupAction(act, tr) {
  const name = tr.dataset.gname || (tr.querySelector('input[name=name]') || {}).value;
  const body = {};
  for (const k of ['kind', 'start', 'end', 'desc', 'url']) body[k] = tr.querySelector(`input[name=${k}]`).value.trim();
  if (act === 'create' || act === 'save') {
    if (!name || !name.trim()) throw new Error('a group name is required');
    await PUT(`/api/v1/groups/${encodeURIComponent(name.trim())}`, body);
    toast(act === 'create' ? `group ${name.trim()} created — tag workstreams into it from their detail pane` : `saved ${name}`, 'ok');
  } else if (act === 'delete') {
    const g = S.groups.find(x => x.name === name);
    let untag = false;
    if (g && g.count) { untag = confirm(`${name} has ${g.count} member(s).\n\nOK: remove the tag from them too.\nCancel: keep the tags, only forget the metadata.`); }
    else if (!confirm(`Forget group ${name}?`)) return;
    const r = await DEL(`/api/v1/groups/${encodeURIComponent(name)}?untag=${untag}`);
    toast(`removed ${name}` + (r.untagged ? ` (untagged ${r.untagged})` : ''), 'ok');
  }
  await refreshAll();
  openGroups();
}

// ------------------------------------------------------------------ wiring

document.addEventListener('click', async (e) => {
  const sbtn = e.target.closest('button[data-sync]');
  if (sbtn) {
    const name = sbtn.dataset.sync;
    await busy(sbtn, async () => {
      sbtn.classList.add('busy'); sbtn.textContent = `⟳ ${name}: syncing…`;
      const r = await api('POST', `/api/v1/sources/${encodeURIComponent(name)}/sync`, {});
      (r.reports || []).forEach(rep => toast(`${rep.source}: ${reportSummary(rep)}` + (rep.errors && rep.errors.length ? '\n' + rep.errors.join('\n') : ''), rep.errors && rep.errors.length ? 'err' : 'ok'));
      scheduleRefresh(50);
    })();
    return;
  }
  const gbtn = e.target.closest('button[data-gact]');
  if (gbtn) {
    e.stopPropagation();
    const tr = gbtn.closest('tr');
    if (gbtn.dataset.gact === 'edit') { openGroups(tr.dataset.group); return; }
    await busy(gbtn, () => groupAction(gbtn.dataset.gact, tr))();
    return;
  }
  const grow = e.target.closest('tr.group-row');
  if (grow && !e.target.closest('a')) {
    const key = grow.dataset.group;
    S.collapsed[key] = !grow.classList.contains('collapsed');
    localStorage.setItem('jug.collapsed', JSON.stringify(S.collapsed));
    render();
    return;
  }
  const btn = e.target.closest('button[data-act]');
  if (btn) {
    const tr = btn.closest('tr[data-name]');
    const name = tr ? tr.dataset.name : (S.detail && S.detail.name);
    if (name) { e.stopPropagation(); await act(name, btn.dataset.act, btn); }
    return;
  }
  const row = e.target.closest('#list tbody tr[data-name]');
  if (row && !e.target.closest('a')) select(row.dataset.name);
  const cand = e.target.closest('.cands li[data-id]');
  if (cand && S.detail) {
    const relaunch = $('#cands-relaunch') && $('#cands-relaunch').checked;
    await busy(null, async () => { await PUT(wsPath(S.detail.name) + '/session', { id: cand.dataset.id, relaunch }); delete sessionCache[S.detail.name]; S.cands = null; toast('session pinned' + (relaunch ? ' and opencode relaunched' : ''), 'ok'); scheduleRefresh(50); })();
  }
});
document.addEventListener('change', async (e) => {
  if (e.target.id === 'cands-all' && S.detail) { S.candsAll = e.target.checked; await act(S.detail.name, 'cands', null); }
  if (e.target.dataset && e.target.dataset.gcheck !== undefined && S.detail) {
    const groups = $$('#detail-pane input[data-gcheck]').filter(c => c.checked).map(c => c.dataset.gcheck);
    await busy(null, async () => { await PATCH(wsPath(S.detail.name), { groups }); toast('groups saved', 'ok'); await refreshAll(); })();
  }
});
document.addEventListener('submit', async (e) => {
  const f = e.target; if (!S.detail) return;
  // form.id would be the <input name="id"> (named controls shadow form properties)
  const fid = f.getAttribute('id');
  const p = wsPath(S.detail.name); const fd = new FormData(f);
  if (fid === 'f-edit') {
    e.preventDefault();
    await busy(f.querySelector('button'), async () => {
      const body = {}; for (const k of ['category', 'id', 'desc', 'jira']) { const v = (fd.get(k) || '').trim(); if (v) body[k] = v; }
      const cur = S.detail; const jira = (cur.refs || []).find(r => r.type === 'jira');
      if (body.category === cur.category) delete body.category; if (body.id === cur.id) delete body.id; if (body.desc === cur.desc) delete body.desc; if (jira && body.jira === jira.key) delete body.jira;
      if (!Object.keys(body).length) { toast('nothing changed'); return; }
      const r = await PATCH(p, body);
      (r.result.warnings || []).forEach(w => toast(w, 'err'));
      toast(r.result.moved ? `${r.result.old_name} → ${r.result.name}` : 'saved', 'ok');
      S.selected = r.workstream.name; await refreshAll(); await loadDetail(S.selected);
    })();
  } else if (fid === 'f-ref') {
    e.preventDefault();
    await busy(f.querySelector('button'), async () => { await POST(p + '/refs', { type: fd.get('type'), value: fd.get('value'), title: fd.get('title'), status: fd.get('status') }); toast('ref added', 'ok'); await loadDetail(S.detail.name, { keepTodo: true }); scheduleRefresh(50); })();
  } else if (fid === 'f-group') {
    e.preventDefault();
    const name = (fd.get('name') || '').trim(); if (!name) return;
    await busy(f.querySelector('button'), async () => { await POST(`/api/v1/groups/${encodeURIComponent(name)}/members`, { workstreams: [S.detail.name] }); toast(`added to ${name}`, 'ok'); await refreshAll(); await loadDetail(S.detail.name, { keepTodo: true }); })();
  } else if (fid === 'f-wt') {
    e.preventDefault();
    await busy(f.querySelector('button'), async () => { const r = await POST(p + '/repo', { repo: fd.get('repo'), branch: fd.get('branch'), base: fd.get('base'), subdir: (fd.get('subdir') || '').trim(), no_fetch: fd.get('no_fetch') === 'on' }); toast(r.worktree.what + (r.worktree.code_dir !== r.worktree.path ? ' · windows start in ' + short(r.worktree.code_dir) : '') + (r.worktree.seeded && r.worktree.seeded.length ? ' · seeded ' + r.worktree.seeded.join(', ') : ''), 'ok'); if (S.detail.state !== 'none') toast('windows already open still use the old directory: close and show again to start them in the worktree'); await loadDetail(S.detail.name, { keepTodo: true }); scheduleRefresh(50); })();
  } else if (fid === 'f-subdir') {
    e.preventDefault();
    await busy(f.querySelector('button'), async () => {
      const sub = (fd.get('subdir') || '').trim() || '.';
      const r = await PATCH(p, { subdir: sub });
      toast(r.result.code_dir ? 'windows now start in ' + short(r.result.code_dir) : 'already there', 'ok');
      await loadDetail(S.detail.name, { keepTodo: true }); scheduleRefresh(50);
    })();
  }
});
document.addEventListener('click', async (e) => {
  if (e.target.id === 'todo-save' && S.detail) { await busy(e.target, async () => { const r = await PUT(wsPath(S.detail.name) + '/todo', { text: $('#todo').value }); S.todo = { name: S.detail.name, text: $('#todo').value, open: r.open, dirty: false }; $('#todo-open').textContent = `(${r.open} open)`; $('#todo-state').textContent = 'saved'; toast('TODO.md saved', 'ok'); scheduleRefresh(50); })(); }
  if (e.target.id === 'todo-reload' && S.detail) { S.todo = null; await loadDetail(S.detail.name); }
});

$('#btn-new').onclick = openCreate;
$('#btn-groups').onclick = () => openGroups();
$('#btn-view').onclick = () => { S.view = S.view === 'grouped' ? 'flat' : 'grouped'; localStorage.setItem('jug.view', S.view); render(); };
$('#empty-new').onclick = (e) => { e.preventDefault(); openCreate(); };
$('#btn-toggle').onclick = busy($('#btn-toggle'), async () => { const r = await POST('/api/v1/slot/toggle'); toast(r.on ? `workspace ${r.slot.num} is now the slot` : 'slot released', 'ok'); scheduleRefresh(50); });
$('#btn-park').onclick = busy($('#btn-park'), async () => { await POST('/api/v1/slot/park'); toast('parked', 'ok'); scheduleRefresh(50); });
$('#btn-lot').onclick = busy($('#btn-lot'), async () => { await POST('/api/v1/lot'); });
// Relaunch every opencode: it kills running TUIs (the sessions survive in
// opencode's store and resume), so ask first and say how many.
$('#btn-relaunch').onclick = busy($('#btn-relaunch'), async () => {
  const live = S.items.filter(i => i.state !== 'none').length;
  if (!live) { toast('no workstream has windows', 'err'); return; }
  if (!confirm(`Restart the opencode window of every live workstream (${live})?\n\nEach comes back on its pinned session, parked ones in place in the lot. Whatever a session was in the middle of is interrupted; the sessions themselves are kept.`)) return;
  const r = await POST('/api/v1/relaunch', {});
  const parts = [`${r.relaunched.length} relaunched`];
  if (r.skipped.length) parts.push(`${r.skipped.length} skipped`);
  if (r.errors && r.errors.length) parts.push(`${r.errors.length} failed`);
  toast(parts.join(', ') + (r.errors && r.errors.length ? '\n' + r.errors.join('\n') : ''), r.errors && r.errors.length ? 'err' : 'ok');
  scheduleRefresh(50);
});
$('#modal-close').onclick = closeModal;
$('#modal').addEventListener('click', (e) => { if (e.target.id === 'modal') closeModal(); });
$('#filter').addEventListener('input', (e) => { S.filter = e.target.value; render(); });
$('#only-live').addEventListener('change', (e) => { S.onlyLive = e.target.checked; render(); });
$('#show-finished').checked = S.showFinished;
$('#show-finished').addEventListener('change', (e) => { S.showFinished = e.target.checked; localStorage.setItem('jug.finished', S.showFinished ? '1' : '0'); render(); });
$('#only-mine').checked = S.onlyMine;
$('#only-mine').addEventListener('change', (e) => { S.onlyMine = e.target.checked; localStorage.setItem('jug.mine', S.onlyMine ? '1' : '0'); render(); });
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') { if (!$('#modal').classList.contains('hidden')) closeModal(); }
  if (e.key === '/' && !/input|textarea|select/i.test(document.activeElement.tagName)) { e.preventDefault(); $('#filter').focus(); }
  if (e.key === 'n' && e.altKey) { e.preventDefault(); openCreate(); }
  if (e.key === 'g' && e.altKey) { e.preventDefault(); openGroups(); }
});
window.addEventListener('beforeunload', (e) => { if (S.todo && S.todo.dirty) { e.preventDefault(); e.returnValue = ''; } });

// live updates
function connectEvents() {
  const es = new EventSource('/api/v1/events');
  es.addEventListener('hello', () => { S.live = true; $('#live').className = 'dot on'; });
  es.addEventListener('changed', () => scheduleRefresh());
  es.addEventListener('synced', () => scheduleRefresh());
  es.addEventListener('tick', () => scheduleRefresh());
  es.onerror = () => { S.live = false; $('#live').className = 'dot err'; };
}

(async function main() {
  await loadConfig();
  await refreshAll();
  connectEvents();
  const hash = decodeURIComponent(location.hash.slice(1));
  if (hash) select(hash);
})();
