'use strict';

const editor = document.getElementById('editor');
const resultsEl = document.getElementById('results');
const treeEl = document.getElementById('schemaTree');
const txBar = document.getElementById('txBar');
const txText = document.getElementById('txText');
const txPill = document.getElementById('txPill');

// ---------- 皮肤 ----------
// 颜色全在 CSS 变量里，这里只负责切 html[data-theme]，并把选择记在本机。
const THEME_KEY = 'godb.theme';
const THEMES = ['dark', 'sky', 'taro', 'mint'];

function applyTheme(name, save) {
  if (!THEMES.includes(name)) name = 'dark';
  // 深色是默认值，挂在 :root 上，所以不加属性即可
  if (name === 'dark') {
    document.documentElement.removeAttribute('data-theme');
  } else {
    document.documentElement.setAttribute('data-theme', name);
  }
  for (const dot of document.querySelectorAll('.theme-dot')) {
    dot.setAttribute('aria-pressed', String(dot.dataset.theme === name));
  }
  // 只有用户点按钮时才记为偏好；链接参数（?theme=）只是当次预览
  if (save) {
    try { localStorage.setItem(THEME_KEY, name); } catch (e) {}
  }
}

for (const dot of document.querySelectorAll('.theme-dot')) {
  dot.addEventListener('click', () => applyTheme(dot.dataset.theme, true));
}
// 进入页面时按上次的选择点亮按钮（属性已经由 head 里的内联脚本设好了）
applyTheme(document.documentElement.getAttribute('data-theme') || 'dark', false);

// 最近一次 /api/schema 的结果，右键菜单和表设计器都从这里取，避免每次再请求。
let schemaCache = [];
// 当前会话是否有未提交的事务（DDL 不能在事务里执行）。
let txActive = false;
// 展开了字段的表名。表树默认只显示到表级（和 Navicat 一样），
// 展开状态自己记着，否则每次刷新/轮询都会把用户展开的字段收回去。
const expandedTables = new Set();

// 最近一次查询的完整响应与结果数组：翻页、改每页行数时重渲染用，
// 不重新查库（结果已经在浏览器里了）。
let lastData = null;
let lastResults = [];
// 每个结果块的翻页状态，key 是它在 results 里的下标。
const pageState = new Map();

const EXAMPLES = {
  select: `SELECT * FROM employees ORDER BY id LIMIT 100;`,
  group: `SELECT d.name        AS 部门,
       count(e.id)   AS 人数,
       sum(e.salary) AS 薪资总额,
       round(avg(e.salary), 2) AS 平均薪资
FROM departments d
LEFT JOIN employees e ON e.dept_id = d.id
GROUP BY d.name
ORDER BY 平均薪资 DESC NULLS LAST;`,
  join: `-- LEFT JOIN：没有部门的员工也会出现（部门列为 NULL）
SELECT e.name  AS 员工,
       e.title AS 职位,
       d.name  AS 部门,
       d.city  AS 城市
FROM employees e
LEFT JOIN departments d ON e.dept_id = d.id
ORDER BY e.id
LIMIT 50;`,
  tx: `-- 事务演示：改完先看效果，再决定提交还是回滚
BEGIN;
UPDATE employees SET salary = salary + 1000 WHERE dept_id = 1;
SELECT name, salary FROM employees WHERE dept_id = 1;
-- 满意就执行 COMMIT; 后悔就执行 ROLLBACK;`,
  ddl: `CREATE TABLE projects (
  id       SERIAL PRIMARY KEY,
  name     TEXT NOT NULL UNIQUE,
  owner_id INTEGER,
  started  TIMESTAMP DEFAULT now()
);
INSERT INTO projects (name, owner_id) VALUES
  ('jsFPGA', 1), ('iceIDE', 2), ('考试系统', 4);
SELECT * FROM projects;`,
  explain: `EXPLAIN
SELECT d.name, count(*) AS 人数
FROM employees e
JOIN departments d ON e.dept_id = d.id
WHERE e.salary > 20000
GROUP BY d.name
HAVING count(*) > 1
ORDER BY 人数 DESC;`,
  alter: `ALTER TABLE employees ADD COLUMN level INTEGER DEFAULT 1;
SELECT id, name, level FROM employees LIMIT 5;
ALTER TABLE employees DROP COLUMN level;`,
};

/* ---------------- 工具 ---------------- */

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

// 本机时钟步进（纳秒），由服务端启动时探测；小于它的耗时只能显示成 "<步进"。
let clockGranNs = 0;

// 把纳秒格式化成好读的时长
function fmtDur(ns) {
  if (ns === undefined || ns === null) return '';
  if (ns < 0) ns = 0;
  if (ns < 1000) return Math.round(ns) + ' ns';
  if (ns < 1000000) {
    const us = ns / 1000;
    return (us < 10 ? us.toFixed(1) : Math.round(us)) + ' µs';
  }
  if (ns < 1000000000) {
    const ms = ns / 1000000;
    return (ms < 10 ? ms.toFixed(2) : ms.toFixed(1)) + ' ms';
  }
  return (ns / 1000000000).toFixed(2) + ' s';
}

// 服务端耗时：测量值小于时钟步进时，说明这台机器的时钟分辨不出，显示上界
function fmtTook(ns) {
  if (ns === undefined || ns === null) return '';
  if (clockGranNs > 0 && ns < clockGranNs) return '<' + fmtDur(clockGranNs);
  return fmtDur(ns);
}

function autoSize() {
  editor.style.height = 'auto';
  editor.style.height = Math.min(editor.scrollHeight + 2, window.innerHeight * 0.38) + 'px';
}

/* ---------------- 请求 ---------------- */

