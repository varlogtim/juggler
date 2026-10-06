/* juggler web UI. Vanilla JS over the REST API (see README "Web UI and REST
   API"). State lives in S; render() redraws the list, renderDetail() the
   selected workstream. An EventSource on /api/v1/events triggers refreshes. */
'use strict';

const S = {
  status: null, items: [], config: null, repos: [],
  selected: null, detail: null, todo: null, session: null, cands: null, candsAll: false,
  filter: '', onlyLive: false, live: false,
};

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
    const [status, items] = await Promise.all([GET('/api/v1/status'), GET('/api/v1/workstreams')]);
    S.status = status; S.items = items;
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

function render() {
  renderStatus();
  const q = S.filter.trim().toLowerCase();
  const rows = S.items.filter(it => {
    if (S.onlyLive && it.state === 'none') return false;
    if (!q) return true;
    const hay = [it.id, it.desc, it.category, it.name, it.git && it.git.branch, ...(it.refs || []).map(r => r.key + ' ' + r.status + ' ' + r.title)].join(' ').toLowerCase();
    return q.split(/\s+/).every(t => hay.includes(t));
  });
  const order = { displayed: 0, parked: 1, none: 2 };
  rows.sort((a, b) => (order[a.state] - order[b.state]) || (b.shown || '').localeCompare(a.shown || '') || (b.created || '').localeCompare(a.created || ''));
  const tb = $('#list tbody');
  tb.innerHTML = rows.map(it => {
    const g = it.git;
    const branch = g ? (g.branch || ('@' + g.head)) + (g.dirty ? '*' : '') : '';
    const sess = it.opencode_session ? it.opencode_session.slice(-8) : '';
    return h`<tr data-name="${it.name}" class="${it.state} ${S.selected === it.name ? 'selected' : ''}">
      <td class="state" title="${it.state}">${stateGlyph(it.state)}</td>
      <td class="id">${it.id}</td>
      <td><span class="badge cat-${it.category}">${it.category}</span></td>
      <td class="desc">${it.desc}${it.has_code ? '' : raw(' <span class="badge nocode" title="no code dir: terminals start in the workstream dir">no code</span>')}</td>
      <td>${raw((it.refs || []).map(refBadge).join(''))}</td>
      <td class="mono">${it.todos_open || ''}</td>
      <td class="mono" title="${g ? short(it.code_path) : ''}">${branch}${g && g.dirty ? raw(' <span class="badge dirty" title="uncommitted changes">dirty</span>') : ''}</td>
      <td class="mono dim" title="${it.opencode_session || ''}">${sess}</td>
      <td class="actions sm">${it.state === 'displayed' ? raw('<button data-act="focus" title="focus opencode">focus</button>') : raw('<button data-act="show" class="primary" title="show in the slot">show</button>')}</td>
    </tr>`;
  }).join('');
  $('#count').textContent = `${rows.length}/${S.items.length}`;
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
      <span class="badge ${d.state}">${stateGlyph(d.state)} ${d.state}${d.shown ? ' · ' + ago(d.shown) : ''}</span></div>
    <div class="d-sub">${short(d.dir)}</div>
    <div class="d-actions sm">
      ${d.state === 'displayed' ? raw('<button data-act="focus">focus opencode</button><button data-act="park">park</button>') : raw('<button data-act="show" class="primary">show</button>')}
      <button data-act="term" title="terminal in the right stack">term</button>
      <button data-act="notes" title="TODO.md in the editor">notes</button>
      <button data-act="review" title="uncommitted diff in the editor" ${g ? '' : 'disabled'}>review</button>
      ${jira ? raw('<button data-act="open-jira">open jira</button>') : ''}
      ${pr ? raw('<button data-act="open-pr">open pr</button>') : ''}
      <button data-act="dictate" title="toggle dictation notes">dictate</button>
      ${live ? raw('<button data-act="close" title="kill its windows; files are kept">close windows</button>') : ''}
    </div>

    <fieldset><legend>edit</legend>
      <form id="f-edit" class="grid">
        <label>category</label><input name="category" list="cats" value="${d.category}">
        <label>id</label><input name="id" value="${d.id}">
        <label>description</label><input name="desc" value="${d.desc}">
        <label>jira key</label><input name="jira" value="${jira ? jira.key : ''}" placeholder="attach a ticket (sets id + work unless given)">
        <span></span><div class="row"><button class="primary">save</button><span class="muted small">changing these renames the directory; windows are closed and reopened</span></div>
      </form>
    </fieldset>

    <fieldset><legend>code</legend>
      ${g ? raw(h`<dl class="kv">
          <dt>path</dt><dd>${short(d.code_path)}${d.code_inside ? ' (inside)' : ''}</dd>
          <dt>branch</dt><dd>${g.branch || '(detached)'} @ ${g.head}${g.dirty ? raw(' <span class="badge dirty">dirty</span>') : ''}</dd>
          ${g.upstream ? raw(h`<dt>upstream</dt><dd>${g.upstream} ${g.ahead > 0 ? '↑' + g.ahead : ''} ${g.behind > 0 ? '↓' + g.behind : ''}</dd>`) : ''}
          ${g.linked ? raw(h`<dt>worktree of</dt><dd>${short(g.main)}</dd>`) : ''}
        </dl>
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

function worktreeForm() {
  if (!S.repos.length) return '<div class="muted small" style="margin-top:8px">no repos configured ([repos.&lt;name&gt;] in config.toml) — add a worktree from the CLI with a path: <code>jug repo add --repo /path/to/checkout</code></div>';
  return h`<form id="f-wt" class="row sm" style="margin-top:8px">
    <span class="muted">add worktree of</span>
    <select name="repo">${raw(S.repos.map(r => h`<option value="${r.name}" ${r.ok ? '' : 'disabled'}>${r.name}${r.ok ? '' : ' (missing)'}</option>`).join(''))}</select>
    <input name="branch" placeholder="branch (default: id or user/slug)" style="width:220px">
    <input name="base" placeholder="base (default: remote HEAD)" style="width:170px">
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

function openModal(title, bodyHTML) { $('#modal-title').textContent = title; $('#modal-body').innerHTML = bodyHTML; $('#modal').classList.remove('hidden'); }
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
      <label>code</label>
      <div class="radio-group">
        <label><input type="radio" name="code" value="repo" ${repos.length ? 'checked' : 'disabled'}> worktree of
          <select name="repo" ${repos.length ? '' : 'disabled'}>${repos.map(r => `<option value="${esc(r.name)}">${esc(r.name)}</option>`).join('')}</select></label>
        <div class="sub row sm"><input name="branch" placeholder="branch (default: id or user/slug)" style="width:220px"><input name="base" placeholder="base (default: remote HEAD)" style="width:170px"><label class="chk"><input type="checkbox" name="no_fetch"> no fetch</label></div>
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
    const code = fd.get('code');
    if (code === 'repo') { body.repo = fd.get('repo'); body.branch = fd.get('branch'); body.base = fd.get('base'); body.no_fetch = fd.get('no_fetch') === 'on'; }
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
  setTimeout(() => $('#f-new [name=desc]').focus(), 30);
}

// ------------------------------------------------------------------ wiring

document.addEventListener('click', async (e) => {
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
  } else if (fid === 'f-wt') {
    e.preventDefault();
    await busy(f.querySelector('button'), async () => { const r = await POST(p + '/repo', { repo: fd.get('repo'), branch: fd.get('branch'), base: fd.get('base'), no_fetch: fd.get('no_fetch') === 'on' }); toast(r.worktree.what + (r.worktree.seeded && r.worktree.seeded.length ? ' · seeded ' + r.worktree.seeded.join(', ') : ''), 'ok'); if (S.detail.state !== 'none') toast('windows already open still use the old directory: close and show again to start them in the worktree'); await loadDetail(S.detail.name, { keepTodo: true }); scheduleRefresh(50); })();
  }
});
document.addEventListener('click', async (e) => {
  if (e.target.id === 'todo-save' && S.detail) { await busy(e.target, async () => { const r = await PUT(wsPath(S.detail.name) + '/todo', { text: $('#todo').value }); S.todo = { name: S.detail.name, text: $('#todo').value, open: r.open, dirty: false }; $('#todo-open').textContent = `(${r.open} open)`; $('#todo-state').textContent = 'saved'; toast('TODO.md saved', 'ok'); scheduleRefresh(50); })(); }
  if (e.target.id === 'todo-reload' && S.detail) { S.todo = null; await loadDetail(S.detail.name); }
});

$('#btn-new').onclick = openCreate;
$('#empty-new').onclick = (e) => { e.preventDefault(); openCreate(); };
$('#btn-toggle').onclick = busy($('#btn-toggle'), async () => { const r = await POST('/api/v1/slot/toggle'); toast(r.on ? `workspace ${r.slot.num} is now the slot` : 'slot released', 'ok'); scheduleRefresh(50); });
$('#btn-park').onclick = busy($('#btn-park'), async () => { await POST('/api/v1/slot/park'); toast('parked', 'ok'); scheduleRefresh(50); });
$('#btn-lot').onclick = busy($('#btn-lot'), async () => { await POST('/api/v1/lot'); });
$('#modal-close').onclick = closeModal;
$('#modal').addEventListener('click', (e) => { if (e.target.id === 'modal') closeModal(); });
$('#filter').addEventListener('input', (e) => { S.filter = e.target.value; render(); });
$('#only-live').addEventListener('change', (e) => { S.onlyLive = e.target.checked; render(); });
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') { if (!$('#modal').classList.contains('hidden')) closeModal(); }
  if (e.key === '/' && !/input|textarea|select/i.test(document.activeElement.tagName)) { e.preventDefault(); $('#filter').focus(); }
  if (e.key === 'n' && e.altKey) { e.preventDefault(); openCreate(); }
});
window.addEventListener('beforeunload', (e) => { if (S.todo && S.todo.dirty) { e.preventDefault(); e.returnValue = ''; } });

// live updates
function connectEvents() {
  const es = new EventSource('/api/v1/events');
  es.addEventListener('hello', () => { S.live = true; $('#live').className = 'dot on'; });
  es.addEventListener('changed', () => scheduleRefresh());
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
