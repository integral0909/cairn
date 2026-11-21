// The Cairn console: run SQL against the server's JSON API.
const $ = (id) => document.getElementById(id);
const sqlBox = $('sql');
const HISTORY_KEY = 'cairn.history';

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  for (const c of children.flat()) if (c != null) node.append(c instanceof Node ? c : String(c));
  return node;
}

async function run(sql) {
  if (!sql.trim()) return;
  $('run').disabled = $('explain').disabled = true;
  $('status').textContent = 'running…';
  try {
    const res = await fetch('api/query', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sql }),
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || res.statusText);
    render(data.results || []);
    remember(sql);
    loadSchema();
    const total = (data.results || []).reduce((t, r) => t + r.elapsedMs, 0);
    $('status').textContent = `${data.results.length} statement${data.results.length === 1 ? '' : 's'} in ${total.toFixed(2)} ms`;
  } catch (err) {
    $('results').replaceChildren(el('p', { class: 'error' }, err.message));
    $('status').textContent = '';
  } finally {
    $('run').disabled = $('explain').disabled = false;
  }
}

function render(results) {
  const out = results.map((r) => {
    const box = el('div', { class: 'result' }, el('pre', { class: 'sql' }, r.sql));
    if (r.error) {
      box.append(el('p', { class: 'error' }, r.error));
      return box;
    }
    if (r.columns && r.columns.length === 1 && r.columns[0] === 'plan') {
      box.append(el('pre', { class: 'plan' }, (r.rows || []).map((row) => row[0]).join('\n')));
      return box;
    }
    if (r.columns && r.columns.length) {
      const rows = r.rows || [];
      const head = el('tr', {}, r.columns.map((c) => el('th', {}, c)));
      const body = rows.map((row) =>
        el('tr', {}, row.map((v) => {
          if (v === null) return el('td', { class: 'null' }, 'NULL');
          return el('td', { class: typeof v === 'number' ? 'num' : '' }, String(v));
        })),
      );
      box.append(
        el('div', { class: 'meta' }, `${rows.length} row${rows.length === 1 ? '' : 's'}${r.truncated ? ' (truncated)' : ''} · ${r.elapsedMs.toFixed(2)} ms`),
        el('div', { class: 'scroll' }, el('table', {}, el('thead', {}, head), el('tbody', {}, body))),
      );
    } else {
      box.append(el('div', { class: 'meta' }, `${r.rowsAffected} row${r.rowsAffected === 1 ? '' : 's'} changed · ${r.elapsedMs.toFixed(2)} ms`));
    }
    return box;
  });
  $('results').replaceChildren(...out);
}

async function loadSchema() {
  try {
    const res = await fetch('api/schema');
    const data = await res.json();
    $('stats').textContent = `${data.tables.length} tables · ${data.pages} pages · ${data.logFrames} log frames`;
    $('tables').replaceChildren(
      ...data.tables.map((t) =>
        el('li', {},
          el('button', { class: 'table', type: 'button', title: t.sql, onclick: () => insert(`SELECT * FROM ${t.name} LIMIT 100;`) }, t.name),
          el('div', { class: 'cols' }, t.columns.map((c) =>
            el('div', { class: c.primaryKey ? 'pk' : '' }, `${c.name} ${c.type.toLowerCase()}${c.primaryKey ? ' · pk' : ''}`))),
        ),
      ),
    );
    if (!data.tables.length) $('tables').replaceChildren(el('li', { class: 'muted' }, 'No tables yet.'));
  } catch {
    $('stats').textContent = 'server unreachable';
  }
}

function insert(text) {
  sqlBox.value = text;
  sqlBox.focus();
}

function history() {
  try { return JSON.parse(localStorage.getItem(HISTORY_KEY)) || []; } catch { return []; }
}

function remember(sql) {
  if (!$('keep').checked) return;
  const list = [sql, ...history().filter((s) => s !== sql)].slice(0, 50);
  try { localStorage.setItem(HISTORY_KEY, JSON.stringify(list)); } catch { /* storage may be unavailable */ }
  drawHistory();
}

function drawHistory() {
  $('history').replaceChildren(...history().map((s) => el('li', { onclick: () => insert(s) }, s)));
}

$('run').addEventListener('click', () => run(sqlBox.value));
$('explain').addEventListener('click', () => {
  const stmt = sqlBox.value.trim().replace(/;\s*$/, '');
  if (stmt) run(/^explain\b/i.test(stmt) ? stmt : `EXPLAIN ${stmt}`);
});
sqlBox.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
    e.preventDefault();
    run(sqlBox.value);
  }
});
try { $('keep').checked = localStorage.getItem('cairn.keep') !== '0'; } catch { $('keep').checked = false; }
$('keep').addEventListener('change', () => { try { localStorage.setItem('cairn.keep', $('keep').checked ? '1' : '0'); } catch { /* ignore */ } });
drawHistory();
loadSchema();