async function api(path, body) {
  const opts = { method: body === undefined ? 'GET' : 'POST', headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  try {
    return JSON.parse(text);
  } catch (e) {
    throw new Error('服务端返回了非 JSON 响应：' + text.slice(0, 200));
  }
}

/* ---------------- 执行 ---------------- */

let busy = false;

async function run(prefix) {
  if (busy) return;
  let sql = editor.value.trim();
  if (!sql) return;
  if (prefix === 'explain') {
    sql = sql.split(';').filter(s => s.trim()).map(s => 'EXPLAIN ' + s.trim() + ';').join('\n');
  }
  busy = true;
  setRunning(true);
  const t0 = performance.now();
  try {
    const data = await api('/api/query', { sql });
    data.roundTripMs = performance.now() - t0;
    renderResults(data);
    updateTx(data.tx);
    pushHistory(sql);
    // DDL 之后刷新表结构
    const ddl = /(create|drop|alter)\s+table/i.test(sql);
    if (ddl || (data.error && /table/i.test(data.error.message))) {
      await loadSchema();
    }
  } catch (e) {
    resultsEl.innerHTML = '';
    resultsEl.appendChild(errorCard({ message: String(e.message || e) }));
  } finally {
    busy = false;
    setRunning(false);
  }
}

function setRunning(on) {
  const btn = document.getElementById('btnRun');
  btn.disabled = on;
  btn.textContent = on ? '执行中…' : '执行';
  if (!on) {
    btn.innerHTML = '执行 <kbd>Ctrl</kbd>+<kbd>Enter</kbd>';
  }
}

/* ---------------- 渲染结果 ---------------- */

function renderResults(data) {
  resultsEl.innerHTML = '';
  lastData = data;
  lastResults = (data.results || []).slice();
  if (data.clockGranNs) clockGranNs = data.clockGranNs;
  const waitNs = data.tx && data.tx.waitNs ? data.tx.waitNs : 0;
  if (waitNs > 0) {
    const note = el('div', 'wait-note', `本次执行等待表锁 ${fmtTook(waitNs)}`);
    note.title = `等待锁 ${waitNs} ns`;
    resultsEl.appendChild(note);
  }
  const list = data.results || [];
  list.forEach((r, i) => resultsEl.appendChild(resultCard(r, i)));
  if (list.length > 1) {
    resultsEl.appendChild(el('div', 'batch-note',
      `共 ${list.length} 条语句 · 服务端合计 ${fmtTook(data.elapsedNs)}`));
  }
  if (data.roundTripMs !== undefined) {
    const rt = el('div', 'batch-note', `浏览器往返 ${data.roundTripMs.toFixed(2)} ms（含网络与 JSON 编码，浏览器时钟精度）`);
    resultsEl.appendChild(rt);
  }
  if (data.error) {
    resultsEl.appendChild(errorCard(data.error));
  }
  if (!list.length && !data.error) {
    resultsEl.appendChild(el('div', 'msg', '没有返回结果'));
  }
}

function resultCard(r, idx) {
  const card = el('div', 'result-card');
  const head = el('div', 'result-head');
  head.appendChild(el('span', 'result-tag', r.tag || r.command || 'OK'));
  const metaBits = [];
  if (typeof r.rowCount === 'number' && r.command === 'SELECT') metaBits.push(r.rowCount + ' 行');
  if (typeof r.affected === 'number' && r.affected > 0 && r.command !== 'SELECT') metaBits.push('影响 ' + r.affected + ' 行');
  if (r.tookNs !== undefined) metaBits.push(fmtTook(r.tookNs));
  const metaEl = el('span', 'result-meta', metaBits.join(' · '));
  if (r.tookNs !== undefined) {
    let tip = `服务端执行 ${r.tookNs} ns（含等待锁，不含 SQL 解析）`;
    if (clockGranNs > 0) tip += `；本机时钟步进 ${fmtDur(clockGranNs)}`;
    metaEl.title = tip;
  }
  head.appendChild(metaEl);
  head.appendChild(el('span', 'spacer'));

  // 导出按钮：只有真的返回了行才有意义
  if (r.rows && r.rows.length) {
    const acts = el('div', 'result-actions');
    acts.appendChild(exportBtn('CSV', () => {
      downloadText('query-' + stamp() + '.csv', '\ufeff' + csvOf(r), 'text/csv');
    }));
    acts.appendChild(exportBtn('JSON', () => {
      downloadText('query-' + stamp() + '.json', jsonOf(r), 'application/json');
    }));
    head.appendChild(acts);
  }

  if (r.plan && r.plan.length && r.command !== 'EXPLAIN') {
    const btn = el('button', 'plan-btn', '执行计划');
    const pre = el('pre', 'plan', r.plan.join('\n'));
    btn.addEventListener('click', () => {
      pre.classList.toggle('show');
      btn.classList.toggle('on');
    });
    head.appendChild(btn);
    card.appendChild(head);
    card.appendChild(pre);
  } else {
    card.appendChild(head);
  }

  if (r.message) {
    card.appendChild(el('div', 'msg', r.message));
  }

  if (r.rows && r.rows.length) {
    card.appendChild(gridBlock(r, idx));
  } else if (r.columns && r.columns.length && !r.message) {
    card.appendChild(el('div', 'msg', '（0 行）'));
  }
  return card;
}

// gridBlock 渲染结果表格 + 分页条。分页状态按「第几个结果」记在 pageState 里，
// 翻页时整体重渲染（结果集已经在内存里，不重新查库）。
function gridBlock(r, idx) {
  const box = document.createDocumentFragment();
  const total = r.rows.length;
  let st = pageState.get(idx);
  if (!st) {
    st = { page: 0, size: 100 };
    pageState.set(idx, st);
  }
  const pages = Math.max(1, Math.ceil(total / st.size));
  if (st.page > pages - 1) st.page = pages - 1;
  const from = st.page * st.size;
  const to = Math.min(from + st.size, total);

  const wrap = el('div', 'grid-wrap');
  const table = el('table', 'grid');
  const thead = el('thead');
  const tr = el('tr');
  for (const c of r.columns || []) {
    const th = el('th');
    th.appendChild(el('span', 'cname', c.name));
    if (c.type) th.appendChild(el('span', 'ctype', c.type));
    tr.appendChild(th);
  }
  thead.appendChild(tr);
  table.appendChild(thead);

  const tbody = el('tbody');
  for (let i = from; i < to; i++) {
    const row = r.rows[i];
    const tr2 = el('tr');
    for (const v of row) {
      let td;
      if (v === null || v === undefined) {
        td = el('td', 'null', 'NULL');
      } else if (typeof v === 'number') {
        td = el('td', 'num', String(v));
      } else if (typeof v === 'boolean') {
        td = el('td', 'bool', v ? 'true' : 'false');
      } else {
        td = el('td', '', String(v));
      }
      tr2.appendChild(td);
    }
    tbody.appendChild(tr2);
  }
  table.appendChild(tbody);
  wrap.appendChild(table);
  box.appendChild(wrap);

  const foot = el('div', 'grid-foot');
  foot.appendChild(el('span', 'grid-range', `第 ${from + 1}–${to} 行 / 共 ${total} 行`));
  foot.appendChild(el('span', 'spacer'));

  if (total > 50) {
    const sizeSel = document.createElement('select');
    sizeSel.className = 'pg-size';
    for (const n of [50, 100, 200, 500]) {
      const o = document.createElement('option');
      o.value = String(n);
      o.textContent = n + ' 行/页';
      sizeSel.appendChild(o);
    }
    sizeSel.value = String(st.size);
    sizeSel.addEventListener('change', () => {
      st.size = parseInt(sizeSel.value, 10) || 100;
      st.page = 0;
      renderResults(lastData);
    });
    foot.appendChild(sizeSel);
    foot.appendChild(el('span', 'grid-range', `${st.page + 1}/${pages} 页`));

    const prev = el('button', 'pg-btn', '‹ 上一页');
    prev.disabled = st.page <= 0;
    prev.addEventListener('click', () => {
      if (st.page > 0) { st.page--; renderResults(lastData); }
    });
    foot.appendChild(prev);

    const next = el('button', 'pg-btn', '下一页 ›');
    next.disabled = st.page >= pages - 1;
    next.addEventListener('click', () => {
      if (st.page < pages - 1) { st.page++; renderResults(lastData); }
    });
    foot.appendChild(next);
  }

  box.appendChild(foot);
  return box;
}

function exportBtn(label, run) {
  const b = el('button', 'exp-btn', label);
  b.title = '导出为 ' + label;
  b.addEventListener('click', run);
  return b;
}

function stamp() {
  const d = new Date();
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
}

function downloadText(name, text, mime) {
  const blob = new Blob([text], { type: mime + ';charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  setTimeout(() => URL.revokeObjectURL(url), 2000);
}

function csvCell(v) {
  if (v === null || v === undefined) return '';
  const s = String(v);
  return /[",\r\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
}

function csvOf(r) {
  const cols = (r.columns || []).map(c => c.name);
  const lines = [cols.map(csvCell).join(',')];
  for (const row of r.rows) lines.push(row.map(csvCell).join(','));
  return lines.join('\r\n');
}

function jsonOf(r) {
  const cols = (r.columns || []).map(c => c.name);
  const arr = r.rows.map(row => {
    const o = {};
    cols.forEach((c, i) => { o[c] = row[i] === undefined ? null : row[i]; });
    return o;
  });
  return JSON.stringify(arr, null, 2);
}

function errorCard(err) {
  const card = el('div', 'error-card');
  let title = '执行失败';
  if (err.kind === 'lock_timeout') title = '锁冲突 · 等待超时';
  if (err.kind === 'deadlock') title = '检测到死锁 · 已回滚';
  const head = el('div', 'error-head', title);
  const body = el('div', 'error-body', err.message || '未知错误');
  card.appendChild(head);
  card.appendChild(body);
  if (err.rolledBack) {
    card.appendChild(el('div', 'error-body error-pos', '整个事务已自动回滚并释放锁，修正后可直接重试。'));
  }
  if (err.line) {
    card.appendChild(el('div', 'error-body error-pos', `位置：第 ${err.line} 行，第 ${err.col} 列`));
  }
  if (typeof err.stmt === 'number' && err.stmt > 0) {
    card.appendChild(el('div', 'error-body error-pos', `第 ${err.stmt + 1} 条语句失败，之前的语句已生效`));
  }
  return card;
}

/* ---------------- 表结构 ---------------- */

async function loadSchema() {
  try {
    const data = await api('/api/schema');
    schemaCache = data.tables || [];
    renderSchema(schemaCache);
    updateTx(data.tx);
  } catch (e) {
    treeEl.innerHTML = '';
    treeEl.appendChild(el('div', 'empty-hint', '读取表结构失败：' + e.message));
  }
}

function renderSchema(tables) {
  treeEl.innerHTML = '';
  if (!tables.length) {
    treeEl.appendChild(el('div', 'empty-hint', '暂无表，试试「载入示例数据」'));
    return;
  }
  for (const t of tables) {
    const node = el('div', 'table-node' + (expandedTables.has(t.name) ? ' open' : ''));
    const row = el('div', 'table-row');
    row.title = `${t.name} · ${t.rows} 行 · ${t.columns.length} 个字段（点 ▶ 展开字段，右键有表操作）`;
    const caret = el('span', 'caret', '▶');
    caret.addEventListener('click', ev => {
      ev.stopPropagation();
      const open = node.classList.toggle('open');
      if (open) expandedTables.add(t.name);
      else expandedTables.delete(t.name);
    });
    row.appendChild(caret);
    row.appendChild(el('span', 'tname', t.name));
    row.appendChild(el('span', 'tcount', String(t.rows)));
    row.addEventListener('click', () => openTable(t.name));
    row.addEventListener('contextmenu', ev => {
      ev.preventDefault();
      ev.stopPropagation();
      showCtxMenu(ev.clientX, ev.clientY, tableMenuItems(t));
    });
    node.appendChild(row);

    const cols = el('div', 'cols');
    for (const c of t.columns) {
      const cr = el('div', 'col-row');
      cr.appendChild(el('span', '', c.name));
      if (c.primaryKey) cr.appendChild(el('span', 'pk', 'PK'));
      cr.appendChild(el('span', 'ctype', c.type));
      cr.title = `${c.name} ${c.type}${c.notNull ? ' NOT NULL' : ''}${c.default ? ' DEFAULT ' + c.default : ''}`;
      cr.addEventListener('click', ev => {
        ev.stopPropagation();
        editor.value = `SELECT ${qi(c.name)} FROM ${qi(t.name)} LIMIT 100;`;
        autoSize();
        run();
      });
      cols.appendChild(cr);
    }
    node.appendChild(cols);
    treeEl.appendChild(node);
  }
}

/* ---------------- 锁状态面板 ---------------- */

const lockPanel = document.getElementById('lockPanel');
const lockBody = document.getElementById('lockBody');
const lockMeta = document.getElementById('lockMeta');
const lockBadge = document.getElementById('lockBadge');
let lockOpen = false;

function toggleLocks(force) {
  lockOpen = force === undefined ? !lockOpen : force;
  lockPanel.classList.toggle('hidden', !lockOpen);
  document.body.classList.toggle('locks-open', lockOpen);
  if (lockOpen) loadLocks();
}

async function loadLocks() {
  try {
    const snap = await api('/api/locks');
    renderLockBadge(snap);
    if (lockOpen) renderLocks(snap);
  } catch (e) {
    /* 忽略轮询错误 */
  }
}

function renderLockBadge(snap) {
  const n = (snap.waiters || []).length;
  if (n > 0) {
    lockBadge.textContent = String(n);
    lockBadge.classList.remove('hidden');
  } else {
    lockBadge.classList.add('hidden');
  }
}

function renderLocks(snap) {
  lockBody.innerHTML = '';
  const tables = snap.tables || [];
  if (!tables.length) {
    lockBody.appendChild(el('div', 'lock-empty', '当前没有任何表锁（空闲中）。'));
  } else {
    lockBody.appendChild(el('div', 'lock-sec', '表锁'));
    for (const t of tables) {
      const row = el('div', 'lock-row');
      row.appendChild(el('div', 'lock-name', t.name));
      const holds = el('div', 'lock-holds');
      for (const h of t.holders || []) {
        const chip = el('span', 'lk ' + (h.mode === 'S' ? 'lk-s' : 'lk-x'), h.mode);
        const who = el('span', 'lock-who', sessionShort(h.session));
        const wrap = el('span', 'lock-holds');
        wrap.appendChild(chip);
        wrap.appendChild(who);
        holds.appendChild(wrap);
      }
      row.appendChild(holds);
      if ((t.waiters || []).length) {
        const names = t.waiters.map(w => sessionShort(w.session)).join('、');
        row.appendChild(el('div', 'lock-wait', '⏳ 等待中：' + names));
      }
      lockBody.appendChild(row);
    }
  }

  const waiters = snap.waiters || [];
  if (waiters.length) {
    lockBody.appendChild(el('div', 'lock-sec', '等待队列'));
    for (const w of waiters) {
      const row = el('div', 'lock-row');
      row.appendChild(el('div', 'lock-name', sessionShort(w.session)));
      row.appendChild(el('div', 'lock-wait', '要锁：' + (w.tables || []).join('、')));
      lockBody.appendChild(row);
    }
  }

  lockMeta.textContent = `冲突次数 ${snap.conflicts || 0}` + (snap.lastBlock ? ` · 最近 ${snap.lastBlock}` : '');
}

function sessionShort(id) {
  if (!id) return '?';
  return id.length > 6 ? id.slice(0, 6) : id;
}

/* ---------------- 事务状态 ---------------- */

let heartbeat = null;

function updateTx(tx) {
  if (!tx) return;
  txActive = !!tx.active;
  if (tx.active) {
    // 事务打开期间定时心跳，避免被服务端当作“遗弃事务”回收
    if (!heartbeat) {
      heartbeat = setInterval(() => { api('/api/schema').catch(() => {}); }, 25000);
    }
  } else if (heartbeat) {
    clearInterval(heartbeat);
    heartbeat = null;
  }
  if (tx.active) {
    txBar.classList.remove('hidden');
    if (tx.aborted) {
      txBar.classList.add('aborted');
      txText.textContent = '事务已中止（语句出错），只能回滚';
      txPill.textContent = '事务已中止';
      txPill.className = 'pill pill-aborted';
      document.getElementById('btnCommit').classList.add('hidden');
    } else {
      txBar.classList.remove('aborted');
      const held = (tx.held || []).join('、');
      txText.textContent = `事务进行中 · 已修改 ${tx.changes} 行（未提交）` + (held ? ` · 持锁 ${held}` : '');
      txPill.textContent = '事务中 · ' + tx.changes + ' 行';
      txPill.className = 'pill pill-active';
      document.getElementById('btnCommit').classList.remove('hidden');
    }
  } else {
    txBar.classList.add('hidden');
    txPill.textContent = '空闲';
    txPill.className = 'pill pill-idle';
  }
}

async function txAction(action) {
  const data = await api('/api/tx', { action });
  renderResults(data);
  updateTx(data.tx);
  await loadSchema();
}

/* ---------------- 事件绑定 ---------------- */

document.getElementById('btnRun').addEventListener('click', () => run());
document.getElementById('btnClear').addEventListener('click', () => {
  editor.value = '';
  localStorage.removeItem('godb.sql');
  autoSize();
  editor.focus();
});
document.getElementById('btnExplain').addEventListener('click', () => run('explain'));
document.getElementById('btnRefresh').addEventListener('click', loadSchema);
document.getElementById('btnLocks').addEventListener('click', () => toggleLocks());
document.getElementById('btnLockClose').addEventListener('click', () => toggleLocks(false));
document.getElementById('btnCommit').addEventListener('click', () => txAction('commit'));
document.getElementById('btnRollback').addEventListener('click', () => txAction('rollback'));

document.getElementById('btnSample').addEventListener('click', async () => {
  if (!confirm('将重建示例数据（departments / employees），当前所有表会被清空。继续？')) return;
  try {
    await api('/api/sample', {});
    await loadSchema();
    editor.value = EXAMPLES.select;
    autoSize();
    run();
  } catch (e) {
    alert('载入失败：' + e.message);
  }
});

document.getElementById('btnReset').addEventListener('click', async () => {
  if (!confirm('确定要删除所有表吗？此操作不可撤销。')) return;
  try {
    await api('/api/reset', {});
    await loadSchema();
    resultsEl.innerHTML = '';
    resultsEl.appendChild(el('div', 'msg', '数据库已清空'));
    updateTx({ active: false });
  } catch (e) {
    alert('清空失败：' + e.message);
  }
});

for (const chip of document.querySelectorAll('.chip')) {
  chip.addEventListener('click', () => {
    editor.value = EXAMPLES[chip.dataset.ex] || '';
    autoSize();
    editor.focus();
  });
}

editor.addEventListener('keydown', ev => {
  if ((ev.ctrlKey || ev.metaKey) && ev.key === 'Enter') {
    ev.preventDefault();
    run();
    return;
  }
  if (ev.key === 'Tab') {
    ev.preventDefault();
    const start = editor.selectionStart;
    const end = editor.selectionEnd;
    editor.value = editor.value.slice(0, start) + '  ' + editor.value.slice(end);
    editor.selectionStart = editor.selectionEnd = start + 2;
  }
});

editor.addEventListener('input', () => {
  autoSize();
  localStorage.setItem('godb.sql', editor.value);
});


/* ---------------- 设置（落盘节奏 / 端口 / 密码） ---------------- */

const settingsModal = document.getElementById('settingsModal');
const settingsBody = document.getElementById('settingsBody');
const settingsHint = document.getElementById('settingsHint');
let settingsData = null;

function setHint(msg, kind) {
  settingsHint.textContent = msg || '';
  settingsHint.className = 'modal-hint' + (kind ? ' ' + kind : '');
}

function fmtBytes(n) {
  if (!n) return '0 B';
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
  return (n / 1073741824).toFixed(2) + ' GB';
}

async function openSettings() {
  settingsModal.classList.remove('hidden');
  settingsBody.innerHTML = '';
  setHint('加载中…', '');
  try {
    const d = await api('/api/config');
    if (d.error) {
      settingsBody.appendChild(el('div', 'set-warn', d.error));
      setHint('');
      return;
    }
    settingsData = d;
    renderSettings();
    setHint('');
  } catch (e) {
    settingsBody.appendChild(el('div', 'set-warn', '读取配置失败：' + e.message));
  }
}

function closeSettings() {
  settingsModal.classList.add('hidden');
}

function setRow(labelText, input, unit) {
  const row = el('div', 'set-row');
  const lab = el('label', '', labelText);
  if (input.id) lab.htmlFor = input.id;
  row.appendChild(lab);
  row.appendChild(input);
  if (unit) row.appendChild(el('span', 'unit', unit));
  return row;
}

function numInput(id, val, min, max) {
  const i = document.createElement('input');
  i.type = 'number';
  i.id = id;
  i.value = String(val);
  if (min !== undefined) i.min = String(min);
  if (max !== undefined) i.max = String(max);
  return i;
}

function textInput(id, val, placeholder, type) {
  const i = document.createElement('input');
  i.type = type || 'text';
  i.id = id;
  i.value = val || '';
  if (placeholder) i.placeholder = placeholder;
  return i;
}

function checkInput(id, checked) {
  const i = document.createElement('input');
  i.type = 'checkbox';
  i.id = id;
  i.checked = !!checked;
  return i;
}

function renderSettings() {
  const d = settingsData;
  settingsBody.innerHTML = '';
  const rt = d.runtime || {};

  if (!rt.canEdit) {
    settingsBody.appendChild(el('div', 'set-readonly',
      '当前连接来自 ' + (rt.clientIp || '远程序') + '，只能查看。配置的写操作只接受本机（127.0.0.1）请求。'));
  }

  /* ---- 落盘 ---- */
  settingsBody.appendChild(el('div', 'set-sec', '落盘节奏（checkpoint）'));
  settingsBody.appendChild(setRow('每多少次提交', numInput('setCpCommits', d.storage.checkpointCommits, 0, 1000000), '次'));
  settingsBody.appendChild(setRow('每隔多少秒', numInput('setCpSeconds', d.storage.checkpointSeconds, 0, 86400), '秒'));
  settingsBody.appendChild(setRow('WAL 超过', numInput('setCpWALMB', d.storage.checkpointWalMB, 0, 65536), 'MB'));
  settingsBody.appendChild(el('div', 'set-hint',
    '三条任一满足就落盘（写快照 + 轮转 WAL），填 0 表示关闭该条。提交时的 fsync 始终同步执行，不受这里影响。'));

  const cp = rt.checkpoint || {};
  const stats = el('div', 'set-stats');
  const stat = (k, v) => {
    const s = el('span', '', k + ' ');
    s.appendChild(el('b', '', v));
    stats.appendChild(s);
  };
  stat('累计落盘', (cp.runs || 0) + ' 次');
  stat('上次落盘', cp.lastAt ? (cp.secondsAgo || 0) + ' 秒前' : '尚未');
  stat('之后已提交', (cp.commitsSince || 0) + ' 次');
  stat('当前 WAL', fmtBytes(cp.walBytes || 0));
  stat('数据文件', fmtBytes(rt.dataBytes || 0));
  stat('表 / 行数', (rt.tables || 0) + ' / ' + (rt.rows || 0));
  settingsBody.appendChild(stats);

  if ((d.warnings || []).length) {
    d.warnings.forEach(w => settingsBody.appendChild(el('div', 'set-warn', w)));
  }

  /* ---- 网络 ---- */
  settingsBody.appendChild(el('div', 'set-sec', '网络（改完立即生效）'));
  settingsBody.appendChild(setRow('控制台端口', numInput('setHTTPPort', d.network.httpPort, 1, 65535)));
  const httpRemote = checkInput('setHTTPRemote', d.network.httpRemote);
  const httpRemoteRow = el('div', 'set-row');
  httpRemoteRow.appendChild(el('label', '', ''));
  httpRemoteRow.appendChild(httpRemote);
  httpRemoteRow.appendChild(el('span', 'unit', '允许远程访问控制台'));
  settingsBody.appendChild(httpRemoteRow);
  settingsBody.appendChild(el('div', 'set-hint',
    '当前监听：' + (rt.httpAddr || '—') + '（' + (d.network.httpRemote ? '同网段可访问' : '仅本机') + '）' +
    '　关闭远程时只绑定 127.0.0.1。'));

  const pgEnabled = checkInput('setPGEnabled', d.network.pgEnabled);
  const pgRow = el('div', 'set-row');
  pgRow.appendChild(el('label', '', ''));
  pgRow.appendChild(pgEnabled);
  pgRow.appendChild(el('span', 'unit', '启用 PostgreSQL 协议端口'));
  settingsBody.appendChild(pgRow);

  const pgPort = numInput('setPGPort', d.network.pgPort, 1, 65535);
  pgPort.disabled = !d.network.pgEnabled;
  settingsBody.appendChild(setRow('PG 端口', pgPort));

  const pgRemote = checkInput('setPGRemote', d.network.pgRemote);
  const pgRemoteRow = el('div', 'set-row');
  pgRemoteRow.appendChild(el('label', '', ''));
  pgRemoteRow.appendChild(pgRemote);
  pgRemoteRow.appendChild(el('span', 'unit', '允许远程连接 PG'));
  settingsBody.appendChild(pgRemoteRow);

  settingsBody.appendChild(el('div', 'set-hint',
    '当前监听：' + (rt.pgAddr || '未启用') +
    '　改端口会断开现有客户端，重连即可。改控制台端口后需要用新端口重新打开页面。'));

  pgEnabled.addEventListener('change', () => {
    pgPort.disabled = !pgEnabled.checked;
  });

  /* ---- 安全 ---- */
  settingsBody.appendChild(el('div', 'set-sec', '安全（PostgreSQL 协议密码）'));
  const requirePw = checkInput('setRequirePw', d.auth.requirePassword);
  const pwRow = el('div', 'set-row');
  pwRow.appendChild(el('label', '', ''));
  pwRow.appendChild(requirePw);
  pwRow.appendChild(el('span', 'unit', '连接 PG 时必须输入密码'));
  settingsBody.appendChild(pwRow);

  const userInput = textInput('setAuthUser', d.auth.user, 'godb');
  const pwInput = textInput('setAuthPassword', '', d.auth.hasPassword ? '已设置，留空则不修改' : '设置密码', 'password');
  settingsBody.appendChild(setRow('用户名', userInput));
  settingsBody.appendChild(setRow('密码', pwInput));
  settingsBody.appendChild(el('div', 'set-hint',
    '密码以 PBKDF2-SHA256 哈希保存在配置文件里，接口不会回显。' +
    (d.auth.hasPassword ? ' 已有密码；留空表示保持不变。' : '')));

  settingsBody.appendChild(el('div', 'set-warn',
    '网页控制台本身不登录：能访问控制台的人可以执行任意 SQL。配置的修改只接受本机请求，' +
    '所以远程只能查看。PG 协议是明文传输（没有 TLS），跨公网请自行套隧道。'));

  const pathEl = el('div', 'set-path', '配置文件：' + (d.path || '—') + '　客户端：' + (rt.clientIp || '—'));
  settingsBody.appendChild(pathEl);

  const saveBtn = document.getElementById('btnSettingsSave');
  saveBtn.disabled = !rt.canEdit;
}

async function saveSettings() {
  if (!settingsData || !settingsData.runtime.canEdit) return;
  const val = id => {
    const n = document.getElementById(id);
    const v = parseInt(n.value, 10);
    return isNaN(v) ? 0 : v;
  };
  const beforeAddr = settingsData.runtime.httpAddr;
  const body = {
    storage: {
      checkpointCommits: val('setCpCommits'),
      checkpointSeconds: val('setCpSeconds'),
      checkpointWalMB: val('setCpWALMB'),
    },
    network: {
      httpPort: val('setHTTPPort'),
      httpRemote: document.getElementById('setHTTPRemote').checked,
      pgEnabled: document.getElementById('setPGEnabled').checked,
      pgPort: val('setPGPort'),
      pgRemote: document.getElementById('setPGRemote').checked,
    },
    auth: {
      requirePassword: document.getElementById('setRequirePw').checked,
      user: document.getElementById('setAuthUser').value.trim(),
      password: document.getElementById('setAuthPassword').value,
      clearPassword: false,
    },
  };
  setHint('保存中…', '');
  try {
    const res = await api('/api/config', body);
    if (res.error) {
      setHint(res.error, 'err');
      return;
    }
    settingsData = res;
    renderSettings();
    const afterAddr = res.runtime.httpAddr;
    if (beforeAddr && afterAddr && beforeAddr !== afterAddr) {
      setHint('已保存。控制台换到了 ' + afterAddr + '，请用新地址打开页面。', 'ok');
    } else {
      setHint('已保存并生效。', 'ok');
    }
    loadSchema();
  } catch (e) {
    setHint('保存失败：' + e.message, 'err');
  }
}

document.getElementById('btnSettings').addEventListener('click', openSettings);
document.getElementById('btnSettingsClose').addEventListener('click', closeSettings);
document.getElementById('btnSettingsSave').addEventListener('click', saveSettings);
settingsModal.addEventListener('click', e => {
  if (e.target === settingsModal) closeSettings();
});
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !settingsModal.classList.contains('hidden')) closeSettings();
});

/* ---------------- 启动 ---------------- */

(function init() {
  const saved = localStorage.getItem('godb.sql');
  if (saved) {
    editor.value = saved;
  } else {
    editor.value = EXAMPLES.select;
  }
  autoSize();
  loadSchema();
  loadLocks();
  setInterval(loadLocks, 3000);
  // 支持 #locks / #settings 直接打开对应面板
  if (location.hash === '#locks') toggleLocks(true);
  if (location.hash === '#settings') openSettings();
})();

/* ---------------- 表管理：标识符 / 类型 / 提示 ---------------- */

// PG 保留字里会被当成别名的那些，生成 SQL 时给它们加引号。
const RESERVED_IDENT = new Set(`select from where group by having order limit offset as distinct all
join inner outer left right full cross on using natural and or not null is in between like ilike
case when then else end insert into values update set delete create table drop alter add column
rename to truncate begin commit rollback primary key unique default check references foreign constraint
user current_user order`.split(/\s+/).filter(Boolean));

// 设计器下拉里的类型，按 PG 的写法。
const TYPE_LIST = ['int2', 'int4', 'int8', 'serial', 'bigserial', 'numeric', 'float4', 'float8',
  'varchar', 'char', 'text', 'boolean', 'date', 'time', 'timestamp', 'timestamptz',
  'interval', 'json', 'jsonb', 'uuid', 'bytea'];

// 用户建表时可能写全名/别名，统一收敛到 PG 内部名，方便回填下拉。
const TYPE_ALIAS = {
  smallint: 'int2', integer: 'int4', int: 'int4', int4: 'int4', serial4: 'serial',
  bigint: 'int8', int8: 'int8', bigserial: 'bigserial', serial8: 'bigserial',
  real: 'float4', 'double precision': 'float8', double: 'float8', float8: 'float8',
  decimal: 'numeric', numeric: 'numeric', bool: 'boolean', boolean: 'boolean',
  'character varying': 'varchar', varchar: 'varchar', character: 'char', char: 'char', bpchar: 'char',
  text: 'text', date: 'date', time: 'time', 'time without time zone': 'time',
  timetz: 'time', 'time with time zone': 'time',
  timestamp: 'timestamp', 'timestamp without time zone': 'timestamp', datetime: 'timestamp',
  timestamptz: 'timestamptz', 'timestamp with time zone': 'timestamptz',
  interval: 'interval', json: 'json', jsonb: 'jsonb', uuid: 'uuid', bytea: 'bytea',
};

function qi(name) {
  const s = String(name || '').trim();
  if (/^[A-Za-z_][A-Za-z0-9_$]*$/.test(s) && !RESERVED_IDENT.has(s.toLowerCase())) return s;
  return '"' + s.replace(/"/g, '""') + '"';
}

// 把 "numeric(12,2)" / "varchar(255)" / "integer" 拆成基名 + 长度 + 小数位。
function parseType(text) {
  let raw = String(text || '').trim().toLowerCase();
  let args = [];
  const m = raw.match(/\(([^)]*)\)/);
  if (m) {
    args = m[1].split(',').map(x => parseInt(x.trim(), 10)).filter(n => !isNaN(n));
    raw = raw.slice(0, m.index).trim();
  }
  const base = TYPE_ALIAS[raw] || (TYPE_LIST.includes(raw) ? raw : 'text');
  let len = args[0] || 0;
  let scale = base === 'numeric' ? (args[1] || 0) : 0;
  return { base, len, scale };
}

// 反向：把设计器里的一行拼成 SQL 类型写法。
function typeSql(r) {
  if (r.type === 'varchar' || r.type === 'char') return r.len > 0 ? `${r.type}(${r.len})` : r.type;
  if (r.type === 'numeric') return r.len > 0 ? `numeric(${r.len},${r.scale || 0})` : 'numeric';
  return r.type;
}

let toastTimer = null;
function toast(msg, isErr) {
  const t = document.getElementById('toast');
  if (!t) return;
  t.textContent = msg;
  t.className = 'toast' + (isErr ? ' err' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = 'toast hidden'; }, isErr ? 7000 : 2600);
}

