'use strict';

const editor = document.getElementById('editor');
const resultsEl = document.getElementById('results');
const treeEl = document.getElementById('schemaTree');
const txBar = document.getElementById('txBar');
const txText = document.getElementById('txText');
const txPill = document.getElementById('txPill');

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
  if (data.clockGranNs) clockGranNs = data.clockGranNs;
  const waitNs = data.tx && data.tx.waitNs ? data.tx.waitNs : 0;
  if (waitNs > 0) {
    const note = el('div', 'wait-note', `本次执行等待表锁 ${fmtTook(waitNs)}`);
    note.title = `等待锁 ${waitNs} ns`;
    resultsEl.appendChild(note);
  }
  const list = data.results || [];
  for (const r of list) {
    resultsEl.appendChild(resultCard(r));
  }
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

function resultCard(r) {
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
    const max = Math.min(r.rows.length, 300);
    for (let i = 0; i < max; i++) {
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
    card.appendChild(wrap);
    if (r.rows.length > max) {
      card.appendChild(el('div', 'more-rows', `还有 ${r.rows.length - max} 行未显示（前端最多渲染 300 行）`));
    }
  } else if (r.columns && r.columns.length && !r.message) {
    card.appendChild(el('div', 'msg', '（0 行）'));
  }
  return card;
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
    renderSchema(data.tables || []);
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
    const node = el('div', 'table-node open');
    const row = el('div', 'table-row');
    const caret = el('span', 'caret', '▶');
    caret.addEventListener('click', ev => {
      ev.stopPropagation();
      node.classList.toggle('open');
    });
    row.appendChild(caret);
    row.appendChild(el('span', 'tname', t.name));
    row.appendChild(el('span', 'tcount', String(t.rows)));
    row.addEventListener('click', () => {
      editor.value = `SELECT * FROM ${t.name} LIMIT 100;`;
      autoSize();
      run();
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
        editor.value = `SELECT ${c.name} FROM ${t.name} LIMIT 100;`;
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
  // 支持 #locks 直接打开锁面板
  if (location.hash === '#locks') toggleLocks(true);
})();