// DDL 不允许在显式事务里执行（引擎的硬性限制），先拦一下给个明确提示。
function ensureNoTx() {
  if (!txActive) return true;
  toast('当前事务还没结束，DDL 不能在事务里执行，请先提交或回滚', true);
  return false;
}

/* ---------------- 右键菜单 ---------------- */

function ctxMenuEl() { return document.getElementById('ctxMenu'); }

function hideCtxMenu() {
  const m = ctxMenuEl();
  if (!m) return;
  m.classList.add('hidden');
  m.innerHTML = '';
}

function showCtxMenu(x, y, items) {
  const m = ctxMenuEl();
  if (!m) return;
  m.innerHTML = '';
  for (const it of items) {
    if (!it) continue;
    if (it.sep) { m.appendChild(el('div', 'ctx-sep')); continue; }
    const b = el('button', 'ctx-item' + (it.danger ? ' danger' : ''), it.label);
    if (it.hint) b.appendChild(el('span', 'ctx-hint', it.hint));
    if (it.disabled) {
      b.disabled = true;
    } else {
      b.addEventListener('click', () => { hideCtxMenu(); it.run(); });
    }
    m.appendChild(b);
  }
  m.classList.remove('hidden');
  m.style.left = x + 'px';
  m.style.top = y + 'px';
  // 贴边时往回收，别跑出视口
  const r = m.getBoundingClientRect();
  if (r.right > window.innerWidth - 6) m.style.left = Math.max(6, window.innerWidth - r.width - 6) + 'px';
  if (r.bottom > window.innerHeight - 6) m.style.top = Math.max(6, window.innerHeight - r.height - 6) + 'px';
}

function tableMenuItems(t) {
  return [
    { label: '打开表', hint: 'SELECT *', run: () => openTable(t.name) },
    { label: '设计表', hint: 'ALTER', run: () => openDesigner(t.name) },
    { sep: true },
    { label: '新建表…', run: () => openDesigner(null) },
    { label: '重命名…', run: () => renameTable(t.name) },
    { label: '复制表结构…', run: () => copyTableStructure(t) },
    { sep: true },
    { label: '清空数据', hint: 'TRUNCATE', danger: true, run: () => truncateTable(t) },
    { label: '删除表', hint: 'DROP', danger: true, run: () => dropTable(t) },
    { sep: true },
    { label: '刷新列表', run: loadSchema },
  ];
}

function blankMenuItems() {
  return [
    { label: '新建表…', run: () => openDesigner(null) },
    { label: '刷新列表', run: loadSchema },
  ];
}

/* ---------------- 表级动作 ---------------- */

function openTable(name) {
  editor.value = `SELECT * FROM ${qi(name)} LIMIT 100;`;
  autoSize();
  run();
}

async function runDdl(sql, okMsg) {
  if (!ensureNoTx()) return false;
  try {
    const data = await api('/api/query', { sql });
    updateTx(data.tx);
    renderResults(data);
    await loadSchema();
    if (data.error) {
      toast(data.error.message || '执行失败', true);
      return false;
    }
    if (okMsg) toast(okMsg);
    return true;
  } catch (e) {
    toast('执行失败：' + e.message, true);
    return false;
  }
}

// 通用输入弹窗：返回 Promise<string|null>
function askText({ title, label, value, hint, okText }) {
  return new Promise(resolve => {
    const modal = document.getElementById('promptModal');
    const input = document.getElementById('promptInput');
    const errEl = document.getElementById('promptHint');
    document.getElementById('promptTitle').textContent = title || '输入';
    document.getElementById('promptLabel').textContent = label || '名称';
    document.getElementById('promptHintText').textContent = hint || '';
    document.getElementById('btnPromptOk').textContent = okText || '确定';
    input.value = value || '';
    errEl.textContent = '';
    errEl.className = 'modal-hint';
    modal.classList.remove('hidden');
    setTimeout(() => { input.focus(); input.select(); }, 30);

    const done = v => {
      modal.classList.add('hidden');
      document.getElementById('btnPromptOk').onclick = null;
      document.getElementById('btnPromptCancel').onclick = null;
      document.getElementById('btnPromptClose').onclick = null;
      input.onkeydown = null;
      resolve(v);
    };
    const submit = () => {
      const v = input.value.trim();
      if (!v) { errEl.textContent = '不能为空'; errEl.className = 'modal-hint err'; return; }
      done(v);
    };
    document.getElementById('btnPromptOk').onclick = submit;
    document.getElementById('btnPromptCancel').onclick = () => done(null);
    document.getElementById('btnPromptClose').onclick = () => done(null);
    input.onkeydown = ev => {
      if (ev.key === 'Enter') { ev.preventDefault(); submit(); }
      if (ev.key === 'Escape') { ev.preventDefault(); done(null); }
    };
  });
}

async function renameTable(oldName) {
  const name = await askText({
    title: '重命名表', label: '新表名', value: oldName,
    hint: '会执行 ALTER TABLE ... RENAME TO ...', okText: '重命名',
  });
  if (!name || name === oldName) return;
  await runDdl(`ALTER TABLE ${qi(oldName)} RENAME TO ${qi(name)};`, `表已重命名为 ${name}`);
}

async function copyTableStructure(t) {
  const name = await askText({
    title: '复制表结构', label: '新表名', value: t.name + '_copy',
    hint: '只复制结构（字段/类型/主键），不复制数据 —— 引擎不支持 INSERT ... SELECT',
    okText: '创建',
  });
  if (!name) return;
  const cols = t.columns.map(c => {
    const p = parseType(c.type);
    let s = '  ' + qi(c.name) + ' ' + typeSql({ type: p.base, len: p.len, scale: p.scale });
    if (c.primaryKey) s += ' PRIMARY KEY';
    else {
      if (c.notNull) s += ' NOT NULL';
      if (c.unique) s += ' UNIQUE';
      if (c.default) s += ' DEFAULT ' + c.default;
    }
    return s;
  });
  const sql = `CREATE TABLE ${qi(name)} (\n${cols.join(',\n')}\n);`;
  await runDdl(sql, `已复制表结构到 ${name}`);
}

async function truncateTable(t) {
  if (!confirm(`确定清空表 ${t.name} 的所有数据吗？\n\n表结构会保留，操作不可撤销（${t.rows} 行）。`)) return;
  await runDdl(`TRUNCATE TABLE ${qi(t.name)};`, `已清空 ${t.name}`);
}

async function dropTable(t) {
  if (!confirm(`确定删除表 ${t.name} 吗？\n\n表结构和数据都会被删除，不可撤销。`)) return;
  await runDdl(`DROP TABLE ${qi(t.name)};`, `已删除 ${t.name}`);
}

/* ---------------- 表设计器 ---------------- */

let designer = null;

function designerModalEl() { return document.getElementById('designerModal'); }

function newDesignerRow() {
  return { orig: null, name: '', type: 'int4', len: 0, scale: 0, notNull: false, pk: false, uniq: false, def: '' };
}

function openDesigner(name) {
  const t = name ? schemaCache.find(x => x.name === name) : null;
  if (name && !t) { toast('找不到表 ' + name, true); return; }
  designer = { mode: t ? 'edit' : 'new', origName: t ? t.name : '', origCols: [], rows: [], sel: -1 };

  if (t) {
    designer.origCols = t.columns.map(c => ({
      name: c.name, type: c.type, notNull: !!c.notNull, def: c.default || '',
      pk: !!c.primaryKey, uniq: !!c.unique,
    }));
    designer.rows = t.columns.map(c => {
      const p = parseType(c.type);
      // serial 列在 catalog 里存的是 integer + Serial 标记，这里还原成 serial，
      // 否则设计器会显示成 int4，改动别处保存时看不出它是自增列。
      const base = c.serial ? 'serial' : p.base;
      return {
        orig: c.name, name: c.name, type: base, len: p.len, scale: p.scale,
        notNull: !!c.notNull, pk: !!c.primaryKey, uniq: !!c.unique, def: c.default || '',
        origType: typeSql({ type: base, len: p.len, scale: p.scale }),
      };
    });
  } else {
    designer.rows = [newDesignerRow()];
  }

  document.getElementById('designerTitle').textContent = t ? '表设计器 · ' + t.name : '新建表';
  document.getElementById('designerTableName').value = t ? t.name : '';
  document.getElementById('designerMeta').textContent = t
    ? `${t.rows} 行 · ${t.columns.length} 列`
    : '填写表名和字段，保存即执行 CREATE TABLE';
  const hint = document.getElementById('designerHint');
  hint.textContent = '';
  hint.className = 'modal-hint';
  designerModalEl().classList.remove('hidden');
  setSqlPreviewOpen(false);
  renderDesigner();
  refreshDesignerSql();
  setTimeout(() => {
    const input = document.getElementById('designerTableName');
    if (!t) input.focus();
    else document.getElementById('designerRows').querySelector('input')?.focus();
  }, 30);
}

function closeDesigner() {
  designerModalEl().classList.add('hidden');
  designer = null;
}

function numInputEl(value, onInput) {
  const i = document.createElement('input');
  i.type = 'number';
  i.min = '0';
  i.value = value ? String(value) : '';
  i.addEventListener('input', () => {
    const v = parseInt(i.value, 10);
    onInput(isNaN(v) ? 0 : v);
  });
  return i;
}

function checkEl(checked, onChange) {
  const c = document.createElement('input');
  c.type = 'checkbox';
  c.checked = !!checked;
  c.addEventListener('change', () => onChange(c.checked));
  return c;
}

function renderDesigner() {
  const tb = document.getElementById('designerRows');
  tb.innerHTML = '';
  designer.rows.forEach((r, i) => {
    const tr = el('tr');
    if (i === designer.sel) tr.classList.add('sel');
    tr.addEventListener('mousedown', () => {
      designer.sel = i;
      for (const node of tb.children) node.classList.remove('sel');
      tr.classList.add('sel');
    });
    tr.appendChild(el('td', 'c-idx', String(i + 1)));

    const tdName = el('td', 'c-name');
    const nameIn = document.createElement('input');
    nameIn.type = 'text';
    nameIn.spellcheck = false;
    nameIn.placeholder = '字段名';
    nameIn.value = r.name;
    nameIn.addEventListener('input', () => { r.name = nameIn.value; refreshDesignerSql(); });
    tdName.appendChild(nameIn);
    tr.appendChild(tdName);

    const tdType = el('td', 'c-type');
    const sel = document.createElement('select');
    for (const ty of TYPE_LIST) {
      const o = document.createElement('option');
      o.value = ty;
      o.textContent = ty;
      sel.appendChild(o);
    }
    sel.value = r.type;
    tdType.appendChild(sel);
    tr.appendChild(tdType);

    const tdLen = el('td', 'c-len');
    const lenIn = numInputEl(r.len, v => { r.len = v; refreshDesignerSql(); });
    tdLen.appendChild(lenIn);
    tr.appendChild(tdLen);

    const tdScale = el('td', 'c-len');
    const scaleIn = numInputEl(r.scale, v => { r.scale = v; refreshDesignerSql(); });
    tdScale.appendChild(scaleIn);
    tr.appendChild(tdScale);

    const syncLen = () => {
      const needLen = r.type === 'varchar' || r.type === 'char' || r.type === 'numeric';
      lenIn.disabled = !needLen;
      scaleIn.disabled = r.type !== 'numeric';
      lenIn.placeholder = needLen ? (r.type === 'numeric' ? '精度' : '长度') : '';
    };
    sel.addEventListener('change', () => { r.type = sel.value; syncLen(); refreshDesignerSql(); });
    syncLen();

    const tdNotNull = el('td', 'c-flag');
    tdNotNull.appendChild(checkEl(r.notNull, v => { r.notNull = v; refreshDesignerSql(); }));
    tr.appendChild(tdNotNull);

    const tdPk = el('td', 'c-flag');
    const pkIn = checkEl(r.pk, v => {
      r.pk = v;
      if (v) {
        // 主键只留一个（引擎是单列主键）
        designer.rows.forEach(o => { if (o !== r) o.pk = false; });
        renderDesigner();
      }
      refreshDesignerSql();
    });
    tdPk.appendChild(pkIn);
    tr.appendChild(tdPk);

    const tdUniq = el('td', 'c-flag');
    tdUniq.appendChild(checkEl(r.uniq, v => { r.uniq = v; refreshDesignerSql(); }));
    tr.appendChild(tdUniq);

    const tdDef = el('td', 'c-def');
    const defIn = document.createElement('input');
    defIn.type = 'text';
    defIn.spellcheck = false;
    defIn.placeholder = '如 now()';
    defIn.value = r.def || '';
    defIn.addEventListener('input', () => { r.def = defIn.value; refreshDesignerSql(); });
    tdDef.appendChild(defIn);
    tr.appendChild(tdDef);

    const tdDel = el('td', 'c-del');
    const del = el('button', 'row-del', '×');
    del.title = '删除该字段';
    del.addEventListener('click', ev => {
      ev.stopPropagation();
      designer.rows.splice(i, 1);
      if (designer.sel >= designer.rows.length) designer.sel = designer.rows.length - 1;
      renderDesigner();
      refreshDesignerSql();
    });
    tdDel.appendChild(del);
    tr.appendChild(tdDel);

    tb.appendChild(tr);
  });
}

// 按「原结构 vs 当前编辑」的差异生成语句。引擎的 ALTER 只支持
// 加列/删列/改列名/改类型，NOT NULL 和默认值改不了，这里给注释提示。
function buildDesignerSql() {
  const nameInput = document.getElementById('designerTableName');
  const name = (nameInput ? nameInput.value : '').trim();
  if (!name) return { error: '表名不能为空', stmts: [], sql: '' };

  const rows = designer.rows.filter(r => (r.name || '').trim());
  if (!rows.length) return { error: '至少要有一个字段', stmts: [], sql: '' };

  const seen = new Set();
  for (const r of rows) {
    const k = r.name.trim().toLowerCase();
    if (seen.has(k)) return { error: '字段名重复：' + r.name.trim(), stmts: [], sql: '' };
    seen.add(k);
  }

  const stmts = [];
  const warns = [];

  if (designer.mode === 'new') {
    const lines = rows.map(r => {
      let s = '  ' + qi(r.name.trim()) + ' ' + typeSql(r);
      if (r.pk) s += ' PRIMARY KEY';
      else {
        if (r.notNull) s += ' NOT NULL';
        if (r.uniq) s += ' UNIQUE';
      }
      if (r.def.trim()) s += ' DEFAULT ' + r.def.trim();
      return s;
    });
    stmts.push(`CREATE TABLE ${qi(name)} (\n${lines.join(',\n')}\n);`);
  } else {
    const old = designer.origName;
    if (name.toLowerCase() !== old.toLowerCase()) {
      stmts.push(`ALTER TABLE ${qi(old)} RENAME TO ${qi(name)};`);
    }
    const tbl = qi(name);

    // 1) 改列名（后面的语句都用新名字）
    for (const r of rows) {
      if (r.orig && r.orig.toLowerCase() !== r.name.trim().toLowerCase()) {
        stmts.push(`ALTER TABLE ${tbl} RENAME COLUMN ${qi(r.orig)} TO ${qi(r.name.trim())};`);
      }
    }
    // 2) 改类型
    for (const r of rows) {
      if (!r.orig) continue;
      const now = typeSql(r);
      if (r.origType && now !== r.origType) {
        stmts.push(`ALTER TABLE ${tbl} ALTER COLUMN ${qi(r.name.trim())} TYPE ${now};`);
      }
    }
    // 3) 加列
    for (const r of rows) {
      if (r.orig) continue;
      let s = `ALTER TABLE ${tbl} ADD COLUMN ${qi(r.name.trim())} ${typeSql(r)}`;
      if (r.notNull) s += ' NOT NULL';
      if (r.uniq) s += ' UNIQUE';
      if (r.def.trim()) s += ' DEFAULT ' + r.def.trim();
      stmts.push(s + ';');
    }
    // 4) 删列（原来有、现在列表里没有）
    const kept = new Set(rows.filter(r => r.orig).map(r => r.orig.toLowerCase()));
    for (const o of designer.origCols) {
      if (!kept.has(o.name.toLowerCase())) {
        stmts.push(`ALTER TABLE ${tbl} DROP COLUMN ${qi(o.name)};`);
      }
    }
    // 5) 不支持的改动，只在预览里提示
    for (const r of rows) {
      if (!r.orig) continue;
      const o = designer.origCols.find(x => x.name === r.orig);
      if (!o) continue;
      if (!!o.notNull !== !!r.notNull) warns.push(`${r.name}: 引擎不支持修改已有列的 NOT NULL，已忽略`);
      if ((o.def || '') !== (r.def || '').trim()) warns.push(`${r.name}: 引擎不支持修改已有列的默认值，已忽略`);
      if (!!o.uniq !== !!r.uniq) warns.push(`${r.name}: 引擎不支持给已有列加/去 UNIQUE，已忽略`);
      if (!!o.pk !== !!r.pk && !r.pk) warns.push(`${r.name}: 引擎不支持取消已有列的主键，已忽略`);
      if (!!o.pk !== !!r.pk && r.pk) warns.push(`${r.name}: 引擎不支持给已有表新增主键，已忽略`);
    }
  }

  let sql = stmts.join('\n');
  if (warns.length) sql = warns.map(w => '-- ' + w).join('\n') + '\n' + sql;
  return { stmts, warns, sql };
}

function refreshDesignerSql() {
  if (!designer) return;
  const pre = document.getElementById('designerSql');
  const built = buildDesignerSql();
  if (!pre.classList.contains('hidden')) {
    pre.textContent = built.error ? '-- ' + built.error : (built.stmts.length ? built.sql : '-- 没有修改');
  }
  return built;
}

function setSqlPreviewOpen(open) {
  const pre = document.getElementById('designerSql');
  const btn = document.getElementById('btnSqlPreview');
  pre.classList.toggle('hidden', !open);
  btn.classList.toggle('on', open);
  if (open) {
    const built = buildDesignerSql();
    pre.textContent = built.error ? '-- ' + built.error : (built.stmts.length ? built.sql : '-- 没有修改');
  }
}

async function saveDesigner() {
  if (!designer) return;
  const hint = document.getElementById('designerHint');
  const built = buildDesignerSql();
  if (built.error) {
    hint.textContent = built.error;
    hint.className = 'modal-hint err';
    return;
  }
  if (!built.stmts.length) {
    hint.textContent = '没有需要保存的修改';
    hint.className = 'modal-hint';
    return;
  }
  if (!ensureNoTx()) return;
  const btn = document.getElementById('btnDesignerSave');
  const isNew = designer.mode === 'new';
  btn.disabled = true;
  hint.textContent = '执行中…';
  hint.className = 'modal-hint';
  try {
    const data = await api('/api/query', { sql: built.sql });
    updateTx(data.tx);
    renderResults(data);
    if (data.error) {
      hint.textContent = data.error.message || '保存失败';
      hint.className = 'modal-hint err';
      await loadSchema();
      return;
    }
    if (isNew) {
      // 新建完直接展开，省得再点一次
      const created = document.getElementById('designerTableName').value.trim();
      if (created) expandedTables.add(created);
    }
    closeDesigner();
    await loadSchema();
    toast(isNew ? '表已创建' : '表结构已更新');
  } catch (e) {
    hint.textContent = '保存失败：' + e.message;
    hint.className = 'modal-hint err';
  } finally {
    btn.disabled = false;
  }
}

/* ---------------- 绑定 ---------------- */

document.getElementById('btnNewTable').addEventListener('click', () => openDesigner(null));

treeEl.addEventListener('contextmenu', ev => {
  if (ev.target.closest('.table-row')) return; // 表节点自己处理
  ev.preventDefault();
  showCtxMenu(ev.clientX, ev.clientY, blankMenuItems());
});

document.addEventListener('mousedown', ev => {
  const m = ctxMenuEl();
  if (m && !m.classList.contains('hidden') && !ev.target.closest('.ctx-menu')) hideCtxMenu();
});
window.addEventListener('resize', hideCtxMenu);

document.getElementById('btnDesignerClose').addEventListener('click', closeDesigner);
document.getElementById('btnDesignerCancel').addEventListener('click', closeDesigner);
document.getElementById('btnDesignerSave').addEventListener('click', saveDesigner);
document.getElementById('btnSqlPreview').addEventListener('click', () => {
  const pre = document.getElementById('designerSql');
  setSqlPreviewOpen(pre.classList.contains('hidden'));
});
document.getElementById('btnAddCol').addEventListener('click', () => {
  if (!designer) return;
  designer.rows.push(newDesignerRow());
  designer.sel = designer.rows.length - 1;
  renderDesigner();
  refreshDesignerSql();
});
document.getElementById('btnDelCol').addEventListener('click', () => {
  if (!designer) return;
  if (designer.sel < 0 || designer.sel >= designer.rows.length) {
    toast('先在网格里点一行，再删除', true);
    return;
  }
  designer.rows.splice(designer.sel, 1);
  if (designer.sel >= designer.rows.length) designer.sel = designer.rows.length - 1;
  renderDesigner();
  refreshDesignerSql();
});
document.getElementById('btnSetPk').addEventListener('click', () => {
  if (!designer) return;
  const r = designer.rows[designer.sel];
  if (!r) { toast('先选一行', true); return; }
  const next = !r.pk;
  designer.rows.forEach(o => { o.pk = false; });
  r.pk = next;
  renderDesigner();
  refreshDesignerSql();
});
document.getElementById('designerTableName').addEventListener('input', refreshDesignerSql);

designerModalEl().addEventListener('keydown', ev => {
  if (ev.key === 'Escape') closeDesigner();
  if ((ev.ctrlKey || ev.metaKey) && ev.key === 'Enter') { ev.preventDefault(); saveDesigner(); }
});

document.addEventListener('keydown', ev => {
  if (ev.key !== 'Escape') return;
  const m = ctxMenuEl();
  if (m && !m.classList.contains('hidden')) hideCtxMenu();
  if (designer && !designerModalEl().classList.contains('hidden')) closeDesigner();
});

/* ---------------- SQL 历史（localStorage，最近 30 条） ---------------- */

const HISTORY_KEY = 'godb.history';
const HISTORY_MAX = 30;

function loadHistory() {
  try {
    const list = JSON.parse(localStorage.getItem(HISTORY_KEY) || '[]');
    return Array.isArray(list) ? list.filter(x => x && typeof x.sql === 'string') : [];
  } catch (e) {
    return [];
  }
}

function pushHistory(sql) {
  const text = String(sql || '').trim();
  if (!text) return;
  const list = loadHistory();
  // 连着执行同一条不重复记
  if (list.length && list[0].sql === text) return;
  list.unshift({ sql: text, at: Date.now() });
  try {
    localStorage.setItem(HISTORY_KEY, JSON.stringify(list.slice(0, HISTORY_MAX)));
  } catch (e) {
    /* 存储满了就算了，历史不是关键功能 */
  }
}

function fmtWhen(ms) {
  const d = new Date(ms || Date.now());
  const p = n => String(n).padStart(2, '0');
  const today = new Date();
  const sameDay = d.toDateString() === today.toDateString();
  const hm = `${p(d.getHours())}:${p(d.getMinutes())}`;
  return sameDay ? hm : `${d.getMonth() + 1}/${d.getDate()} ${hm}`;
}

function openHistory() {
  const modal = document.getElementById('historyModal');
  const body = document.getElementById('historyBody');
  const hint = document.getElementById('historyHint');
  const list = loadHistory();
  body.innerHTML = '';
  hint.textContent = list.length ? `共 ${list.length} 条（本机浏览器保存）` : '';
  if (!list.length) {
    body.appendChild(el('div', 'hist-empty', '还没有执行过 SQL。'));
  }
  for (const item of list) {
    const b = el('button', 'hist-item');
    b.appendChild(el('span', 'hist-when', fmtWhen(item.at)));
    b.appendChild(el('span', 'hist-sql', item.sql));
    b.title = '点击填入编辑器';
    b.addEventListener('click', () => {
      editor.value = item.sql;
      autoSize();
      editor.focus();
      closeHistory();
    });
    body.appendChild(b);
  }
  modal.classList.remove('hidden');
}

function closeHistory() {
  document.getElementById('historyModal').classList.add('hidden');
}

document.getElementById('btnHistory').addEventListener('click', openHistory);
document.getElementById('btnHistoryClose').addEventListener('click', closeHistory);
document.getElementById('btnHistoryCancel').addEventListener('click', closeHistory);
document.getElementById('btnHistoryClear').addEventListener('click', () => {
  if (!confirm('清空本机保存的 SQL 历史？')) return;
  localStorage.removeItem(HISTORY_KEY);
  openHistory();
});
document.getElementById('historyModal').addEventListener('click', ev => {
  if (ev.target.id === 'historyModal') closeHistory();
});
document.addEventListener('keydown', ev => {
  if (ev.key !== 'Escape') return;
  const m = document.getElementById('historyModal');
  if (m && !m.classList.contains('hidden')) closeHistory();
});
