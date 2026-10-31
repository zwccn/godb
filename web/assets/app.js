'use strict';

// ---------------- 编辑器 ----------------
//
// 用 CodeMirror 5（vendor 里，不走 CDN）做 SQL 高亮 + 智能提示。它把 textarea
// 藏起来换成自己的 DOM，所以这里包一层 `editor` 适配器：上层代码继续写
// `editor.value` / `editor.focus()`，底下走 CodeMirror 还是原生 textarea 由
// cm 是否为 null 决定 —— 万一 vendor 脚本没加载起来，编辑区照样能用。
const editorEl = document.getElementById('editor');
let cm = null; // CodeMirror 实例；null 表示退回原生 textarea

const editor = {
  get value() { return cm ? cm.getValue() : editorEl.value; },
  set value(v) {
    const text = v == null ? '' : String(v);
    if (cm) {
      cm.setValue(text);
      cm.setCursor({ line: cm.lineCount() - 1, ch: 0 });
    } else {
      editorEl.value = text;
    }
    autoSize();
  },
  focus() { if (cm) cm.focus(); else editorEl.focus(); },
  getBoundingClientRect() {
    return (cm ? cm.getWrapperElement() : editorEl).getBoundingClientRect();
  },
};
const treeEl = document.getElementById('schemaTree');
const txBar = document.getElementById('txBar');
const txText = document.getElementById('txText');
const txPill = document.getElementById('txPill');

// ---------- 皮肤 ----------
// 颜色全在 CSS 变量里，这里只负责切 html[data-theme]，并把选择记在本机。
const THEME_KEY = 'godb.theme';
// 第一次打开用哪套皮肤。深色是 CSS 里的 :root（不加属性），所以「默认」和
// 「无属性」不是一回事：选深色会主动清掉属性，首次访问则要先写上 data-theme="steel"。
const DEFAULT_THEME = 'steel';
// 顺序即设置页里的展示顺序；默认的那套排第一
const THEME_LIST = [
  { id: 'steel', name: '科技灰' },
  { id: 'dark', name: '深色' },
  { id: 'sky', name: '淡蓝' },
  { id: 'taro', name: '芋紫' },
  { id: 'mint', name: '薄荷绿' },
];
const THEMES = THEME_LIST.map(t => t.id);

function applyTheme(name, save) {
  if (!THEMES.includes(name)) name = DEFAULT_THEME;
  // 深色是默认值，挂在 :root 上，所以不加属性即可
  if (name === 'dark') {
    document.documentElement.removeAttribute('data-theme');
  } else {
    document.documentElement.setAttribute('data-theme', name);
  }
  // 皮肤按钮现在渲染在设置页里，每次都是新建的元素，所以这里按当前 DOM 查
  for (const pick of document.querySelectorAll('.theme-pick')) {
    pick.setAttribute('aria-pressed', String(pick.dataset.theme === name));
  }
  // 只有用户点按钮时才记为偏好；链接参数（?theme=）只是当次预览
  if (save) {
    try { localStorage.setItem(THEME_KEY, name); } catch (e) {}
  }
}

// 用事件委托：设置页每次打开都会重建皮肤按钮，逐个绑会绑到已经销毁的节点上
document.addEventListener('click', ev => {
  const pick = ev.target.closest && ev.target.closest('.theme-pick');
  if (pick) applyTheme(pick.dataset.theme, true);
});
// 进入页面时按上次的选择点亮按钮（属性已经由 head 里的内联脚本设好了）。
// 兜底是 'dark' 而不是 DEFAULT_THEME：属性缺省等于深色（:root 就是那套变量），
// 首次访问的 'steel' 由内联脚本写上去。
applyTheme(document.documentElement.getAttribute('data-theme') || 'dark', false);

// ---------- 字号 ----------
// CSS 里所有 font-size 都是 calc(基准px * var(--fs))，所以这里只改一个变量。
const FS_KEY = 'godb.fs';
const FS_DEFAULT = 120;
const FS_MIN = 85;
const FS_MAX = 165;
const FS_STEP = 5;

function clampFs(pct) {
  if (!Number.isFinite(pct)) return FS_DEFAULT;
  // 对齐到 5% 的档位，避免链接参数带进奇怪的数字
  const snapped = Math.round(pct / FS_STEP) * FS_STEP;
  return Math.min(FS_MAX, Math.max(FS_MIN, snapped));
}

function applyFontScale(pct, save) {
  const v = clampFs(pct);
  document.documentElement.style.setProperty('--fs', (v / 100).toFixed(2));
  // 字号控件在设置页里是动态渲染的，用类名查而不是缓存的引用
  for (const node of document.querySelectorAll('.fs-value')) node.textContent = v + '%';
  for (const btn of document.querySelectorAll('.fs-btn')) {
    const step = Number(btn.dataset.fsStep);
    btn.disabled = step < 0 ? v <= FS_MIN : v >= FS_MAX;
  }
  if (save) {
    try { localStorage.setItem(FS_KEY, String(v)); } catch (e) {}
  }
  return v;
}

// 同样走委托（设置页重建后不需要重新绑定）
document.addEventListener('click', ev => {
  const btn = ev.target.closest && ev.target.closest('.fs-btn');
  if (!btn || btn.disabled) return;
  applyFontScale(clampFs(currentFsPct()) + Number(btn.dataset.fsStep) * FS_STEP, true);
});

// 当前生效的百分比（从 --fs 反推，这样链接参数 ?fs= 也算在内）
function currentFsPct() {
  const raw = getComputedStyle(document.documentElement).getPropertyValue('--fs');
  const n = parseFloat(raw);
  return Number.isFinite(n) ? Math.round(n * 100) : FS_DEFAULT;
}

applyFontScale(currentFsPct(), false);

// 最近一次 /api/schema 的结果，右键菜单和表设计器都从这里取，避免每次再请求。
let schemaCache = [];
// 当前会话是否有未提交的事务（DDL 不能在事务里执行）。
let txActive = false;
// 顶栏圆点要同时反映「系统健康」和「事务状态」，两边都会更新它，
// 所以把最近一次的事务状态记下来，谁先到都能拼出正确的提示文字。
let lastTx = { active: false, aborted: false, changes: 0 };
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
-- 满意就执行 COMMIT; 后悔就执行 ROLLBACK;
-- 想整体还原示例数据：设置 → 数据 → 载入示例数据`,
  ddl: `DROP TABLE IF EXISTS projects;
CREATE TABLE projects (
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
  alter: `DROP TABLE IF EXISTS demo_alter;
CREATE TABLE demo_alter (id INTEGER, note TEXT);
INSERT INTO demo_alter VALUES (1, 'before');

ALTER TABLE demo_alter ADD COLUMN level INTEGER DEFAULT 1;
SELECT * FROM demo_alter;          -- 新列按 DEFAULT 回填

ALTER TABLE demo_alter DROP COLUMN level;
SELECT * FROM demo_alter;`,
  funcs: `-- 常用函数与 interval（不依赖示例表，可反复执行）
SELECT random()                         AS r,
       lpad('1', 6, '0')                AS code,
       now() - INTERVAL '1 day'         AS 昨天此时,
       DATE '2024-01-02' - INTERVAL '1 day' AS 前一天,
       TIMESTAMP '2024-01-01 10:00:00' + INTERVAL '02:30:00' AS 加两个半小时;

SELECT unnest(array[10, 20, 30]) AS n;   -- 数组展开成多行

SELECT pg_size_pretty(pg_database_size(current_database())) AS 库大小;`,
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

// ---------------- 编辑器高度 ----------------
//
// 两种模式：
//   自动（默认）—— 跟着内容长高，到 55vh 封顶，之后自己滚；
//   手动（拖过底边）—— 固定高度，存在 localStorage，下次打开还是这么高。
// 双击拖手回到自动模式。

const EDITOR_MIN = 180;               // 桌面：约 8 行，也是 CSS 的 min-height
const EDITOR_MIN_NARROW = 104;        // 窄屏：约 4 行
const EDITOR_RESULT_ROOM = 320;       // 至少给下面的结果区留这么多
const EDITOR_H_KEY = 'godb.editorH';

let editorManualH = 0; // 0 表示自动

// 手机上一行 SQL 配 180px 的编辑器会吃掉半屏（390×844 里编辑器占了 1/3），
// 所以窄屏把「最低高度」降到 4 行左右，其余照样跟着内容长。
function editorMinH() {
  return window.innerWidth <= 720 ? EDITOR_MIN_NARROW : EDITOR_MIN;
}

function editorMaxH() {
  return Math.max(editorMinH(), window.innerHeight - EDITOR_RESULT_ROOM);
}

function setEditorHeight(h) {
  const px = Math.round(h);
  if (cm) {
    // CodeMirror 的官方接口：直接定 wrapper 高度，内部自己滚
    cm.setSize(null, px + 'px');
    return;
  }
  editorEl.style.height = px + 'px';
}

function autoSize() {
  if (editorManualH) {
    // 手动定过高度就别再跟着内容跳了，否则刚拖好的框一打字又变回去
    setEditorHeight(Math.min(editorManualH, editorMaxH()));
    return;
  }
  // 内容高度 + 最低 180px：一行 SQL 时不该塌成一条缝
  //
  // CodeMirror 不用 getScrollInfo().height —— 它对没渲染的行是估算值，
  // 刚 setValue 完往往还是旧的，表现为「贴了 40 行进来，高度却不涨」。
  // 行数 × 单行高 + 上下内边距（CSS 里 .CodeMirror-lines 是 10px）更可靠。
  let content;
  if (cm) {
    content = cm.lineCount() * cm.defaultTextHeight() + 20;
  } else {
    editorEl.style.height = 'auto';
    content = editorEl.scrollHeight + 2;
  }
  const h = Math.max(editorMinH(), Math.min(content, window.innerHeight * 0.55));
  setEditorHeight(h);
}

function loadEditorHeight() {
  const n = parseInt(localStorage.getItem(EDITOR_H_KEY), 10);
  if (n > 0) editorManualH = Math.max(editorMinH(), n);
}

// initCodeMirror 把 textarea 换成 CodeMirror（本地 vendor），并接上补全。
// vendor 没加载起来就静默保持 textarea —— 宁可少个高亮，也不能让编辑区不可用。
function initCodeMirror() {
  if (!window.CodeMirror || !window.GodbHint) return;
  cm = CodeMirror.fromTextArea(editorEl, {
    mode: 'text/x-pgsql', // 用 PostgreSQL 方言做高亮
    theme: 'godb',
    lineWrapping: true,
    indentUnit: 2,
    tabSize: 2,
    lineNumbers: false,
    placeholder: '-- 在这里写 SQL，Ctrl+Enter 执行；Ctrl+Space 唤出补全',
    hintOptions: {
      hint: window.GodbHint.complete,
      // 渲染函数挂在每条候选上（show-hint 是逐条 render），这里不配全局 render
      completeSingle: false,     // 只有一个候选时不要自动吞掉，让人自己按 Tab/Enter
      completeOnSingleClick: true, // 默认要双击才插入，太反直觉：点一下就该补上
    },
    extraKeys: {
      'Ctrl-Enter': () => run(),
      'Cmd-Enter': () => run(),
      'Shift-Ctrl-F': () => formatSQL(),
      'Shift-Cmd-F': () => formatSQL(),
      'Ctrl-Space': 'autocomplete',
      Tab: c => c.replaceSelection('  ', 'end'),
    },
  });
  cm.on('change', onEditorChange);

  // 输入法：网页**没法**替用户切换操作系统的中英文状态（老 IE 的 ime-mode 早在
  // Chrome/Firefox 里被移除了），能做的只有这两件小事：
  //   1. inputmode=latin：手机上弹英文键盘，不用每次先切一下；
  //   2. 关掉自动大写/自动纠错/拼写检查：否则手机常把 select 悄悄改成 Select，
  //      输错还会被"纠正"成别的词。
  // 桌面端仍然要自己按 Shift 切英文 —— 这是浏览器安全模型决定的，绕不过去。
  const inputField = cm.getInputField && cm.getInputField();
  if (inputField) {
    inputField.setAttribute('inputmode', 'latin');
    inputField.setAttribute('autocapitalize', 'off');
    inputField.setAttribute('autocorrect', 'off');
    inputField.setAttribute('autocomplete', 'off');
    inputField.setAttribute('spellcheck', 'false');
    inputField.setAttribute('lang', 'en');
  }

  // 打字时自动弹补全：只在「正在写一个词」或刚打完点号时弹，
  // 空格、逗号、括号后不弹（那会儿弹出来只会挡住下一句 SQL）。
  cm.on('inputRead', (c, change) => {
    if (c.state.completionActive) return;
    const typed = (change.text || []).join('');
    if (!/[A-Za-z_.]/.test(typed.slice(-1))) return;
    const cur = c.getCursor();
    const before = c.getLine(cur.line).slice(0, cur.ch);
    if (!/(^|[^\w$])[A-Za-z_][\w$]*$/.test(before) && !/\.$/.test(before)) return;
    c.showHint();
  });

  // 补全要用当前库的真实表结构：每次现取，切库/建表后立刻生效
  window.GodbHint.setSchemaProvider(() => schemaCache);
}

(function initEditorResize() {
  const grip = document.getElementById('editorResize');
  if (!grip) return;

  grip.addEventListener('pointerdown', ev => {
    if (ev.button !== 0) return;
    ev.preventDefault();
    const startY = ev.clientY;
    const startH = editor.getBoundingClientRect().height;
    document.body.classList.add('editor-resizing');
    try { grip.setPointerCapture(ev.pointerId); } catch (e) {}

    const move = e => {
      const h = Math.max(editorMinH(), Math.min(startH + (e.clientY - startY), editorMaxH()));
      editorManualH = h;
      setEditorHeight(h);
    };
    const up = () => {
      document.body.classList.remove('editor-resizing');
      grip.removeEventListener('pointermove', move);
      grip.removeEventListener('pointerup', up);
      grip.removeEventListener('pointercancel', up);
      try { grip.releasePointerCapture(ev.pointerId); } catch (e) {}
      if (editorManualH) localStorage.setItem(EDITOR_H_KEY, String(Math.round(editorManualH)));
    };
    grip.addEventListener('pointermove', move);
    grip.addEventListener('pointerup', up);
    grip.addEventListener('pointercancel', up);
  });

  // 双击：回到自动高度（顺便清掉记忆，否则刷新又回来了）
  grip.addEventListener('dblclick', () => {
    editorManualH = 0;
    localStorage.removeItem(EDITOR_H_KEY);
    autoSize();
  });
})();

// 窗口变矮时把编辑器夹回来，别把结果区挤成 0
window.addEventListener('resize', () => {
  if (editorManualH > editorMaxH()) {
    editorManualH = editorMaxH();
    setEditorHeight(editorManualH);
  } else if (!editorManualH) {
    autoSize();
  }
});

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

/* ---------------- 弹窗的关闭规则 ---------------- */

// 所有弹窗（设置 / 数据库 / 表设计器 / 输入 / 历史）**点遮罩不关闭**。
// 理由：这些弹窗里都有正在填的内容（表名、字段、库名、SQL 历史），误点一下空白
// 就丢掉输入是最容易招骂的交互。关它只有三条路：× 按钮、取消/关闭按钮、Esc。
//
// 以前设置/数据库/历史三个弹窗各自挂了「点遮罩关闭」，删掉时一并集中到这里说明，
// 免得后加的弹窗又照着旧代码抄回去。抽屉（锁状态）本来就不吃遮罩点击，无需处理。

/* ---------------- 弹窗：拖动与缩放 ---------------- */

// 所有弹窗都能拖着走、拽右下角改大小，双击标题栏回居中。
//
// 实现上有个前提：卡片平时靠 `.modal { place-items:center }` 居中，没有任何
// left/top。第一次拖动/缩放时才把当前视觉位置「冻结」成显式的 left/top
// （frameCard），之后才动得起来 —— 否则 transform/居中会一直覆盖位移。
//
// 不用 CSS 的 resize:both：原生把手在深色皮肤下看不清，而且会撞上
// max-height: calc(100vh - 80px)，拽到一半尺寸不动，手感像卡住。

const WIN_MIN_W = 320;  // 再窄表头和按钮就挤成一团了
const WIN_MIN_H = 180;
const WIN_EDGE = 56;    // 最多允许拖出屏幕，但要留这么多在屏幕内，免得拽丢
const WIN_MARGIN = 8;   // 缩放时与视口边缘的间隙

function winClamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }

// frameCard 把「居中」换成绝对坐标。只做一次（dataset.framed 标记）。
function frameCard(card) {
  if (card.dataset.framed === '1') return;
  const r = card.getBoundingClientRect();
  card.style.transform = 'none';
  card.style.margin = '0';
  card.style.width = r.width + 'px';
  // 不清掉 max-*：拖出去以后卡片可能比视口还高，得让它在屏幕内能滚
  card.style.left = winClamp(r.left, -(r.width - WIN_EDGE), window.innerWidth - WIN_EDGE) + 'px';
  card.style.top = winClamp(r.top, 0, Math.max(0, window.innerHeight - WIN_EDGE)) + 'px';
  card.dataset.framed = '1';
  card.classList.add('framed');
}

// resetCard 双击标题栏：回到默认居中、默认尺寸。
function resetCard(card) {
  for (const p of ['left', 'top', 'width', 'height', 'transform', 'margin', 'maxWidth', 'maxHeight']) {
    card.style.removeProperty(p);
  }
  card.dataset.framed = '0';
  delete card.dataset.userSize;
  card.classList.remove('framed', 'dragging', 'resizing');
  // 设置窗的高度是量出来的（见 lockSettingsHeight）：复位把 inline 高度清掉了，得重新量
  if (card.classList.contains('set-win')) lockSettingsHeight();
}

// startDrag 抓标题栏拖动。用 pointer 事件 + setPointerCapture：
// 鼠标移到卡片外面也照样跟随，不会「甩掉」。
function startDrag(card, ev) {
  if (ev.button !== 0) return;
  // 标题栏里的按钮（×）和输入框不该触发拖动
  if (ev.target.closest('button, input, select, textarea, a')) return;
  ev.preventDefault();
  frameCard(card);
  const r = card.getBoundingClientRect();
  const offX = ev.clientX - r.left;
  const offY = ev.clientY - r.top;
  const head = ev.currentTarget;
  card.classList.add('dragging');
  try { head.setPointerCapture(ev.pointerId); } catch (e) {}

  const move = e => {
    const left = winClamp(e.clientX - offX, -(r.width - WIN_EDGE), window.innerWidth - WIN_EDGE);
    const top = winClamp(e.clientY - offY, 0, Math.max(0, window.innerHeight - WIN_EDGE));
    card.style.left = left + 'px';
    card.style.top = top + 'px';
  };
  const up = () => {
    card.classList.remove('dragging');
    head.removeEventListener('pointermove', move);
    head.removeEventListener('pointerup', up);
    head.removeEventListener('pointercancel', up);
    try { head.releasePointerCapture(ev.pointerId); } catch (e) {}
  };
  head.addEventListener('pointermove', move);
  head.addEventListener('pointerup', up);
  head.addEventListener('pointercancel', up);
}

// startResize 拽右下角改大小。左上角固定，符合「抓角落」的直觉。
function startResize(card, ev) {
  if (ev.button !== 0) return;
  ev.preventDefault();
  ev.stopPropagation();
  frameCard(card);
  const r = card.getBoundingClientRect();
  // 一旦手动改尺寸，就必须放掉 CSS 的 max-width/max-height，
  // 否则拽到上限后尺寸不动、只有鼠标在动。
  card.style.width = r.width + 'px';
  card.style.height = r.height + 'px';
  card.style.maxWidth = 'none';
  card.style.maxHeight = 'none';
  // 手动改过尺寸：以后打开设置不再按内容重新量高度（见 lockSettingsHeight）
  card.dataset.userSize = '1';
  const grip = ev.currentTarget;
  card.classList.add('resizing');
  try { grip.setPointerCapture(ev.pointerId); } catch (e) {}

  const move = e => {
    const maxW = Math.max(WIN_MIN_W, window.innerWidth - r.left - WIN_MARGIN);
    const maxH = Math.max(WIN_MIN_H, window.innerHeight - r.top - WIN_MARGIN);
    card.style.width = winClamp(e.clientX - r.left, WIN_MIN_W, maxW) + 'px';
    card.style.height = winClamp(e.clientY - r.top, WIN_MIN_H, maxH) + 'px';
  };
  const up = () => {
    card.classList.remove('resizing');
    grip.removeEventListener('pointermove', move);
    grip.removeEventListener('pointerup', up);
    grip.removeEventListener('pointercancel', up);
    try { grip.releasePointerCapture(ev.pointerId); } catch (e) {}
  };
  grip.addEventListener('pointermove', move);
  grip.addEventListener('pointerup', up);
  grip.addEventListener('pointercancel', up);
}

function initWindow(modal) {
  const card = modal.querySelector('.modal-card');
  if (!card || card.dataset.win === '1') return;
  card.dataset.win = '1';

  const head = card.querySelector('.modal-head');
  if (head) {
    head.addEventListener('pointerdown', ev => startDrag(card, ev));
    head.addEventListener('dblclick', ev => {
      if (ev.target.closest('button')) return; // 双击 × 不该复位
      resetCard(card);
    });
  }
  const grip = document.createElement('div');
  grip.className = 'win-grip';
  grip.title = '拖动调整大小（双击标题栏复位）';
  grip.addEventListener('pointerdown', ev => startResize(card, ev));
  card.appendChild(grip);
}

document.querySelectorAll('.modal').forEach(initWindow);

// 视口变小（换显示器、缩小窗口）时把窗口夹回可见范围，别让它飘到屏幕外找不回来
window.addEventListener('resize', () => {
  document.querySelectorAll('.modal-card[data-framed="1"]').forEach(card => {
    const r = card.getBoundingClientRect();
    card.style.left = winClamp(r.left, -(r.width - WIN_EDGE), window.innerWidth - WIN_EDGE) + 'px';
    card.style.top = winClamp(r.top, 0, Math.max(0, window.innerHeight - WIN_EDGE)) + 'px';
    // 尺寸也别超出新视口
    if (card.style.width) {
      card.style.width = Math.min(r.width, window.innerWidth - r.left - WIN_MARGIN) + 'px';
    }
    if (card.style.height) {
      card.style.height = Math.min(r.height, window.innerHeight - r.top - WIN_MARGIN) + 'px';
    }
  });
});

/* ---------------- 执行 ---------------- */

let busy = false;

// 按分号切语句，但跳过字符串、引号标识符和注释里的分号。
// 之前直接用 split(';')，`SELECT 'a;b'` 会被切成两条，后面加 EXPLAIN 全乱套。
function splitStatements(sql) {
  const out = [];
  let buf = '';
  let quote = null; // 当前在 '...' 还是 "..." 里
  let i = 0;
  const n = sql.length;
  while (i < n) {
    const c = sql[i];
    const next = sql[i + 1];
    if (quote) {
      buf += c;
      if (c === quote) {
        if (next === quote) { buf += next; i += 2; continue; } // '' 是转义
        quote = null;
      }
      i++;
      continue;
    }
    if (c === "'" || c === '"') { quote = c; buf += c; i++; continue; }
    if (c === '-' && next === '-') {
      while (i < n && sql[i] !== '\n') buf += sql[i++];
      continue;
    }
    if (c === '/' && next === '*') {
      buf += '/*';
      i += 2;
      while (i < n && !(sql[i] === '*' && sql[i + 1] === '/')) buf += sql[i++];
      if (i < n) { buf += '*/'; i += 2; }
      continue;
    }
    if (c === ';') { out.push(buf); buf = ''; i++; continue; }
    buf += c;
    i++;
  }
  if (buf.trim()) out.push(buf);
  return out.map(s => s.trim()).filter(Boolean);
}

// 跳过空白和注释，取第一个词的小写形式（判断这条语句是什么类型）
function leadingKeyword(text) {
  let i = 0;
  const n = text.length;
  while (i < n) {
    const c = text[i];
    if (/\s/.test(c)) { i++; continue; }
    if (c === '-' && text[i + 1] === '-') {
      while (i < n && text[i] !== '\n') i++;
      continue;
    }
    if (c === '/' && text[i + 1] === '*') {
      i += 2;
      while (i < n && !(text[i] === '*' && text[i + 1] === '/')) i++;
      i += 2;
      continue;
    }
    const m = /^[A-Za-z_]+/.exec(text.slice(i));
    return m ? m[0].toLowerCase() : '';
  }
  return '';
}

// 「执行计划」要解释哪些语句：只给查询加 EXPLAIN。
// 两个坑都踩过：
//   1. 编辑器里已经写了 EXPLAIN（比如从帮助示例填进来）→ 会变成 EXPLAIN EXPLAIN；
//   2. 事务演示那种 BEGIN / UPDATE 被硬加上 EXPLAIN → 服务端报 only supports SELECT。
function withExplain(raw) {
  const keep = [];
  const skipped = [];
  for (const part of splitStatements(raw)) {
    const kw = leadingKeyword(part);
    if (!kw) continue;
    if (kw === 'explain') { keep.push(part + ';'); continue; }
    if (kw === 'select' || kw === 'with' || kw === 'values') {
      keep.push('EXPLAIN ' + part + ';');
      continue;
    }
    skipped.push(kw.toUpperCase());
  }
  return { sql: keep.join('\n'), skipped };
}

async function run(prefix) {
  if (busy) return;
  let sql = editor.value.trim();
  if (!sql) return;
  let skipped = [];
  if (prefix === 'explain') {
    const ex = withExplain(sql);
    skipped = ex.skipped;
    if (!ex.sql) {
      // 一条能解释的都没有：别把请求发出去换个看不懂的报错回来
      showPaneError({
        title: '无法生成执行计划',
        message: skipped.length
          ? `「执行计划」只能解释查询语句（SELECT / WITH / VALUES）。当前这批是 ${skipped.join('、')}，想直接跑它们请点「执行」。`
          : '编辑器里还没有可解释的语句。',
      });
      return;
    }
    sql = ex.sql;
  }
  busy = true;
  setRunning(true);
  lastSubmittedSQL = sql;
  const t0 = performance.now();
  try {
    const data = await api('/api/query', { sql });
    data.roundTripMs = performance.now() - t0;
    renderResults(data);
    updateTx(data.tx);
    pushHistory(sql);
    if (skipped.length) {
      toast('已解释查询，跳过 ' + skipped.join('、') + '（EXPLAIN 只解释 SELECT / WITH / VALUES）');
    }
    // DDL 之后刷新表结构
    const ddl = /(create|drop|alter)\s+table/i.test(sql);
    if (ddl || (data.error && /table/i.test(data.error.message))) {
      await loadSchema();
    }
    // 在编辑器里建/删库也要刷新库列表（否则下拉里看到的还是旧的）
    const touchedDB = (data.results || []).some(r =>
      /^(CREATE|DROP) DATABASE$/.test(r.tag || r.command || '')) ||
      /^(create|drop)\s+database/i.test(sql);
    if (touchedDB) await loadDBs();
  } catch (e) {
    showPaneError({ message: String(e.message || e) });
  } finally {
    busy = false;
    setRunning(false);
  }
}

function setRunning(on) {
  const btn = document.getElementById('btnRun');
  const label = document.getElementById('btnRunText');
  btn.disabled = on;
  btn.classList.toggle('is-running', on);
  // 只改文字节点：按钮里还有图标和快捷键，整体重写 innerHTML 会把它们冲掉
  if (label) label.textContent = on ? '执行中…' : '执行';
  btn.title = on ? '正在执行…' : '执行 SQL · Ctrl+Enter';
}

/* 美化 SQL：有选中就只美化选中那段，没选中整段。
   排版规则在 sql-format.js 里（本地实现，零依赖）；它宁可返回 null
   也不吐半截 SQL，所以这里拿不到结果就原样保留 + 提示。 */
function formatSQL() {
  const fmt = window.GodbFormat && window.GodbFormat.format;
  if (!fmt) { toast('美化模块没加载起来，刷新一下页面', true); return; }

  const all = editor.value;
  if (!all.trim()) { toast('编辑器是空的'); return; }

  const apply = (src) => {
    const out = fmt(src);
    if (out == null) {
      toast('括号或引号没配对，先补全再美化（内容没动）', true);
      return null;
    }
    if (out === src) { toast('已经是规整的写法了'); return null; }
    return out;
  };

  if (cm) {
    const sel = cm.getSelection();
    const out = apply(sel && sel.trim() ? sel : all);
    if (out == null) return;
    if (sel && sel.trim()) {
      cm.replaceSelection(out, 'around');
    } else {
      // 整篇替换走 replaceRange（一次 undo 就能回退），别用 setValue —— 那个会清掉撤销栈
      cm.operation(() => {
        cm.replaceRange(out, { line: 0, ch: 0 }, cm.posFromIndex(all.length));
      });
    }
    onEditorChange();
    toast(sel && sel.trim() ? '已美化选中的 SQL' : '已美化 SQL');
    cm.focus();
    return;
  }

  // 退回原生 textarea：execCommand 替换能保住撤销栈
  const start = editorEl.selectionStart;
  const end = editorEl.selectionEnd;
  const hasSel = end > start;
  const out = apply(hasSel ? all.slice(start, end) : all);
  if (out == null) return;
  editorEl.focus();
  editorEl.setSelectionRange(start, end);
  let done = false;
  try { done = document.execCommand('insertText', false, out); } catch (e) { done = false; }
  if (!done) editorEl.setRangeText(out, start, end, 'end');
  onEditorChange();
  toast(hasSel ? '已美化选中的 SQL' : '已美化 SQL');
}

/* ---------------- 渲染结果 ---------------- */

/* ---------------- 渲染结果 ----------------
   结构：#results > [tab 栏] + [pane]。
   pane 里只放「当前 tab」的内容：信息栏是一串文本，结果 tab 是一张表。
   同一时刻只有一处滚动（.info-body 或 .grid-wrap），不会再出现两条滚动条。 */

let resultTab = 0;          // 0 = 信息；1.. = 第 n 个「有行」的结果
let lastSubmittedSQL = '';  // 信息栏按语句回显 SQL 用
const WELCOME_HTML = (() => {
  const pane = document.getElementById('resultPane');
  return pane ? pane.innerHTML : '';
})();

// 清空结果区（切库、清库时用）：回到欢迎页或一句提示
function clearResults(text) {
  lastData = null;
  lastResults = [];
  resultTab = 0;
  const tabs = document.getElementById('resultTabs');
  if (tabs) { tabs.innerHTML = ''; tabs.classList.add('hidden'); }
  const pane = document.getElementById('resultPane');
  if (!pane) return;
  pane.innerHTML = text ? '' : WELCOME_HTML;
  if (text) pane.appendChild(el('div', 'results-empty', text));
}

// 错误不进结果 tab：写进信息栏，默认就停在信息栏
function showPaneError(err) {
  lastSubmittedSQL = '';
  renderResults({ results: [], error: err }, false);
}

// 只有真返回了行的语句才配一个「结果 N」tab；UPDATE / DDL 没有表可看，信息栏里说清楚即可
function resultTabModels(data) {
  const list = (data && data.results) || [];
  const models = [{ kind: 'info' }];
  list.forEach((r, i) => {
    if (hiddenResults.has(i)) return; // 手动关掉的
    if (r.rows && r.rows.length) models.push({ kind: 'res', r: r, idx: i });
  });
  return models;
}

// hiddenResults 记「用户手动关掉的第几个结果」。
// 新一次执行就清空（renderResults 里），所以关掉只是当前这份结果的视图操作，
// 不影响数据本身 —— CSV/JSON 导出读的还是完整 rows。
const hiddenResults = new Set();

function renderResults(data, keepTab) {
  if (!data) return;
  hiddenResults.clear();
  lastData = data;
  lastResults = (data.results || []).slice();
  if (data.clockGranNs) clockGranNs = data.clockGranNs;
  const models = resultTabModels(data);
  // 默认落点：正常拿到结果表就停「结果 1」，没有表（错误 / DDL / 空结果）就停信息栏
  if (!keepTab) resultTab = models.length > 1 ? 1 : 0;
  if (resultTab > models.length - 1) resultTab = 0;
  renderResultTabs(data);
  renderResultPane(data);
}

function renderResultTabs(data) {
  const tabs = document.getElementById('resultTabs');
  if (!tabs) return;
  const models = resultTabModels(data);
  const stmts = splitStatements(lastSubmittedSQL || '');
  tabs.innerHTML = '';

  if (resultTab > models.length - 1) resultTab = 0;

  const addTab = (label, idx, extra, tip, closeFor) => {
    // 文字单独放一层 span：tab 上还挂着关闭键「×」，textContent 会把 × 一起算进去，
    // 断言/读屏都只该看到「结果 1」。
    const b = el('button', 'result-tab' + (resultTab === idx ? ' on' : '') + (extra ? ' ' + extra : ''));
    b.appendChild(el('span', 'tab-label', label));
    b.type = 'button';
    b.setAttribute('role', 'tab');
    b.setAttribute('aria-selected', resultTab === idx ? 'true' : 'false');
    if (tip) b.title = tip;
    b.addEventListener('click', () => {
      resultTab = idx;
      renderResultTabs(data);
      renderResultPane(data);
    });
    if (closeFor !== undefined) {
      // 按钮里不能再放 button，用一个 span 当关闭键（点它不切 tab）
      const x = el('span', 'tab-x', '×');
      // 读屏时不要把「×」读进 tab 名字里（名字还是「结果 1」）
      x.setAttribute('aria-hidden', 'true');
      x.title = '关掉这个结果（只影响当前视图，数据没删）';
      x.addEventListener('click', ev => {
        ev.stopPropagation();
        closeResultTab(data, closeFor);
      });
      b.appendChild(x);
    }
    tabs.appendChild(b);
  };

  // closeResultTab 把某个结果从当前视图里摘掉；关的是当前 tab 就回到「信息」。
  const closeResultTab = (data, origIdx) => {
    const before = resultTabModels(data);
    const cur = before[resultTab];
    hiddenResults.add(origIdx);
    if (cur && cur.kind === 'res' && cur.idx === origIdx) resultTab = 0;
    renderResultTabs(data);
    renderResultPane(data);
  };

  addTab('信息', 0, data.error ? 'has-err' : '', '往返时间、每条语句的耗时与报错');

  let n = 0;
  for (let i = 1; i < models.length; i++) {
    n++;
    const r = models[i].r;
    const stmt = (stmts[models[i].idx] || '').trim();
    addTab('结果 ' + n, i, '',
      (r.tag || r.command || '') + (stmt ? ' · ' + stmt.split('\n')[0].slice(0, 80) : ''),
      models.length > 2 ? models[i].idx : undefined);
  }

  const sum = [];
  const cnt = (data.results || []).length;
  if (cnt > 1) sum.push('共 ' + cnt + ' 条语句');
  if (data.roundTripMs !== undefined) sum.push('往返 ' + data.roundTripMs.toFixed(2) + ' ms');
  if (sum.length) tabs.appendChild(el('span', 'result-tabs-meta', sum.join(' · ')));

  tabs.classList.remove('hidden');
}

function renderResultPane(data) {
  const pane = document.getElementById('resultPane');
  if (!pane) return;
  const models = resultTabModels(data);
  const cur = models[resultTab] || models[0];
  pane.innerHTML = '';
  if (!cur || cur.kind === 'info') pane.appendChild(infoPanel(data));
  else pane.appendChild(resultCard(cur.r, cur.idx));
}

// 信息栏：一行总览（往返 / 合计 / 等锁）+ 每条语句的 SQL 与状态
function infoPanel(data) {
  const box = el('div', 'info-body');
  const list = data.results || [];
  const stmts = splitStatements(lastSubmittedSQL || '');

  const sum = [];
  if (data.roundTripMs !== undefined) {
    sum.push('浏览器往返 ' + data.roundTripMs.toFixed(2) + ' ms（含网络与 JSON 编码，浏览器时钟精度）');
  }
  if (list.length > 1 && data.elapsedNs !== undefined) sum.push('服务端合计 ' + fmtTook(data.elapsedNs));
  const waitNs = data.tx && data.tx.waitNs ? data.tx.waitNs : 0;
  if (waitNs > 0) sum.push('本次执行等待表锁 ' + fmtTook(waitNs));
  if (sum.length) box.appendChild(el('div', 'info-sum', sum.join(' · ')));

  if (!list.length && !data.error) box.appendChild(el('div', 'results-empty', '没有返回结果'));

  list.forEach((r, i) => {
    const item = el('div', 'info-item');
    const head = el('div', 'info-head');
    head.appendChild(el('span', 'info-no', String(i + 1)));
    const sql = (stmts[i] || '').trim() || '（未记录 SQL）';
    const sqlEl = el('span', 'info-sql', sql);
    sqlEl.title = sql;
    head.appendChild(sqlEl);
    item.appendChild(head);
    item.appendChild(el('div', 'info-status', infoStatus(r)));
    box.appendChild(item);
  });

  if (data.error) {
    // 出错那条也要看得见 SQL：results 里没有它，就从语句列表里补一条
    const failIdx = (typeof data.error.stmt === 'number' && data.error.stmt >= 0)
      ? data.error.stmt : list.length;
    const failSQL = (stmts[failIdx] || '').trim();
    if (failSQL) {
      const item = el('div', 'info-item');
      const head = el('div', 'info-head');
      head.appendChild(el('span', 'info-no', String(failIdx + 1)));
      const sqlEl = el('span', 'info-sql', failSQL);
      sqlEl.title = failSQL;
      head.appendChild(sqlEl);
      item.appendChild(head);
      item.appendChild(el('div', 'info-status err', '失败 · ' + (data.error.title || '执行失败')));
      box.appendChild(item);
    }
    const err = errorCard(data.error);
    err.classList.add('info-error');
    box.appendChild(err);
  }
  return box;
}

function infoStatus(r) {
  const bits = [];
  if (typeof r.rowCount === 'number' && r.command === 'SELECT') bits.push(r.rowCount + ' 行');
  if (typeof r.affected === 'number' && r.affected > 0 && r.command !== 'SELECT') bits.push('影响 ' + r.affected + ' 行');
  if (r.tookNs !== undefined) bits.push('服务端 ' + fmtTook(r.tookNs));
  if (r.message) bits.push(r.message);
  return 'OK' + (bits.length ? ' · ' + bits.join(' · ') : '');
}

// 结果 tab 的内容：头（命令 / 行数 / 导出 / 执行计划）+ 表体 + 分页条
// cmdLabel 把 PG 的命令标签压成「动词」。
//
// 标签原文带行数（SELECT 5 / INSERT 0 5 / UPDATE 3），右边 meta 又写着
// 「5 行」或「影响 3 行」—— 两处并排就成了「SELECT 5  5 行」，第一眼容易
// 读成「SELECT 5」是一条语句。所以徽标只留动词，行数交给 meta。
// 带空格的命令（CREATE TABLE / ALTER TABLE…）原样保留。
function cmdLabel(r) {
  const raw = r.tag || r.command || 'OK';
  const m = /^([A-Z]+)(\s+\d+)+$/.exec(raw);
  return m ? m[1] : raw;
}

function resultCard(r, idx) {
  const card = el('div', 'result-card');
  const head = el('div', 'result-head');
  head.appendChild(el('span', 'result-tag', cmdLabel(r)));
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
  const acts = el('div', 'result-actions');
  acts.appendChild(exportBtn('CSV', () => {
    downloadText('query-' + stamp() + '.csv', '\ufeff' + csvOf(r), 'text/csv');
  }));
  acts.appendChild(exportBtn('JSON', () => {
    downloadText('query-' + stamp() + '.json', jsonOf(r), 'application/json');
  }));
  head.appendChild(acts);

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

  card.appendChild(gridBlock(r, idx));
  return card;
}

// gridBlock 渲染结果表格 + 分页条。分页状态按「第几个结果」记在 pageState 里，
// 翻页时整体重渲染（结果集已经在内存里，不重新查库）。
// 结果表里长文本只显示预览。
//
// 大字段（LOB）现在能存 1MB，而单元格原来是 `white-space: pre-wrap` 全文展开：
// 实测 1200 字就能把一行撑到 411px，1MB 的值会把页面卡死（整段文本进 DOM + 反复排版）。
// 所以单元格里只放前 CELL_PREVIEW_CHARS 个字符（CSS 再夹三行），点开才看全文；
// 查看器自己也有渲染上限，但「复制全文」始终是完整内容。
const CELL_PREVIEW_CHARS = 160;
const CELL_VIEW_CHARS = 200000;

function cellIsLong(s) {
  if (s.length > CELL_PREVIEW_CHARS) return true;
  // 短字符串里换行太多也会把行撑高（≤160 字符，数一遍很便宜）
  let nl = 0;
  for (let i = 0; i < s.length; i++) {
    if (s.charCodeAt(i) === 10 && ++nl > 2) return true;
  }
  return false;
}

function openCellViewer(colName, text) {
  const modal = document.getElementById('cellModal');
  document.getElementById('cellTitle').textContent = colName ? colName + ' · 全文' : '单元格内容';
  const pre = document.getElementById('cellText');
  const cut = text.length > CELL_VIEW_CHARS;
  pre.textContent = cut ? text.slice(0, CELL_VIEW_CHARS) : text;
  pre.scrollTop = 0;
  const note = document.getElementById('cellNote');
  note.classList.toggle('hidden', !cut);
  note.textContent = cut
    ? `内容太长，这里只渲染前 ${CELL_VIEW_CHARS.toLocaleString()} 字符（共 ${text.length.toLocaleString()} 字符）；要完整内容点「复制全文」。`
    : '';
  const lines = text.split('\n').length;
  document.getElementById('cellMeta').textContent =
    `${text.length.toLocaleString()} 字符 · ${lines.toLocaleString()} 行`;
  document.getElementById('cellCopy').onclick = () => {
    copyText(text).then(ok => {
      const btn = document.getElementById('cellCopy');
      btn.textContent = ok ? '已复制' : '复制失败';
      setTimeout(() => { btn.textContent = '复制全文'; }, 1200);
    });
  };
  modal.classList.remove('hidden');
}

function closeCellViewer() {
  document.getElementById('cellModal').classList.add('hidden');
  // 正文可能有 20 万字符，关掉就放掉，别留着占内存
  document.getElementById('cellText').textContent = '';
}

// copyText 优先走剪贴板 API；非 https / 无权限时退回 textarea + execCommand。
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext !== false) {
    return navigator.clipboard.writeText(text).then(() => true, () => legacyCopy(text));
  }
  return Promise.resolve(legacyCopy(text));
}

function legacyCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
  ta.remove();
  return ok;
}

// sortOrder 返回「第 i 行实际该显示 rows 里的哪一行」的下标表；不排序时返回 null。
//
// 排序在浏览器里对已拿到的行做（服务端已经按 LIMIT 返回），所以它是「看这份结果」
// 的视图操作，不会重新执行 SQL。相等值保持原顺序（下标兜底），不会来回跳。
function sortOrder(rows, st) {
  if (st.sortCol < 0) return null;
  const ci = st.sortCol;
  const dir = st.sortDir;
  const idx = Array.from({ length: rows.length }, (_, i) => i);
  idx.sort((a, b) => {
    const r = cmpCell(rows[a][ci], rows[b][ci]);
    return r !== 0 ? r * dir : a - b;
  });
  return idx;
}

// cmpCell 比较两个单元格的值。
// NULL 永远排最后（不管正序倒序）—— 「空值挡在最前面」比空值散在中间更难扫。
function cmpCell(a, b) {
  const an = a === null || a === undefined;
  const bn = b === null || b === undefined;
  if (an || bn) return an && bn ? 0 : an ? 1 : -1;
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  if (typeof a === 'boolean' && typeof b === 'boolean') return (a ? 1 : 0) - (b ? 1 : 0);
  // numeric: true 让 "v2" 排在 "v10" 前面（编号列几乎是这种形态）
  return String(a).localeCompare(String(b), 'zh-Hans-CN', { numeric: true });
}

// colGrip 是表头右边缘那条拖拽手柄：拖动改列宽，双击恢复自适应。
function colGrip(th, ci, st) {
  const g = el('span', 'col-grip');
  g.addEventListener('pointerdown', ev => {
    ev.preventDefault();
    ev.stopPropagation();
    const startX = ev.clientX;
    const startW = th.getBoundingClientRect().width;
    try { g.setPointerCapture(ev.pointerId); } catch (e) {}
    const move = e => {
      th.style.minWidth = Math.max(48, Math.round(startW + (e.clientX - startX))) + 'px';
    };
    const up = () => {
      st.widths = st.widths || {};
      st.widths[ci] = Math.max(48, Math.round(th.getBoundingClientRect().width));
      g.removeEventListener('pointermove', move);
      g.removeEventListener('pointerup', up);
      g.removeEventListener('pointercancel', up);
    };
    g.addEventListener('pointermove', move);
    g.addEventListener('pointerup', up);
    g.addEventListener('pointercancel', up);
  });
  g.addEventListener('dblclick', ev => {
    ev.stopPropagation();
    th.style.minWidth = '';
    if (st.widths) delete st.widths[ci];
  });
  return g;
}

function gridBlock(r, idx) {
  const box = document.createDocumentFragment();
  const total = r.rows.length;
  let st = pageState.get(idx);
  if (!st) {
    // sortCol < 0 = 按服务端返回的顺序；widths 是拖过列宽之后的固定值
    st = { page: 0, size: 100, sortCol: -1, sortDir: 1, widths: {} };
    pageState.set(idx, st);
  }
  const pages = Math.max(1, Math.ceil(total / st.size));
  if (st.page > pages - 1) st.page = pages - 1;
  const from = st.page * st.size;
  const to = Math.min(from + st.size, total);

  const cols = r.columns || [];
  // 结果换了（重新执行、切库）之后列数可能变少，旧的下标就失效了
  if (st.sortCol >= cols.length) st.sortCol = -1;
  const order = sortOrder(r.rows, st);

  const wrap = el('div', 'grid-wrap');
  const table = el('table', 'grid');
  const thead = el('thead');
  const tr = el('tr');
  for (let ci = 0; ci < cols.length; ci++) {
    const c = cols[ci];
    const th = el('th');
    th.appendChild(el('span', 'cname', c.name));
    if (c.type) th.appendChild(el('span', 'ctype', c.type));
    if (st.sortCol === ci) {
      th.appendChild(el('span', 'sort-ind', st.sortDir > 0 ? '▲' : '▼'));
    }
    th.title = '点一下按这一列排序；拖右边缘调列宽（双击恢复）';
    if (st.widths && st.widths[ci]) th.style.minWidth = st.widths[ci] + 'px';
    th.addEventListener('click', ev => {
      if (ev.target.closest('.col-grip')) return; // 拖列宽不是点排序
      if (st.sortCol === ci) {
        st.sortDir = -st.sortDir;
      } else {
        st.sortCol = ci;
        st.sortDir = 1;
      }
      st.page = 0;
      renderResults(lastData, true);
    });
    th.appendChild(colGrip(th, ci, st));
    tr.appendChild(th);
  }
  thead.appendChild(tr);
  table.appendChild(thead);

  const tbody = el('tbody');
  for (let i = from; i < to; i++) {
    const row = r.rows[order ? order[i] : i];
    const tr2 = el('tr');
    for (let j = 0; j < row.length; j++) {
      const v = row[j];
      let td;
      if (v === null || v === undefined) {
        td = el('td', 'null', 'NULL');
      } else if (typeof v === 'number') {
        td = el('td', 'num', String(v));
      } else if (typeof v === 'boolean') {
        td = el('td', 'bool', v ? 'true' : 'false');
      } else {
        const s = String(v);
        if (cellIsLong(s)) {
          // 长文本：只放预览（多一行 div 是为了 CSS 夹行，td 上没法可靠夹高度）
          td = el('td', 'long');
          td.appendChild(el('div', 'cell-clip', s.slice(0, CELL_PREVIEW_CHARS)));
          td.title = `点击看全文（${s.length.toLocaleString()} 字符）`;
          td.addEventListener('click', () => openCellViewer(cols[j] && cols[j].name, s));
        } else {
          td = el('td', '', s);
        }
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
      renderResults(lastData, true);
    });
    foot.appendChild(sizeSel);
    foot.appendChild(el('span', 'grid-range', `${st.page + 1}/${pages} 页`));

    const prev = el('button', 'pg-btn', '‹ 上一页');
    prev.disabled = st.page <= 0;
    prev.addEventListener('click', () => {
      if (st.page > 0) { st.page--; renderResults(lastData, true); }
    });
    foot.appendChild(prev);

    const next = el('button', 'pg-btn', '下一页 ›');
    next.disabled = st.page >= pages - 1;
    next.addEventListener('click', () => {
      if (st.page < pages - 1) { st.page++; renderResults(lastData, true); }
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
  let title = err.title || '执行失败';
  if (!err.title && err.kind === 'lock_timeout') title = '锁冲突 · 等待超时';
  if (!err.title && err.kind === 'deadlock') title = '检测到死锁 · 已回滚';
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

// schemaQuery 是侧栏的过滤词（小写）。表一多的时候纯靠滚找表太慢，
// 这里按表名做子串匹配，匹配不到给一句明确的空态，而不是「暂无表」那种误导。
let schemaQuery = '';
// 最近一次拿到的完整表列表：过滤只改显示，不清数据源（刷新时还要用它重画）
let lastSchemaTables = [];

function renderSchema(tables) {
  treeEl.innerHTML = '';
  lastSchemaTables = tables;
  if (!tables.length) {
    treeEl.appendChild(el('div', 'empty-hint', '暂无表，试试「载入示例数据」'));
    return;
  }
  const q = schemaQuery.trim().toLowerCase();
  const shown = q ? tables.filter(t => t.name.toLowerCase().includes(q)) : tables;
  if (!shown.length) {
    treeEl.appendChild(el('div', 'empty-hint', `没有匹配「${schemaQuery.trim()}」的表`));
    return;
  }
  for (const t of shown) {
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

/* ---------------- 监控面板 ---------------- */

// 数据全部来自 /api/monitor（一次请求拿齐四块），轮询 2 秒。
// 设计原则对应后端：每个数字都能连到一个动作（Kill / EXPLAIN / 开超时），
// 连不上的就不放。

const monPanel = document.getElementById('monPanel');
const monEls = {
  water: document.getElementById('monWater'),
  layers: document.getElementById('monLayers'),
  lockBody: document.getElementById('monLockBody'),
  lockOk: document.getElementById('monLockOk'),
  sqlOk: document.getElementById('monSqlOk'),
  txOk: document.getElementById('monTxOk'),
  diagSub: document.getElementById('monDiagSub'),
  sort: document.getElementById('monSort'),
  tabs: document.getElementById('monTabs'),
  tabN: {
    lock: document.getElementById('monTabLockN'),
    sql: document.getElementById('monTabSqlN'),
    tx: document.getElementById('monTabTxN'),
  },
  nav: document.getElementById('monNav'),
  body: document.getElementById('monBody'),
  ioSum: document.getElementById('monIOSum'),
  spaceSub: document.getElementById('spaceSub'),
  mapBack: document.getElementById('mapBack'),
  mapCrumbs: document.getElementById('mapCrumbs'),
  mapGroup: document.getElementById('mapGroup'),
  mapJump: document.getElementById('mapJump'),
  mapRefresh: document.getElementById('mapRefresh'),
  mapFiles: document.getElementById('mapFiles'),
  mapBody: document.getElementById('mapBody'),
  mapCanvas: document.getElementById('mapCanvas'),
  mapTree: document.getElementById('mapTree'),
  mapRail: document.getElementById('mapRail'),
  mapGrip: document.getElementById('mapGrip'),
  mapNote: document.getElementById('mapNote'),
  mapZoom: document.getElementById('mapZoom'),
  mapZoomVal: document.getElementById('mapZoomVal'),
  mapZoomIn: document.getElementById('mapZoomIn'),
  mapZoomOut: document.getElementById('mapZoomOut'),
  mapZoomReset: document.getElementById('mapZoomReset'),
  pmGrid: document.getElementById('pmGrid'),
  pmLegend: document.getElementById('pmLegend'),
  pmSide: document.getElementById('pmSide'),
  pmMore: document.getElementById('pmMore'),
  spaceBar: document.getElementById('spaceBar'),
  spaceLegend: document.getElementById('spaceLegend'),
  spaceBody: document.getElementById('spaceBody'),
  spaceDetail: document.getElementById('spaceDetail'),
  spaceOk: document.getElementById('spaceOk'),
  setPop: document.getElementById('monSetPop'),
  setBtn: document.getElementById('btnMonSettings'),
  rates: document.getElementById('monRate'),
  proc: document.getElementById('monProc'),
  io: document.getElementById('monIO'),
  slowSql: document.getElementById('monSlowSQL'),
  slowTx: document.getElementById('monSlowTx'),
  pager: {
    lock: document.getElementById('monLockPager'),
    sql: document.getElementById('monSqlPager'),
    tx: document.getElementById('monTxPager'),
  },
  timeouts: document.getElementById('monTimeouts'),
  timeoutHint: document.getElementById('monTimeoutHint'),
  db: document.getElementById('monDB'),
  updated: document.getElementById('monUpdated'),
  dot: document.getElementById('monDot'),
  badge: document.getElementById('monBadge'),
  btn: document.getElementById('btnMonitor'),
};

let monOpen = false;
let monData = null;
let monExpanded = {}; // key = 表名|会话 → 展开详情
let monTimeoutBuilt = false;
let monSavingTimeouts = false;

// 诊断区的 Tab。'' 表示「还没选过」——第一次打开时自动落到有内容的那个，
// 之后尊重用户的选择（每 2 秒轮询不能把人家正在看的 Tab 抢走）。
let monTab = '';
let monTabAuto = true;

// 顺序 = 页面上的顺序。锁明细放最后：它是「出事才看」的，平时先看慢SQL/慢事务。
const MON_TABS = ['sql', 'tx', 'lock'];

// 慢SQL/慢事务的排序口径。两个榜单后端都算好了（slowSql / slowSqlMax），
// 切换时不发请求、不等下一轮轮询。
//   total = 累计耗时：谁最费这台机器（高频小语句会冒头）
//   max   = 单次最慢：谁偶发卡了一下（偶发慢查询会冒头）
// 两种都要，因为它们是两个不同的问题：前者要优化，后者要排查。
let monSortBy = 'total';

function monStmtRows(m) {
  return monSortBy === 'max' ? (m.slowSqlMax || []) : (m.slowSql || []);
}
function monTxRows(m) {
  return monSortBy === 'max' ? (m.slowTxMax || []) : (m.slowTx || []);
}

// 锁明细没有「累计/单次」之分，切到锁 Tab 时把切换器藏起来。
function syncSortToggle() {
  if (!monEls.sort) return;
  monEls.sort.classList.toggle('hidden', monTab === 'lock' || monTab === 'space');
  for (const b of monEls.sort.querySelectorAll('button')) {
    b.classList.toggle('on', b.dataset.sort === monSortBy);
  }
}

if (monEls.sort) {
  monEls.sort.addEventListener('click', e => {
    const b = e.target.closest('button[data-sort]');
    if (!b) return;
    monSortBy = b.dataset.sort;
    syncSortToggle();
    if (monData) {
      monPage.sql = 0;
      monPage.tx = 0;
      renderSlowSQL(monData);
      renderSlowTx(monData);
      updateDiagSub();
    }
  });
}

function toggleMonitor(force) {
  monOpen = force === undefined ? !monOpen : force;
  monPanel.classList.toggle('hidden', !monOpen);
  document.body.classList.toggle('mon-open', monOpen);
  if (monOpen) pollMonitor();
  else setMonSettings(false);
}

/* ---- 页面级 Tab：概览 / 存储 / 空间地图 / 诊断 ----
   按「回答什么问题」分：概览看健康与走势，存储看数字（够不够、该不该整理），
   空间地图看「东西在哪、长什么样」，诊断看谁在捣乱。
   诊断卡内部还有一层小 Tab（慢SQL/慢事务/锁），那是同一个问题的三种切面。 */
const MON_PAGES = ['overview', 'storage', 'map', 'diag'];
let monPageTab = 'overview';

function setMonPage(name) {
  if (MON_PAGES.indexOf(name) < 0) name = MON_PAGES[0];
  monPageTab = name;
  if (monEls.nav) {
    for (const b of monEls.nav.querySelectorAll('.mon-nav-tab')) {
      const on = b.dataset.page === name;
      b.classList.toggle('on', on);
      b.setAttribute('aria-selected', on ? 'true' : 'false');
    }
  }
  for (const p of monPanel.querySelectorAll('.mon-page')) {
    p.classList.toggle('hidden', p.dataset.page !== name);
  }
  // 空间地图是「画布吃满剩余高度」的：这一页不整页滚，否则画布高度算不出来。
  // 其余几页该多高就多高，靠 .mon-body 滚。
  if (monEls.body) {
    monEls.body.scrollTop = 0;
    monEls.body.classList.toggle('map-mode', name === 'map');
  }
  // 空间是「点开才算」的：要 stat 每张表，下钻还要逐页读，不跟着轮询跑。
  if (name === 'storage' && !spaceData) loadSpace();
  if (name === 'map') {
    applyRailWidth();
    if (!spaceData) loadSpace();
    else renderMap();
  }
}

if (monEls.nav) {
  monEls.nav.addEventListener('click', e => {
    const b = e.target.closest('.mon-nav-tab');
    if (b) setMonPage(b.dataset.page);
  });
}

const btnGoStorage = document.getElementById('btnGoStorage');
if (btnGoStorage) btnGoStorage.addEventListener('click', () => setMonPage('storage'));

// 把诊断区切到某个 Tab。manual=true 表示用户点的，记下来不再自动改。
function setMonTab(name, manual) {
  if (MON_TABS.indexOf(name) < 0) name = MON_TABS[0];
  if (manual) monTabAuto = false;
  monTab = name;
  for (const b of monEls.tabs.querySelectorAll('.mon-tab')) {
    const on = b.dataset.tab === name;
    b.classList.toggle('active', on);
    b.setAttribute('aria-selected', on ? 'true' : 'false');
  }
  for (const p of monPanel.querySelectorAll('.mon-panel')) {
    p.classList.toggle('hidden', p.dataset.panel !== name);
  }
  syncSortToggle();
  updateDiagSub();
}

function monCounts() {
  const m = monData || {};
  return {
    lock: (m.locks || []).length,
    // 慢SQL/慢事务给的是「后端跟踪了多少个结构」（stmtLive/txLive），
    // 不是这次返回的行数 —— 榜单只给前 100 条，用行数会写成「跟踪 100 个」。
    sql: m.stmtLive || 0,
    tx: m.txLive || 0,
  };
}

// Tab 右侧的说明文字 + 锁明细上的条数。
// 只有锁明细带数字：「3 把锁」是个量，看得出规模；慢SQL/慢事务是「跟踪了多少
// 个结构」，不是「有几个问题」，数字挂在 Tab 上容易被误读，只写在右侧说明里。
function updateDiagSub() {
  const n = monCounts();
  const waiting = (monData && monData.locks || []).some(r => (r.waiters || []).length);
  const by = monSortBy === 'max' ? '单次最慢' : '累计耗时';
  const byTx = monSortBy === 'max' ? '单次最长' : '累计时长';
  const sub = {
    lock: n.lock ? `${n.lock} 把锁 · 按持有时长降序 · 点行展开事务详情` : '当前没有表锁',
    sql: n.sql ? `跟踪 ${n.sql} 个结构 · 按${by} · 点行看明细` : '还没有执行记录',
    tx: n.tx ? `跟踪 ${n.tx} 个结构 · 按${byTx} · 点行看明细` : '还没有结束的事务',
  };
  for (const k of MON_TABS) {
    const b = monEls.tabN[k];
    if (!b) continue;
    b.textContent = k === 'lock' && n.lock ? String(n.lock) : '';
    b.classList.toggle('alert', k === 'lock' && waiting);
  }
  if (monEls.diagSub) monEls.diagSub.textContent = sub[monTab] || '';
}

// 空表不画表格：一行居中提示，占位高度和一页 10 行差不多（见 .mon-panel 的
// min-height），免得切 Tab 时卡片忽高忽低。
function setPanelEmpty(panel, ok, empty, text) {
  panel.classList.toggle('is-empty', empty);
  ok.classList.toggle('hidden', !empty);
  if (empty && text) ok.textContent = text;
}

/* ---- 诊断区表格分页 ----
   三张表都按 10 行一页：慢SQL 环最多 256 条，全平铺会把面板拉成一屏半，而真正
   要看的永远是前几条。页码按 Tab 各存一份 —— 切走再切回来还在原来那页。 */
const MON_PAGE_SIZE = 10;
const monPage = { sql: 0, tx: 0, lock: 0 };

// 取当前页的行。rows 是后端给的完整列表，这里只切片、不重新请求。
// 页码越界（数据变少 / 翻到最后一页后记录被挤掉）时收到最后一页，不留空页。
function monPageRows(kind, rows) {
  const pages = Math.max(1, Math.ceil(rows.length / MON_PAGE_SIZE));
  if (monPage[kind] > pages - 1) monPage[kind] = pages - 1;
  if (monPage[kind] < 0) monPage[kind] = 0;
  const start = monPage[kind] * MON_PAGE_SIZE;
  return { slice: rows.slice(start, start + MON_PAGE_SIZE), start, pages, total: rows.length };
}

// 分页条。只要有一行数据就显示，一页装得下时两个按钮都禁用 —— 这样「表格区」
// 的高度是恒定的（表头 + 10 行 + 这一条），数据从 3 条变 15 条、切 Tab、翻页
// 都不会让卡片长高或缩水。空态才藏起来（那时整块由 .mon-ok 占位）。
function renderPager(kind, pages, total) {
  const box = monEls.pager[kind];
  if (!box) return;
  box.innerHTML = '';
  if (!total) {
    box.classList.add('hidden');
    return;
  }
  box.classList.remove('hidden');
  box.appendChild(el('span', 'pg-info',
    `第 ${monPage[kind] + 1}/${pages} 页 · 共 ${total} 条`));

  const go = delta => {
    monPage[kind] += delta;
    // 只重画这一张表：翻页跟其它卡片无关，没必要整块重渲染（那还会让
    // 正在展开的锁详情、其它表格的滚动位置一起闪一下）。
    const draw = { lock: renderLocks, sql: renderSlowSQL, tx: renderSlowTx }[kind];
    if (draw && monData) draw(monData);
  };
  const mk = (label, disabled, delta) => {
    const b = el('button', 'pg-btn', label);
    b.type = 'button';
    b.disabled = disabled;
    b.addEventListener('click', () => go(delta));
    box.appendChild(b);
  };
  mk('‹ 上一页', monPage[kind] <= 0, -1);
  mk('下一页 ›', monPage[kind] >= pages - 1, +1);
}

/* 规则配置浮层：超时开关是全局安全配置，不是监控数据，
   常年铺在页面最底部只会让人在告警时往下滚。收进右上角齿轮。 */
function monSettingsOpen() {
  return !!(monEls.setPop && !monEls.setPop.classList.contains('hidden'));
}

function setMonSettings(open) {
  if (!monEls.setPop) return;
  monEls.setPop.classList.toggle('hidden', !open);
  if (monEls.setBtn) monEls.setBtn.classList.toggle('on', open);
  if (open) {
    const first = monEls.timeouts.querySelector('input');
    if (first) first.focus();
  }
}

// 把毫秒渲染成人话。锁明细里的时长是面板的核心指标，不能只显示 0。
function fmtMs(ms) {
  if (ms === undefined || ms === null) return '—';
  if (ms < 0) ms = 0;
  if (ms < 1) return '<1ms';
  if (ms < 1000) return Math.round(ms) + ' ms';
  if (ms < 60000) return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + ' s';
  if (ms < 3600000) return Math.floor(ms / 60000) + 'm' + Math.round((ms % 60000) / 1000) + 's';
  return (ms / 3600000).toFixed(1) + 'h';
}

// 纳秒（后端样本）→ 毫秒文案；小于时钟步进的显示成 "<0.20ms"，不然一片 0.00 很吓人。
function fmtNs(ns) {
  if (!ns || ns <= 0) return '—';
  if (clockGranNs > 0 && ns < clockGranNs) return '<' + (clockGranNs / 1e6).toFixed(2) + 'ms';
  return fmtMs(ns / 1e6);
}

// monPaused：暂停轮询。截图取证、对比两条 SQL 前后的水位时，画面每秒在跳没法看。
// 暂停只停「取数据」，已经画出来的内容原样留着；点一下继续会立刻拉一次。
let monPaused = false;

function setMonPaused(on) {
  monPaused = on;
  const b = document.getElementById('btnMonPause');
  b.textContent = on ? '▶' : 'Ⅱ';
  b.setAttribute('aria-pressed', on ? 'true' : 'false');
  b.title = on ? '继续刷新' : '暂停刷新';
  b.classList.toggle('on', on);
  if (on) {
    monEls.updated.textContent = '已暂停';
  } else {
    pollMonitor();
  }
}

async function pollMonitor() {
  if (monPaused) return;
  let m;
  try {
    m = await api('/api/monitor');
  } catch (e) {
    return; // 轮询失败不打断操作
  }
  if (!m || m.water === undefined) return;
  monData = m;
  renderMonBadge(m);
  if (monOpen) renderMonitor(m);
}

// 顶栏的小圆点 + 待处理计数：面板关着也要能看出「现在不对劲」。
function renderMonBadge(m) {
  const w = m.water || {};
  const lvl = w.level || 'green';
  monEls.dot.className = 'mon-dot ' + lvl;
  syncTopDot();
  const n = (w.wait || 0) + (w.queue || 0);
  if (n > 0) {
    monEls.badge.textContent = String(n);
    monEls.badge.classList.remove('hidden');
  } else {
    monEls.badge.classList.add('hidden');
  }
}

// 圆点 hover 的文字：健康状态 + 事务状态一起给。
// 原来顶栏单独挂一个「空闲」药丸，可「空闲」正是默认状态、说了等于没说；
// 颜色本来就一眼能看出没问题，文字放到 tooltip 里，省下顶栏宽度。
function syncTopDot() {
  if (!monEls.dot) return;
  const lvl = monEls.dot.className.indexOf('yellow') >= 0 ? 'yellow'
    : (monEls.dot.className.indexOf('red') >= 0 ? 'red' : 'green');
  const health = lvl === 'green' ? '健康'
    : (lvl === 'yellow' ? '有排队或槽位已满' : '排队触顶或锁成瓶颈');
  let st = '空闲';
  if (lastTx.active) {
    st = lastTx.aborted ? '事务已中止' : ('事务中 · ' + lastTx.changes + ' 行未提交');
  }
  const tip = health + ' · ' + st;
  monEls.dot.title = tip;
  // 圆点只有 10px，hover 命中区太小；按钮整体也带上同一句，鼠标落在文字上也能看到。
  if (monEls.btn) monEls.btn.title = '监控 · ' + tip;
}

function renderMonitor(m) {
  monEls.db.textContent = m.water.db || '';
  monEls.updated.textContent = new Date(m.nowMs || Date.now()).toLocaleTimeString('zh-CN', { hour12: false });
  renderWater(m.water || {});
  renderLayers(m.layers || {});
  renderRates(m);
  renderProc(m);
  renderIO(m);
  renderIOSummary(m);
  renderLocks(m);
  renderSlowSQL(m);
  renderSlowTx(m);
  renderTimeouts(m.timeouts || {});
  if (!monTab) {
    // 第一次打开：有锁就直接看锁明细（它最先出事），否则落在第一个 Tab。
    const n = monCounts();
    setMonTab(n.lock ? 'lock' : 'sql', false);
  } else {
    updateDiagSub();
  }
}

/* ---- 1. 水位条 ---- */

function renderWater(w) {
  monEls.water.innerHTML = '';
  const lvl = w.level || 'green';
  const execMax = w.execMax > 0 ? String(w.execMax) : '不限';
  const queueMax = w.queueMax > 0 ? String(w.queueMax) : '不限';

  const item = (key, val, max, tip, cls) => {
    const box = el('div', 'w-item' + (cls ? ' ' + cls : ''));
    box.title = tip || '';
    box.appendChild(el('span', 'w-k', key));
    box.appendChild(el('b', 'w-v', val));
    if (max !== undefined) box.appendChild(el('span', 'w-max', '/' + max));
    return box;
  };

  // 从外到内排：排队（等槽位）→ 等锁（拿到槽位但卡在锁）→ 执行（真正在跑）。
  // 从左往右读正好是「一个请求怎么才能跑起来」的顺序。
  monEls.water.appendChild(item('排队', w.queue, queueMax,
    '等执行槽位的请求数；触顶会被快速拒绝（背压）',
    w.queueMax > 0 && w.queue >= w.queueMax ? 'bad' : (w.queue > 0 ? 'warn' : '')));
  monEls.water.appendChild(item('等锁', w.wait, undefined,
    '正在等表锁的事务数；多于执行数说明瓶颈在锁，不在算力',
    w.exec > 0 && w.wait > w.exec ? 'bad' : ''));
  monEls.water.appendChild(item('执行', w.exec, execMax,
    '并发执行槽位：事务持有（空闲事务也占位），不是「正在跑 SQL 的条数」'));

  const lamp = el('div', 'w-lamp');
  lamp.appendChild(el('span', 'mon-dot ' + lvl));
  lamp.appendChild(el('span', 'w-lamp-text',
    lvl === 'green' ? '健康' : (lvl === 'yellow' ? '有压力' : '需处理')));
  monEls.water.appendChild(lamp);

  const c = monData && monData.counters ? monData.counters : {};
  const note = el('div', 'w-note');
  note.title = '自启动以来的累计次数';
  note.textContent = `Kill ${c.kills || 0} · 慢SQL回滚 ${c.slowSqlAborts || 0} · 慢事务回滚 ${c.slowTxAborts || 0}`;
  monEls.water.appendChild(note);
}

/* ---- 2. 三层状态 ---- */

function renderLayers(layers) {
  monEls.layers.innerHTML = '';
  // 顺序与水位条一致：排队 → 等锁 → 执行，眼睛不用在两处来回换算位置。
  const cards = [
    { key: 'queued', name: '排队', lv: 'lv-queue', tip: '等执行槽位的请求' },
    { key: 'waiting', name: '等锁', lv: 'lv-wait', tip: '卡在表锁上的会话' },
    { key: 'running', name: '执行', lv: 'lv-run', tip: '正在跑语句的会话' },
  ];
  for (const c of cards) {
    const d = layers[c.key] || {};
    const box = el('div', 'mon-layer ' + c.lv);
    box.title = c.tip;

    const head = el('div', 'lv-head');
    head.appendChild(el('span', 'lv-name', c.name));
    head.appendChild(el('b', 'lv-count', String(d.count || 0)));
    box.appendChild(head);

    const stat = el('div', 'lv-stat');
    stat.appendChild(el('span', '', 'p50 ' + fmtNs(d.p50Ns)));
    stat.appendChild(el('span', '', 'p95 ' + fmtNs(d.p95Ns)));
    stat.appendChild(el('span', '', 'max ' + fmtNs(d.maxNs)));
    box.appendChild(stat);

    const lg = d.longest;
    const lastRow = el('div', 'lv-longest');
    if (lg) {
      lastRow.appendChild(el('span', 'lv-sess', sessionShort(lg.session)));
      lastRow.appendChild(el('span', 'lv-held', '已 ' + fmtMs(lg.stateMs)));
      const what = (lg.tables && lg.tables.length ? lg.tables.join(',') + ' · ' : '') + (lg.sql || '');
      const prev = el('span', 'lv-sql', what || '—');
      prev.title = lg.sql || '';
      lastRow.appendChild(prev);
    } else {
      lastRow.appendChild(el('span', 'lv-none', '没有'));
    }
    box.appendChild(lastRow);
    monEls.layers.appendChild(box);
  }
}

/* ---- 3. 吞吐与连接：QPS / TPS / 连接数 ---- */

// 三个数回答「库有多忙」，和上面三层的「卡在哪」是不同的问题：
// 三层是瞬时的、告诉你现在哪里堵着；这里是走势、告诉你刚才忙成什么样。
// 排障时最有用的组合是「等锁数一直不为 0 + QPS 掉到地板」——
// 那不是没人来，是全被锁住了。
// 吞吐/资源曲线的时间窗口（秒）。只影响「画多久」，采样节奏与 5 秒均值口径都不变。
// 服务端最多留 5 分钟，所以不给 15 分钟这种画出来一半是空的选项。
const MON_WIN_KEY = 'godb.monWin';
let monWinSec = (() => {
  const n = parseInt(localStorage.getItem(MON_WIN_KEY) || '', 10);
  return n === 60 || n === 300 ? n : 300;
})();

// winSlice 按 atMs 截出窗口内的点；点太少（刚启动）就原样返回，别画出空图。
function winSlice(pts) {
  if (!pts || pts.length < 2) return pts || [];
  const cut = Date.now() - monWinSec * 1000;
  const out = pts.filter(pt => (pt.atMs || 0) >= cut);
  return out.length >= 2 ? out : pts.slice(-2);
}

function winLabel() {
  return monWinSec === 60 ? '1 分钟' : '5 分钟';
}

function syncWinHint() {
  const a = document.getElementById('monRateSub');
  if (a) a.textContent = `每秒采样 · 最近 ${winLabel()} · 当前值为 5 秒平均`;
  const b = document.getElementById('monProcSub');
  if (b) b.textContent = `每秒采样 · 最近 ${winLabel()}`;
  const box = document.getElementById('monWin');
  if (box) {
    for (const btn of box.querySelectorAll('button')) {
      const on = String(monWinSec) === btn.dataset.win;
      btn.classList.toggle('on', on);
      btn.setAttribute('aria-pressed', on ? 'true' : 'false');
    }
  }
}

document.getElementById('monWin').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-win]');
  if (!btn) return;
  const n = parseInt(btn.dataset.win, 10);
  if (n === monWinSec) return;
  monWinSec = n;
  try { localStorage.setItem(MON_WIN_KEY, String(n)); } catch (e) {}
  syncWinHint();
  if (monData) renderMonitor(monData);
});

function renderRates(m) {
  syncWinHint(); // 文案与按钮激活态跟着窗口走（每秒一次的几个 DOM 查询，可以忽略）
  const r = m.rates || {};
  const box = monEls.rates;
  if (!box) return;
  box.innerHTML = '';
  const pts = winSlice(r.series || []);

  const grid = el('div', 'proc-grid');
  grid.appendChild(statCell(
    'QPS',
    r.sampled ? String(r.qps) : '—',
    `峰值 ${r.peakQps || 0} · 语句/秒`,
    '每秒成功执行的语句数，不含 BEGIN/COMMIT —— 和会话详情里的「语句数」同口径'));
  grid.appendChild(statCell(
    'TPS',
    r.sampled ? String(r.tps) : '—',
    `峰值 ${r.peakTps || 0} · 事务/秒`,
    '每秒结束的事务数（提交 + 回滚）。QPS 高 TPS 低 = 每条语句一个自动提交事务；QPS 低 TPS 高 = 空事务多'));
  grid.appendChild(statCell(
    '连接',
    String(r.conns || 0),
    `峰值 ${r.peakConns || 0} · 会话数`,
    '当前会话数（控制台 HTTP + PG 连接）。只涨不降要怀疑连接泄漏'));
  box.appendChild(grid);

  if (pts.length < 2) {
    box.appendChild(el('div', 'mon-none', '曲线需要至少两个采样点（一秒一个）。'));
    return;
  }

  box.appendChild(sparkLines(pts, [
    { get: pt => pt.qps, color: 'var(--accent)', width: 1.4 },
    // TPS 用虚线：自动提交时 QPS 和 TPS 完全相等，两条实线会重叠成一条，
    // 看着像「TPS 没画出来」。虚线叠在实线上，两条都在。
    { get: pt => pt.tps, color: 'var(--ok)', width: 1.2, opacity: 0.9, dash: '5 3' },
  ], {
    mode: 'zero',
    legend: '<span class="spark-row"><i class="spark-k cpu"></i>QPS<i class="spark-k mem"></i>TPS（虚线）</span>',
    tip: '两条线共用纵轴：都是「每秒次数」，量级天生可比。QPS 远高于 TPS 说明基本是自动提交',
  }));

  box.appendChild(sparkLines(pts, [
    { get: pt => pt.conns, color: 'var(--warn)', width: 1.2 },
  ], {
    mode: 'range',
    legend: '<span class="spark-row"><i class="spark-k conn"></i>连接数</span>',
    tip: '纵轴取这段区间的上下界：连接数平时几乎不动，从 0 起画就只是一条贴顶直线',
  }));
}

// 折线图。两条归一化策略：
//   mode='zero'  —— 从 0 起，上限按峰值自适应。适合「0 本身有意义」的量（QPS/TPS、CPU 核数）。
//   mode='range' —— 取区间上下界。适合「平时几乎不动」的量（连接数、堆大小），
//                   否则一条贴顶直线，涨 20% 都看不出来。
// 波动小于 2% 判为持平，把上下界人为撑开，避免把噪声画成锯齿。
function sparkLines(points, lines, opt) {
  const H = (opt && opt.height) || 56;
  const W = 640, padT = 8, padB = 6;
  const n = points.length;
  let lo = 0, hi = 1;

  if (opt.mode === 'zero') {
    let peak = 0;
    for (const l of lines) for (const pt of points) peak = Math.max(peak, l.get(pt) || 0);
    hi = Math.max(1, Math.ceil(peak * 1.15));
  } else {
    let vlo = Infinity, vhi = 0;
    for (const pt of points) {
      const v = lines[0].get(pt) || 0;
      if (v < vlo) vlo = v;
      if (v > vhi) vhi = v;
    }
    if (!isFinite(vlo)) vlo = 0;
    if (vhi - vlo <= Math.max(1, vhi * 0.02)) {
      lo = Math.max(0, vhi - 1);
      hi = vhi + 1;
    } else {
      const pad = (vhi - vlo) * 0.15;
      lo = Math.max(0, vlo - pad);
      hi = vhi + pad;
    }
  }

  const X = i => (n === 1 ? 0 : (i / (n - 1)) * W);
  const Y = v => H - padB - Math.min(1, Math.max(0, (v - lo) / (hi - lo || 1))) * (H - padT - padB);

  let paths = '';
  for (const l of lines) {
    let d = '';
    for (let i = 0; i < n; i++) {
      d += (i ? 'L' : 'M') + X(i).toFixed(1) + ' ' + Y(l.get(points[i]) || 0).toFixed(1);
    }
    paths += `<path d="${d}" fill="none" stroke="${l.color}" stroke-width="${l.width || 1.3}"` +
      ` stroke-opacity="${l.opacity || 1}" stroke-linejoin="round" stroke-linecap="round"` +
      (l.dash ? ` stroke-dasharray="${l.dash}"` : '') +
      ` vector-effect="non-scaling-stroke"/>`;
  }

  const wrap = el('div', 'mon-spark');
  if (opt.tip) wrap.title = opt.tip;
  wrap.innerHTML =
    `<svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" aria-hidden="true">${paths}</svg>` +
    `<span class="spark-tip">${opt.legend || ''}</span>`;
  return wrap;
}

/* ---- 4. 资源：进程自身 + 存储 IO ---- */

function fmtBytes(n) {
  if (!n || n < 0) return '0 B';
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(n < 10240 ? 1 : 0) + ' KB';
  if (n < 1073741824) return (n / 1048576).toFixed(n < 10485760 ? 1 : 0) + ' MB';
  return (n / 1073741824).toFixed(2) + ' GB';
}

// 一格大数字：主值 + 一行说明。三格并排比一列长文本好扫。
function statCell(name, value, sub, tip) {
  const box = el('div', 'proc-cell');
  if (tip) box.title = tip;
  box.appendChild(el('div', 'pc-name', name));
  box.appendChild(el('div', 'pc-value', value));
  if (sub) box.appendChild(el('div', 'pc-sub', sub));
  return box;
}

function renderProc(m) {
  const p = m.proc || {};
  const box = monEls.proc;
  box.innerHTML = '';
  if (!p.numCpu) {
    box.appendChild(el('div', 'mon-none', '这个平台没有采到进程数据。'));
    return;
  }

  const grid = el('div', 'proc-grid');
  grid.appendChild(statCell(
    'CPU',
    p.sampled ? p.cpuCore.toFixed(2) + ' 核' : '—',
    p.sampled
      ? `用户 ${p.cpuUserCore.toFixed(2)} · 内核 ${p.cpuKernelCore.toFixed(2)}`
      : '刚启动，一秒后才有值',
    p.sampled
      ? `占 ${p.numCpu} 核的 ${p.cpuPercent.toFixed(1)}%。用户态高说明在算 SQL，内核态高说明在等 IO 或系统调用`
      : ''));

  grid.appendChild(statCell(
    '内存',
    fmtBytes(p.heapObjects),
    `进程 ${fmtBytes(p.totalBytes)} · 已还 ${fmtBytes(p.released)}`,
    '主值是活对象（真正在存数据）。进程值含预留与尚未归还的部分；下次 GC 目标 ' + fmtBytes(p.gcGoal)));

  grid.appendChild(statCell(
    '协程',
    String(p.goroutines || 0),
    `GC ${p.gcCycles || 0} 次 · 暂停 p95 ${fmtNs(p.gcPauseP95Ns)}`,
    'goroutine 数持续上涨要怀疑连接泄漏；GC 暂停 p95 是「查询莫名多花几十毫秒」的常见来源'));
  box.appendChild(grid);

  const series = winSlice(p.series || []);
  if (series.length > 1) {
    box.appendChild(sparkline(series, p.numCpu));
  } else {
    box.appendChild(el('div', 'mon-none', '曲线需要至少两个采样点。'));
  }
}

// CPU + 内存曲线。两条线各自归一化，绝不共用一个纵轴：
// 「几核」和「几 MB」量级差好几个数量级，共轴的话内存会把 CPU 压成一条贴底直线。
// CPU 轴从 0 起（0 核是有意义的），按峰值自适应（下限 0.5 核）：固定用核数当上限的话，
// 48 核机器上 0.1 核的曲线会完全贴底，什么形状都看不出；上限写在提示文字里，尺度不丢。
// 内存轴取「这段区间的上下界」而不是从 0 起：堆基本是平的，从 0 起就只有一条贴顶的线，
// 泄漏那种缓慢爬升反而看不出来。
function sparkline(points, maxCore) {
  const W = 640, H = 76, padT = 8, padB = 6;
  let peak = 0, memLo = Infinity, memHi = 0;
  for (const pt of points) {
    peak = Math.max(peak, pt.cpuCore || 0);
    const h = pt.heapBytes || 0;
    memLo = Math.min(memLo, h);
    memHi = Math.max(memHi, h);
  }
  if (!isFinite(memLo)) memLo = 0;
  const max = Math.max(0.5, Math.ceil(peak * 1.15 * 10) / 10);
  // 波动小于 2% 就当平的：不然噪声被放大成锯齿，看着像内存过山车
  const flat = memHi - memLo < Math.max(memHi * 0.02, 64 * 1024);
  const memSpan = flat ? 0 : (memHi - memLo);
  const n = points.length;
  const x = i => (n === 1 ? 0 : (i / (n - 1)) * W);
  const inner = H - padT - padB;
  const y = v => H - padB - Math.min(1, Math.max(0, v / max)) * inner;
  const yMem = v => flat || !memSpan
    ? padT + inner / 2
    : H - padB - ((v - memLo) / memSpan) * inner;

  let dCpu = '', dMem = '';
  for (let i = 0; i < n; i++) {
    dCpu += (i ? 'L' : 'M') + x(i).toFixed(1) + ' ' + y(points[i].cpuCore || 0).toFixed(1);
    dMem += (i ? 'L' : 'M') + x(i).toFixed(1) + ' ' + yMem(points[i].heapBytes || 0).toFixed(1);
  }
  const last = points[n - 1] || {};
  const cpuNow = last.cpuCore || 0;
  const memNow = last.heapBytes || 0;

  const wrap = el('div', 'proc-spark');
  wrap.title = `最近 ${n} 个秒级采样点（约 ${n} 秒）。CPU 纵轴 0~${max} 核；`
    + `内存纵轴 ${flat ? '持平' : fmtBytes(memLo) + '~' + fmtBytes(memHi)}。`
    + '两条线各自归一化，只比形状不比高低。';
  wrap.innerHTML =
    `<svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" aria-hidden="true">` +
    `<line x1="0" y1="${y(max).toFixed(1)}" x2="${W}" y2="${y(max).toFixed(1)}" ` +
    `stroke="var(--border-soft)" stroke-width="0.5" stroke-dasharray="3 3"/>` +
    `<path d="${dMem}" fill="none" stroke="var(--ok)" stroke-opacity="0.7" stroke-width="1" ` +
    `stroke-linejoin="round" stroke-dasharray="${flat ? '2 3' : '0'}" ` +
    `vector-effect="non-scaling-stroke"/>` +
    `<path d="${dCpu}" fill="none" stroke="var(--accent)" stroke-width="1.4" ` +
    `stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/>` +
    `</svg>` +
    `<span class="spark-tip">` +
    `<span class="spark-row"><i class="spark-k cpu"></i>CPU 峰值 ${peak.toFixed(2)} · 现在 ${cpuNow.toFixed(2)} 核` +
    `（纵轴 0~${max}）</span>` +
    `<span class="spark-row"><i class="spark-k mem"></i>内存 峰值 ${fmtBytes(memHi)} · 现在 ${fmtBytes(memNow)}` +
    (flat ? '（波动小于 2%，按持平画）' : `（纵轴 ${fmtBytes(memLo)}~${fmtBytes(memHi)}）`) +
    `</span></span>`;
  return wrap;
}

const CP_REASON_TEXT = {
  commits: '提交次数攒够',
  seconds: '定时到点',
  walMB: 'WAL 达阈值',
  manual: '手动 CHECKPOINT',
  flush: '手动落盘 / 退出前',
  ddl: 'DDL 强制',
};

// 一行「标签 + 值 + 说明」：存储卡与概览里的存储摘要共用，
// 两处口径必须一样，抄一份迟早会走偏。
function ioRow(name, main, sub, tip, cls) {
  const r = el('div', 'io-row' + (cls ? ' ' + cls : ''));
  if (tip) r.title = tip;
  r.appendChild(el('span', 'io-k', name));
  r.appendChild(el('span', 'io-v', main));
  if (sub) r.appendChild(el('span', 'io-sub', sub));
  return r;
}

function renderIO(m) {
  const io = m.io || {};
  const f = io.fsync || {};
  const cp = io.checkpoint || {};
  const rt = io.rates || {};
  const box = monEls.io;
  box.innerHTML = '';
  const row = ioRow;

  // p95 超过 10ms 就算值得注意：这已经超过一次内存读的三个数量级
  const fsyncHot = (f.p95Ns || 0) >= 10e6;
  box.appendChild(row('fsync',
    f.runs ? `${fmtNs(f.p50Ns)} / ${fmtNs(f.p95Ns)} / ${fmtNs(f.maxNs)}` : '—',
    f.runs
      ? `p50 / p95 / max · ${rt.fsyncPerSec || 0} 次/s 服务 ${rt.commitsPerSec || 0} 提交/s（5 秒均值）`
      : '还没有写过盘',
    'fsync 延迟就是提交的实际等待。组提交让 fsync 次数少于提交次数，两个数一比就是合并效果',
    fsyncHot ? 'bad' : ''));

  box.appendChild(row('WAL',
    fmtBytes(f.liveBytes),
    `累计写 ${fmtBytes(f.bytes)} · ${fmtBytes(rt.walBytesPerSec || 0)}/s`,
    'WAL 涨到策略阈值会触发一次快照（默认 64 MB）'));

  // 用 lastReason 判断「有没有真正写过」：lastNs=0 表示启动时空库那一轮，
  // 它一张表都没写，不该显示成「0.00ms」。
  box.appendChild(row('快照',
    cp.lastReason ? `${fmtNs(cp.lastNs)} · ${fmtBytes(cp.lastBytes)}` : '—',
    cp.lastReason
      ? `原因 ${CP_REASON_TEXT[cp.lastReason] || cp.lastReason} · 累计 ${cp.runs} 次 · p95 ${fmtNs(cp.p95Ns)}`
      : '还没写过快照',
    '快照是整表重写，占磁盘带宽；fsync 突然变慢常常正好撞上它在写',
    (cp.lastNs || 0) >= 200e6 ? 'warn' : ''));

  // 缓冲池：页存储之后「内存够不够」全看这一行。
  // 命中率 = 页就在池里，不用读盘；超预算 > 0 说明预算已经不够用了。
  const pool = io.pool || {};
  const poolLook = (pool.hits || 0) + (pool.misses || 0);
  const hitRate = poolLook > 0 ? (pool.hits / poolLook) * 100 : 0;
  const poolSub = poolLook > 0
    ? `脏页 ${pool.dirty || 0} · 淘汰 ${pool.evicts || 0} · 预取 ${pool.prefetched || 0}` +
      (pool.overflow ? ` · 超预算 ${pool.overflow}` : '')
    : '还没有页被读过';
  box.appendChild(row('缓冲池',
    poolLook > 0
      ? `${hitRate.toFixed(1)}% · ${pool.resident || 0}/${pool.budget || 0} 页`
      : '—',
    poolSub,
    '命中率高 = 页都在内存里，不用读盘。命中率低说明预算太小或访问太随机；出现「超预算」就该把缓冲池页数调大',
    poolLook > 0 && hitRate < 80 ? 'warn' : ''));

  // 页密度：页里真放着行数据的比例 —— 掉下来基本就是「删了不少、还没 VACUUM」。
  const sp = io.space || {};
  const tableList = io.tables || [];
  const fillOk = (sp.fillPages || 0) > 0;
  const sparse = tableList
    .filter(t => t.fillKnown && (t.pages || 0) >= 8)
    .sort((a, b) => (a.fill || 0) - (b.fill || 0))[0];
  const sparseLow = !!sparse && (sparse.fill || 0) < 0.6;
  box.appendChild(row('页密度',
    fillOk ? `${((sp.fill || 0) * 100).toFixed(0)}% · ${sp.pages} 页` : '—',
    !fillOk
      ? ((sp.pages || 0) > 0
        ? '表都还小（不足 8 页），小表的最后一页天然不满，统计了反而误导'
        : '还没有表落过盘')
      : (sparseLow
        ? `最稀疏：${sparse.name} ${((sparse.fill || 0) * 100).toFixed(0)}%` +
          `（${Math.round(sparse.rows / Math.max(1, sparse.pages))} 行/页）`
        : '各表都还算紧凑'),
    '页密度 = 1 - 页里空闲字节 / 页容量。掉下来通常是删了很多行还没整理：'
      + 'VACUUM 收页内的洞，VACUUM FULL 才能真的把页数降下来（整份重写，期间占着表锁）。'
      + '注意「洞」不算空闲，所以刚删完还没 VACUUM 时这个数不会降，要配合「行/页」看',
    fillOk && (sp.fill || 0) < 0.5 ? 'warn' : ''));

  // 索引：不进缓冲池，大小只能靠 stat；「重建次数」是一个本该为 0 的数。
  const ix = io.index || {};
  const ixRebuilt = (ix.rebuilds || 0) > 0;
  box.appendChild(row('索引',
    ix.indexes ? `${ix.indexes} 个 · ${fmtBytes(ix.bytes || 0)}` : '还没有索引',
    ix.indexes
      ? `${ix.pages || 0} 页` + (ixRebuilt ? ` · 重建 ${ix.rebuilds} 次（索引与表不同步）` : ' · 重建 0 次')
      : '建一个索引后这里会显示它的磁盘占用',
    '索引不写 WAL，靠 meta.json 里的代次和堆对齐：正常重启代次相等、叶子链校验通过，重建 0 次；'
      + '涨了说明索引文件过期或被判定损坏，那次启动白重建了一整棵树',
    ixRebuilt ? 'warn' : ''));

  const tables = tableList;
  const top = tables.slice(0, 3).map(t => `${t.name} ${fmtBytes(t.bytes)}`).join(' · ');
  box.appendChild(row('数据',
    `${tables.length} 表 / ${fmtBytes(io.totalBytes)}`,
    (top || '还没有表') + (sp.lobBytes ? ` · 大字段 ${fmtBytes(sp.lobBytes)}` : ''),
    '各表页文件（tables/tXXXXXX.dat）的大小；行内超过 2KB 的文本另存进 .lob，单独算'));
}

// 概览页的存储摘要：一行三个数（fsync / 缓冲池 / 页密度）。
// 完整的那张卡在「存储」页；这里放摘要，是因为「提交变慢」是最高频的排障入口，
// 不该让人先切页才能看到 fsync。做成一行而不是三行，是为了让概览一屏放得下 ——
// 摘要要滚一下才能看见就白放了。
function renderIOSummary(m) {
  const box = monEls.ioSum;
  if (!box) return;
  const io = m.io || {};
  const f = io.fsync || {};
  const pool = io.pool || {};
  const sp = io.space || {};
  const rt = io.rates || {};
  box.innerHTML = '';

  const item = (label, value, sub, tip, cls) => {
    const it = el('span', 'sum-item' + (cls ? ' ' + cls : ''));
    it.title = tip;
    it.appendChild(el('span', 'sum-k', label));
    it.appendChild(el('b', 'sum-v', value));
    if (sub) it.appendChild(el('span', 'sum-sub', sub));
    return it;
  };

  box.appendChild(item('fsync',
    f.runs ? fmtNs(f.p95Ns) : '—',
    f.runs ? `p95 · ${rt.fsyncPerSec || 0} 次/s` : '还没写过盘',
    'fsync 延迟就是提交的实际等待；WAL / 快照 / 缓冲池的完整数字在「存储」页',
    (f.p95Ns || 0) >= 10e6 ? 'bad' : ''));

  const look = (pool.hits || 0) + (pool.misses || 0);
  box.appendChild(item('缓冲池',
    look > 0 ? `${((pool.hits / look) * 100).toFixed(1)}%` : '—',
    look > 0 ? `${pool.resident || 0}/${pool.budget || 0} 页` : '还没读过页',
    '命中率高 = 页都在内存里；出现「超预算」说明 bufferPoolPages 该调大了',
    look > 0 && pool.hits / look < 0.8 ? 'warn' : ''));

  const fillOk = (sp.fillPages || 0) > 0;
  box.appendChild(item('页密度',
    fillOk ? `${((sp.fill || 0) * 100).toFixed(0)}%` : '—',
    fillOk ? `${sp.pages || 0} 页` : '表都还小',
    '页密度 = 1 - 页里可复用空闲 / 页容量；掉下来通常是删了很多行还没整理',
    fillOk && (sp.fill || 0) < 0.5 ? 'warn' : ''));
}

/* ---- 4. 锁明细 ---- */

function renderLocks(m) {
  const rows = m.locks || [];
  const body = monEls.lockBody;
  const panel = document.getElementById('monTabLock');
  body.innerHTML = '';

  const pg = monPageRows('lock', rows);
  renderPager('lock', pg.pages, pg.total);

  if (!rows.length) {
    setPanelEmpty(panel, monEls.lockOk, true,
      '当前没有表锁，所有会话都空闲（有事务或等锁时会自动展开）');
    updateDiagSub();
    return;
  }
  setPanelEmpty(panel, monEls.lockOk, false);
  updateDiagSub();

  pg.slice.forEach((r, i) => {
    const key = r.table + '|' + r.session;
    const tr = el('tr', 'mon-lock-row');
    if (monExpanded[key]) tr.classList.add('open');

    // 表名 + 锁类型
    const tdTable = el('td', 'c-table');
    tdTable.appendChild(el('span', 'mon-table', r.table));
    tr.appendChild(tdTable);

    const tdMode = el('td', 'c-mode');
    tdMode.appendChild(el('span', 'lk ' + (r.mode === 'S' ? 'lk-s' : 'lk-x'), r.mode));
    tr.appendChild(tdMode);

    const tdSess = el('td', 'c-sess mono');
    tdSess.textContent = sessionShort(r.session);
    tdSess.title = r.session;
    tr.appendChild(tdSess);

    // 已持有时长：核心指标，久了变黄/红，一眼扫出来
    const tdHeld = el('td', 'c-held mono');
    const heldCls = r.heldMs >= 30000 ? 'held bad' : (r.heldMs >= 5000 ? 'held warn' : 'held');
    tdHeld.appendChild(el('span', heldCls, fmtMs(r.heldMs)));
    tr.appendChild(tdHeld);

    const tdState = el('td', 'c-state');
    const running = r.txState === 'running';
    tdState.appendChild(el('span', 'tx-state ' + (running ? 'st-run' : 'st-idle'),
      running ? '执行中' : (r.txState ? '空闲' : '—')));
    tr.appendChild(tdState);

    const tdSql = el('td', 'c-sql mono');
    const preview = (r.lastSql || '').slice(0, 70);
    tdSql.textContent = preview || '—';
    tdSql.title = r.lastSql || '';
    tr.appendChild(tdSql);

    const tdWait = el('td', 'c-wait');
    if ((r.waiters || []).length) {
      for (const w of r.waiters) {
        const chip = el('button', 'wait-chip', `${sessionShort(w.session)}(${w.mode}, 等${fmtMs(w.waitedMs)})`);
        chip.type = 'button';
        chip.title = `会话 ${w.session} 在等 ${r.table} 的锁`;
        chip.addEventListener('click', ev => {
          ev.stopPropagation();
          const other = body.querySelector(`tr[data-sess="${w.session}"]`);
          if (other) {
            other.classList.add('flash');
            other.scrollIntoView({ block: 'center' });
            setTimeout(() => other.classList.remove('flash'), 1200);
          } else {
            toast(`会话 ${sessionShort(w.session)} 只在等锁，没有持有任何锁`);
          }
        });
        tdWait.appendChild(chip);
      }
    } else {
      tdWait.appendChild(el('span', 'muted', '—'));
    }
    tr.appendChild(tdWait);

    const tdAct = el('td', 'c-act');
    const bKill = el('button', 'btn btn-sm btn-warn', 'Kill');
    bKill.type = 'button';
    bKill.title = r.txState === 'running'
      ? '正在执行，无法立刻打断：标记取消，语句结束后自动回滚'
      : '立刻回滚这个事务并释放锁';
    bKill.addEventListener('click', ev => { ev.stopPropagation(); killSession(r.session, r); });
    tdAct.appendChild(bKill);

    const bExp = el('button', 'btn btn-sm btn-ghost', 'EXPLAIN');
    bExp.type = 'button';
    bExp.title = '把这条 SQL 放进编辑器并查看执行计划';
    bExp.addEventListener('click', ev => { ev.stopPropagation(); explainSQL(r.lastSql); });
    tdAct.appendChild(bExp);
    tr.appendChild(tdAct);

    tr.dataset.sess = r.session;
    tr.addEventListener('click', () => {
      monExpanded[key] = !monExpanded[key];
      renderMonitor(monData);
    });
    body.appendChild(tr);

    if (monExpanded[key]) {
      const det = el('tr', 'mon-detail-row');
      const td = el('td');
      td.colSpan = 8;
      const box = el('div', 'mon-detail');
      const act = (m.activities || []).find(a => a.session === r.session) || {};
      const line = (k, v) => {
        const p = el('span', 'd-item');
        p.appendChild(el('span', 'd-k', k));
        p.appendChild(el('span', 'd-v', v));
        box.appendChild(p);
      };
      line('会话', r.session);
      line('事务已持续', fmtMs(act.txAgeMs));
      line('空闲时长', fmtMs(act.txIdleMs) + '（距最后一条 SQL）');
      line('已执行语句', act.txStmts === undefined ? '—' : String(act.txStmts));
      line('涉及表', (act.tables || []).join(', ') || '—');
      const sqlBox = el('div', 'd-sql mono', r.lastSql || '—');
      box.appendChild(sqlBox);
      td.appendChild(box);
      det.appendChild(td);
      body.appendChild(det);
    }
  });
}

async function killSession(id, row) {
  const running = row && row.txState === 'running';
  const msg = running
    ? `会话 ${sessionShort(id)} 正在执行语句，引擎无法中途打断。\n会标记取消，等这条语句跑完后自动回滚事务。继续？`
    : `回滚会话 ${sessionShort(id)} 的事务并释放它持有的锁？未提交的改动会丢掉。`;
  if (!confirm(msg)) return;
  try {
    const r = await api('/api/monitor/kill', { session: id });
    if (r && r.error) {
      toast(r.error, true);
      return;
    }
    toast(running ? '已标记取消，语句结束后回滚' : '已回滚并释放锁');
    pollMonitor();
  } catch (e) {
    toast('Kill 失败：' + e.message, true);
  }
}

function explainSQL(sqlText) {
  if (!sqlText) {
    toast('这个会话没有可用的 SQL', true);
    return;
  }
  editor.value = sqlText;
  toggleMonitor(false);
  run('explain');
}

/* ---- 5. 慢SQL / 慢事务 ---- */

// 行数文案：百万以内给精确值（带千分位）—— 「2万」会把 19980 和 20001 抹平，
// 而判断「扫了多少」差一点就差很多。百万以上才换单位。
function fmtCount(n) {
  if (n === undefined || n === null) return '—';
  n = Number(n);
  if (!isFinite(n)) return '—';
  if (n < 1e6) return n.toLocaleString('en-US');
  if (n < 1e8) return (n / 1e4).toFixed(1).replace(/\.0$/, '') + '万';
  return (n / 1e8).toFixed(1).replace(/\.0$/, '') + '亿';
}

// 一行一个样本。表格只放「扫一眼够用」的四列，其余全在详情里。
function renderSlowSQL(m) {
  const box = monEls.slowSql;
  const panel = document.getElementById('monTabSql');
  box.innerHTML = '';
  const rows = monStmtRows(m);

  const pg = monPageRows('sql', rows);
  renderPager('sql', pg.pages, pg.total);

  if (!rows.length) {
    setPanelEmpty(panel, monEls.sqlOk, true, '还没有执行记录（跑几条 SQL 就会出现）');
    updateDiagSub();
    return;
  }
  setPanelEmpty(panel, monEls.sqlOk, false);
  updateDiagSub();
  pg.slice.forEach(r => {
    const tr = el('tr');
    if (r.running) tr.classList.add('running');
    tr.title = r.running ? '正在执行，还没结束' : '点击查看这组语句的执行明细';
    tr.appendChild(el('td', 'c-num', r.running ? '执行中' : fmtCount(r.n)));
    tr.appendChild(el('td', 'c-num', fmtNs(r.totalNs)));
    tr.appendChild(el('td', 'c-num', r.running ? '—' : fmtNs(r.avgNs)));
    tr.appendChild(el('td', 'c-num', fmtNs(r.maxNs)));
    tr.appendChild(el('td', 'c-sql', r.sql || '—'));
    // 正在执行的那条不属于任何结构，点开也没有明细可看
    if (!r.running) tr.addEventListener('click', () => openSlowDetail('sql', r));
    box.appendChild(tr);
  });
}

function renderSlowTx(m) {
  const box = monEls.slowTx;
  const panel = document.getElementById('monTabTx');
  box.innerHTML = '';
  const rows = monTxRows(m);

  const pg = monPageRows('tx', rows);
  renderPager('tx', pg.pages, pg.total);

  if (!rows.length) {
    setPanelEmpty(panel, monEls.txOk, true, '还没有结束的事务（BEGIN … COMMIT 之后记一条）');
    updateDiagSub();
    return;
  }
  setPanelEmpty(panel, monEls.txOk, false);
  updateDiagSub();
  pg.slice.forEach(r => {
    const tr = el('tr');
    tr.title = '点击查看这类事务的明细';
    tr.appendChild(el('td', 'c-num', fmtCount(r.n)));
    tr.appendChild(el('td', 'c-num', fmtMs(r.totalMs)));
    tr.appendChild(el('td', 'c-num', fmtMs(r.avgMs)));
    tr.appendChild(el('td', 'c-num', fmtMs(r.maxMs)));
    const label = (r.write ? '写 · ' : '读 · ') + ((r.tables || []).join(', ') || '(无表)');
    tr.appendChild(el('td', 'c-sql', label));
    tr.addEventListener('click', () => openSlowDetail('tx', r));
    box.appendChild(tr);
  });
}

/* ---- 5. 空间：四块占比 + 逐表明细（点一行下钻到页级构成） ----
   后端两档代价差一个量级：
   - GET /api/space          只 stat 文件 + 读 meta，毫秒级；
   - GET /api/space?table=x  逐页扫（8KB×页数），点开某张表才算。
   所以这里不跟轮询，只在切到本 Tab / 点「重新扫描」时取。 */
let spaceData = null;   // { space, tables }
let spaceOpenName = ''; // 当前展开的表名（点击同一行=收起）

async function loadSpace() {
  try {
    spaceData = await api('/api/space');
  } catch (e) {
    spaceData = { space: {}, tables: [] };
    if (monEls.spaceLegend) monEls.spaceLegend.textContent = '扫描失败：' + e.message;
    return;
  }
  renderSpace();
  updateDiagSub();
  // 空间地图的数据源就是这个接口：树图的面积、按文件的块数全靠它，扫完要重铺
  if (monPageTab === 'map') renderMap();
}

// 时间戳 → 「多久以前」。注意和文件后面的 fmtAgo(时长) 不是一回事：
// 那个收的是「差了多久（ms）」，这个收的是「某个时刻（Unix ms）」。同名会被后来的盖掉
// —— 踩过一次：刚 VACUUM 过的表显示「498171 小时前」。
function fmtSince(ts) {
  if (!ts) return '从未';
  const d = Date.now() - ts;
  if (d < 90e3) return '刚刚';
  if (d < 3600e3) return `${Math.round(d / 60e3)} 分钟前`;
  if (d < 86400e3) return `${Math.round(d / 3600e3)} 小时前`;
  if (d < 30 * 86400e3) return `${Math.round(d / 86400e3)} 天前`;
  return `${Math.round(d / (30 * 86400e3))} 个月前`;
}

function spaceLegendItem(cls, label, bytes, total) {
  const item = el('span', 'space-item');
  item.appendChild(el('span', 'space-dot s-' + cls));
  item.appendChild(el('span', null, label));
  const b = el('b', null, fmtBytes(bytes));
  item.appendChild(b);
  if (total > 0) item.appendChild(el('span', null, `${((bytes / total) * 100).toFixed(0)}%`));
  return item;
}

function renderSpace() {
  if (!spaceData || !monEls.spaceBody) return;
  const sp = spaceData.space || {};
  const tables = spaceData.tables || [];
  const total = (sp.dataBytes || 0) + (sp.indexBytes || 0) + (sp.lobBytes || 0) + (sp.walBytes || 0);

  // 占比条：四块按字节比例分宽度；总和为 0 时留一条空槽。
  // 顺序是「索引 → 数据 → 大字段 → WAL」：这个库里索引经常比数据还大
  // （主键 + 每张二级索引各一份），先看索引才容易发现「六成空间是索引」这种事。
  monEls.spaceBar.innerHTML = '';
  monEls.spaceLegend.innerHTML = '';
  const parts = [
    ['index', '索引', sp.indexBytes || 0],
    ['data', '数据', sp.dataBytes || 0],
    ['lob', '大字段', sp.lobBytes || 0],
    ['wal', 'WAL', sp.walBytes || 0],
  ];
  for (const [cls, label, bytes] of parts) {
    const seg = el('div', 'space-seg s-' + cls);
    seg.style.flex = total > 0 ? String(bytes) : '0';
    seg.title = `${label} ${fmtBytes(bytes)}` + (total > 0 ? ` · ${((bytes / total) * 100).toFixed(1)}%` : '');
    monEls.spaceBar.appendChild(seg);
    monEls.spaceLegend.appendChild(spaceLegendItem(cls, label, bytes, total));
  }

  monEls.spaceBody.innerHTML = '';
  const empty = tables.length === 0;
  monEls.spaceOk.classList.toggle('hidden', !empty);
  if (empty) {
    monEls.spaceOk.textContent = '还没有表';
    return;
  }
  for (const t of tables) {
    const tr = el('tr');
    if (t.name === spaceOpenName) tr.classList.add('on');
    tr.title = '点击逐页统计这张表的「已用 / 空闲 / 洞」';
    tr.appendChild(el('td', 'c-table', t.name));
    // 列顺序和卡片顶部的占比条保持一致：索引在前、数据在后
    tr.appendChild(el('td', 'c-num', fmtBytes(t.indexBytes || 0)));
    tr.appendChild(el('td', 'c-num', fmtBytes(t.bytes || 0)));
    tr.appendChild(el('td', 'c-num', fmtBytes(t.lobBytes || 0)));
    tr.appendChild(el('td', 'c-num', fmtCount(t.rows || 0)));
    tr.appendChild(el('td', 'c-num', String(t.pages || 0)));
    // 太小的表（<8 页）不显示密度：最后一页天然不满，1 页的表永远是 4% 这种数，
    // 只会误导。和全库那个百分比的阈值保持一致。
    const fillText = t.fillKnown && (t.pages || 0) >= 8
      ? `${((t.fill || 0) * 100).toFixed(0)}%` : '—';
    tr.appendChild(el('td', 'c-num', fillText));
    // 密度只说「现在多空」；配上「多久没整理」才是可执行的信息
    const vacCell = el('td', 'c-num', fmtSince(t.vacuumAt));
    vacCell.title = `上次整理：${fmtSince(t.vacuumAt)}`
      + `\n上次整份重写：${fmtSince(t.rewriteAt)}（VACUUM FULL 才更新）`
      + '\n整理 = VACUUM 或 VACUUM FULL；前者只收页内的洞，后者才降页数';
    tr.appendChild(vacCell);
    tr.addEventListener('click', () => toggleSpaceDetail(t.name));
    monEls.spaceBody.appendChild(tr);
  }
  if (monEls.spaceSub) {
    monEls.spaceSub.textContent = `${tables.length} 张表 · 合计 ${fmtBytes(total)} · 点表名逐页看构成`;
  }
}

async function toggleSpaceDetail(name) {
  const box = monEls.spaceDetail;
  if (!box) return;
  if (spaceOpenName === name) { // 再点一次收起
    spaceOpenName = '';
    box.classList.add('hidden');
    box.innerHTML = '';
    renderSpace();
    return;
  }
  spaceOpenName = name;
  renderSpace();
  box.classList.remove('hidden');
  box.innerHTML = '';
  box.appendChild(el('div', 'space-note', '正在逐页统计…'));
  try {
    const d = await api('/api/space?table=' + encodeURIComponent(name));
    if (spaceOpenName !== name) return; // 期间又点了别的表
    renderSpaceDetail(d);
  } catch (e) {
    box.innerHTML = '';
    box.appendChild(el('div', 'space-note', '统计失败：' + e.message));
  }
}

// 页级构成：把 页数×8KB 拆成 已用 / 空闲 / 洞。三者回收手段不同，所以分开画。
function renderSpaceDetail(d) {
  const box = monEls.spaceDetail;
  if (!box) return;
  box.innerHTML = '';
  const cap = d.capacity || 0;
  const used = d.liveBytes || 0;
  const free = d.freeBytes || 0;
  const hole = d.holeBytes || 0;
  const mine = Math.max(1, used + free + hole); // 剩余的是页头+槽位目录

  const head = el('div', 'space-dhead');
  head.appendChild(el('span', 'space-dname', d.table));
  head.appendChild(el('span', 'space-dmeta',
    `${d.pages} 页 · 容量 ${fmtBytes(cap)} · ${fmtCount(d.liveRows)} 行`
    + (d.deadSlots ? ` · ${fmtCount(d.deadSlots)} 个空槽` : '')));
  box.appendChild(head);

  const bar = el('div', 'space-dbar');
  for (const [cls, bytes] of [['used', used], ['free', free], ['hole', hole]]) {
    const seg = el('div', 'space-dseg s-' + cls);
    seg.style.flex = String(Math.max(0, bytes));
    bar.appendChild(seg);
  }
  box.appendChild(bar);

  const legend = el('div', 'space-legend');
  legend.appendChild(spaceLegendItem('used', '已用', used, mine));
  legend.appendChild(spaceLegendItem('free', '空闲', free, mine));
  legend.appendChild(spaceLegendItem('hole', '洞', hole, mine));
  box.appendChild(legend);

  const note = el('div', 'space-note');
  if (hole > 0) {
    // 表名来自库里的对象，一律 textContent，不拼 HTML
    note.textContent = '「洞」是被删掉的行留下的字节：插入用不上它，只有 VACUUM 能把它变成空闲。'
      + `跑一次 VACUUM ${d.table} 就能收回；页数要降下来还得 VACUUM FULL（整份重写，期间占表锁）。`;
  } else if (free > 0) {
    note.textContent = '没有洞：空闲字节是整理过的，插入可以直接复用。'
      + '要让页数真的降下来（扫描少读页）才需要 VACUUM FULL。';
  } else {
    note.textContent = '这一页数据很满，没什么可回收的。';
  }
  box.appendChild(note);

  // 这一块只给「有几个洞、多少空闲」；要看洞长在哪一页，去空间地图
  const go = el('button', 'space-rescan', '在空间地图里看这些页 ›');
  go.type = 'button';
  go.style.marginTop = '8px';
  go.addEventListener('click', ev => { ev.stopPropagation(); gotoMap(d.table); });
  box.appendChild(go);
}

if (document.getElementById('spaceRescan')) {
  document.getElementById('spaceRescan').addEventListener('click', () => {
    spaceOpenName = '';
    if (monEls.spaceDetail) {
      monEls.spaceDetail.classList.add('hidden');
      monEls.spaceDetail.innerHTML = '';
    }
    loadSpace();
  });
}

/* ---- 5.2 空间地图：库视图（矩形树图）→ 表视图（页网格）→ 页/行 ----
   一个 Tab 两级画布，右边永远是「当前选中对象的详情」：
   - 库视图：面积正比于字节的矩形树图，一格一个对象，点一格进表；
   - 表视图：一格一页，铺满画布（20 万行的表 1450 页能一屏看完，卡片里铺不下）；
   - 右栏：L1 是表详情 + 索引清单，L2 是页头 / 槽位 / 结构 / 文本 / 十六进制 / 关联。
   后端只读文件、不进缓冲池，所以随便点都不会影响数据库；代价是看到的是磁盘上的样子。 */
const MAP_RAIL_DEFAULT = 460;
const MAP_RAIL_MIN = 360;
const MAP_RAIL_MAX = 760;
const MAP_MAX_CELLS = 20;        // 树图最多铺多少块，多出来的合并成「其它」
let mapLevel = 'db';             // db | table
let mapGroup = 'table';          // 树图分组：table（一张表一块）| file（数据 / 每棵索引 / 大字段）
let mapSelTable = '';            // L1 选中的表（右栏详情 + 面包屑）
let mapRailW = Number(localStorage.getItem('godb.mapRailW')) || MAP_RAIL_DEFAULT;
let mapZoom = 1;                  // 视图缩放：1 = 铺满画布；只放大「当前这块区域」，不换页
const MAP_ZOOM_MAX = 20;
let tvPan = { x: 0, y: 0 };       // 树图的平移量（屏幕像素，恒为负或 0）
let pmMap = null;                // 当前文件的页地图快照
let pmSel = { table: '', kind: 'data', index: '' };
let pmOpen = -1;                 // 当前打开的页号
let pmDetail = null;             // 当前页的详情（重渲染用）
let pmLink = null;               // 「索引项 → 堆页 → 大字段」定位结果
let pmTab = 'struct';            // struct | text | hex
const pmCache = new Map();       // 页地图按「表|类型|索引」缓存：切回来不重读（1450 页 ≈ 11 MB）

/* ---- 5.2.1 矩形树图（squarified treemap） ---- */

// squarify 把一组 {value} 铺进矩形，尽量让每块接近正方形 ——
// 直接按比例切长条（slice-and-dice）会切出一堆细缝，名字根本放不下。
function squarify(items, x, y, w, h) {
  const out = [];
  const total = items.reduce((a, b) => a + b.value, 0);
  if (!total || w <= 0 || h <= 0) return out;
  const scale = (w * h) / total;
  let rest = items.map(it => Object.assign({}, it, { area: it.value * scale }));
  let rx = x, ry = y, rw = w, rh = h;

  const worst = (row, short) => {
    const s = row.reduce((a, b) => a + b.area, 0);
    let mx = 0, mn = Infinity;
    for (const it of row) {
      if (it.area > mx) mx = it.area;
      if (it.area < mn) mn = it.area;
    }
    const s2 = s * s, w2 = short * short;
    return Math.max((w2 * mx) / s2, s2 / (w2 * mn));
  };

  while (rest.length) {
    const short = Math.min(rw, rh);
    let row = [rest[0]];
    let best = worst(row, short);
    let i = 1;
    while (i < rest.length) {
      const next = row.concat([rest[i]]);
      const wr = worst(next, short);
      if (wr > best) break;
      row = next; best = wr; i++;
    }
    const sum = row.reduce((a, b) => a + b.area, 0);
    if (rw >= rh) {
      const colW = sum / rh;
      let cy = ry;
      for (const it of row) {
        const ih = it.area / colW;
        out.push({ item: it, x: rx, y: cy, w: colW, h: ih });
        cy += ih;
      }
      rx += colW; rw -= colW;
    } else {
      const rowH = sum / rw;
      let cx = rx;
      for (const it of row) {
        const iw = it.area / rowH;
        out.push({ item: it, x: cx, y: ry, w: iw, h: rowH });
        cx += iw;
      }
      ry += rowH; rh -= rowH;
    }
    rest = rest.slice(row.length);
  }
  return out;
}

function mapTableOf(name) {
  return ((spaceData && spaceData.tables) || []).find(t => t.name === name) || null;
}

// 树图的一块：value 是字节（面积就按它分），parts 是内部构成（底部那条色带）。
function treemapItems() {
  const tables = ((spaceData && spaceData.tables) || []).slice();
  const out = [];
  if (mapGroup === 'file') {
    for (const t of tables) {
      if (t.bytes > 0) {
        out.push({ name: t.name, value: t.bytes, parts: { data: t.bytes }, kind: 'data',
          table: t.name, target: { kind: 'data', index: '' } });
      }
      for (const ix of t.indexes || []) {
        if (ix.bytes > 0) {
          out.push({ name: `${t.name}.${ix.name}`, value: ix.bytes, parts: { index: ix.bytes },
            kind: 'index', table: t.name, target: { kind: 'index', index: ix.name } });
        }
      }
      if (t.lobBytes > 0) {
        out.push({ name: `${t.name} · 大字段`, value: t.lobBytes, parts: { lob: t.lobBytes },
          kind: 'lob', table: t.name, target: null });
      }
    }
  } else {
    for (const t of tables) {
      const value = (t.bytes || 0) + (t.indexBytes || 0) + (t.lobBytes || 0);
      if (value <= 0) continue;      // 还没落过盘的表在树图上没有位置，底栏会说明
      out.push({ name: t.name, value, parts: { data: t.bytes, index: t.indexBytes, lob: t.lobBytes },
        kind: 'table', table: t.name, target: { kind: 'data', index: '' } });
    }
  }
  // WAL 也占地方，不给它一块的话「各块之和 ≠ 全库占用」，看的人会怀疑数字错了
  const wal = (spaceData && spaceData.space && spaceData.space.walBytes) || 0;
  if (wal > 0) {
    out.push({ name: 'WAL', value: wal, parts: { wal }, kind: 'wal', table: '', target: null });
  }
  out.sort((a, b) => b.value - a.value);
  return out;
}

// 块太多就合并最小的那些：硬画出来大家都只剩几个像素，标签全糊在一起。
function mergeSmall(items) {
  if (items.length <= MAP_MAX_CELLS) return { list: items, tail: [] };
  const head = items.slice(0, MAP_MAX_CELLS - 1);
  const tail = items.slice(MAP_MAX_CELLS - 1);
  head.push({
    name: `其它 ${tail.length} 块`,
    value: tail.reduce((a, x) => a + x.value, 0),
    parts: {}, kind: 'other', table: '', target: null, tail,
  });
  return { list: head, tail };
}

function treeTip(it) {
  if (it.kind === 'other') {
    return `其它 ${it.tail.length} 块：${it.tail.slice(0, 6).map(x => x.name).join('、')}`
      + (it.tail.length > 6 ? ' …' : '');
  }
  if (it.kind === 'wal') return `WAL（预写日志）${fmtBytes(it.value)} · 顺序日志，没有「页」可以下钻`;
  const p = it.parts || {};
  const bits = [];
  if (p.data) bits.push(`数据 ${fmtBytes(p.data)}`);
  if (p.index) bits.push(`索引 ${fmtBytes(p.index)}`);
  if (p.lob) bits.push(`大字段 ${fmtBytes(p.lob)}`);
  return `${it.name} · ${fmtBytes(it.value)}` + (bits.length ? `（${bits.join(' / ')}）` : '')
    + '\n单击看详情，双击进页地图';
}

function renderTreemap() {
  const box = monEls.mapTree;
  if (!box) return;
  box.innerHTML = '';
  const items = treemapItems();
  if (!items.length) {
    box.appendChild(el('div', 'map-empty', '还没有落过盘的表 —— 先 CHECKPOINT 一次，页才会写到磁盘上'));
    return;
  }
  const W = box.clientWidth, H = box.clientHeight;
  if (W < 40 || H < 40) return;  // 页面还没显示出来（宽高为 0），切回来会重画
  clampTreePan(W, H);
  const { list } = mergeSmall(items);
  // 缩放不用 CSS transform，而是在「W×k / H×k 的虚拟画布」上重新排版：
  // 面积比例不变（还是正比于字节），但每个格子是实打实的屏幕像素 ——
  // 文字不会被拉伸，放大后小格子够大了，名字自然就出来了。
  const k = mapZoom;
  for (const c of squarify(list, 0, 0, W * k, H * k)) drawTreeCell(box, c);
  renderZoomHud();
}

// 平移量夹紧：虚拟画布比视口大多少，就最多能拖多少，不能把图拖出画面。
function clampTreePan(W, H) {
  const k = mapZoom;
  if (k <= 1) {
    tvPan.x = 0; tvPan.y = 0;
    return;
  }
  tvPan.x = Math.min(0, Math.max(W - W * k, tvPan.x));
  tvPan.y = Math.min(0, Math.max(H - H * k, tvPan.y));
}

// 拖动时只挪位置、不重排：每帧重建 20 个格子会闪。
function applyTreePan() {
  const box = monEls.mapTree;
  if (!box) return;
  for (const d of box.children) {
    const bx = d.dataset.bx, by = d.dataset.by;
    if (bx === undefined) continue;
    d.style.left = `${Number(bx) + tvPan.x}px`;
    d.style.top = `${Number(by) + tvPan.y}px`;
  }
}

let tvRaf = 0;
function scheduleTreemap() {
  if (tvRaf) return;
  tvRaf = requestAnimationFrame(() => {
    tvRaf = 0;
    renderTreemap();
  });
}

// 以画布里的某个点为锚缩放：那个点在缩放前后停在原地（滚轮缩放的手感）。
function setMapZoom(next, anchor) {
  const box = monEls.mapTree;
  if (!box) return;
  const old = mapZoom;
  const k = Math.max(1, Math.min(MAP_ZOOM_MAX, next));
  if (Math.abs(k - old) < 1e-4) return;
  if (mapLevel === 'db' && box.clientWidth > 40) {
    const r = box.getBoundingClientRect();
    const ax = anchor ? anchor.x - r.left : box.clientWidth / 2;
    const ay = anchor ? anchor.y - r.top : box.clientHeight / 2;
    tvPan.x = ax - (ax - tvPan.x) * (k / old);
    tvPan.y = ay - (ay - tvPan.y) * (k / old);
  }
  mapZoom = k;
  if (mapLevel === 'table') renderPageMap();
  else scheduleTreemap();
  renderZoomHud();
}

function resetMapZoom() {
  mapZoom = 1;
  tvPan.x = 0;
  tvPan.y = 0;
  renderZoomHud();
}

function renderZoomHud() {
  const hud = monEls.mapZoom;
  if (!hud) return;
  hud.classList.toggle('hidden', monPageTab !== 'map');
  if (monEls.mapZoomVal) {
    monEls.mapZoomVal.textContent = mapZoom < 9.95 ? mapZoom.toFixed(1) + '×' : Math.round(mapZoom) + '×';
  }
  if (monEls.mapTree) monEls.mapTree.classList.toggle('can-pan', mapLevel === 'db' && mapZoom > 1);
}

function drawTreeCell(box, c) {
  const it = c.item;
  const w = Math.max(2, c.w - 1), h = Math.max(2, c.h - 1);
  const on = mapLevel === 'db' && mapSelTable && it.table === mapSelTable;
  const d = el('div', 'tv-cell' + (it.kind === 'other' ? ' other' : '') + (on ? ' on' : ''));
  d.dataset.bx = String(c.x);
  d.dataset.by = String(c.y);
  d.style.left = `${c.x + tvPan.x}px`;
  d.style.top = `${c.y + tvPan.y}px`;
  d.style.width = `${w}px`;
  d.style.height = `${h}px`;
  d.title = treeTip(it);
  // 细高的条（宽 54、高几千）横着排只能塞下四个字，名字得竖着写 ——
  // 这正是「数据多时某些块太小」最难受的形态，放大之后要靠它才读得出来。
  const vlabel = h >= 120 && w < 92;
  if (vlabel) d.classList.add('vlabel');
  if (w >= 54 && h >= 22) d.appendChild(el('span', 'tv-name', it.name));
  if (!vlabel && w >= 54 && h >= 40) d.appendChild(el('span', 'tv-size', fmtBytes(it.value)));
  const p = it.parts || {};
  const segs = [['data', p.data], ['index', p.index], ['lob', p.lob], ['wal', p.wal]].filter(x => x[1] > 0);
  if (segs.length) {
    // 主导类别决定整块淡色底：面积大的块一眼能看出「主要是索引」还是「主要是数据」
    const top = segs.slice().sort((a, b) => b[1] - a[1])[0][0];
    d.dataset.tint = top;
  }
  if (segs.length && h >= 8) {
    const bar = el('div', 'tv-parts');
    for (const [cls, v] of segs) {
      const seg = el('i', 'p-' + cls);
      seg.style.flex = String(v);
      bar.appendChild(seg);
    }
    d.appendChild(bar);
  }
  if (it.kind === 'other') {
    d.addEventListener('click', () => showOtherBlocks(it.tail));
  } else if (it.kind === 'wal') {
    d.addEventListener('click', () => showWalBlock());
  } else {
    d.addEventListener('click', () => {
      mapSelTable = it.table;
      renderTreemap();
      renderRailDb();
      renderCrumbs();
    });
    d.addEventListener('dblclick', () => enterTable(it.table, it.target && it.target.kind, it.target && it.target.index));
  }
  box.appendChild(d);
}

function showOtherBlocks(tail) {
  const side = monEls.pmSide;
  side.innerHTML = '';
  side.appendChild(el('div', 'pm-head', `其它 ${tail.length} 块`));
  side.appendChild(el('div', 'pm-hint',
    '这些都很小，画成格子只剩几个像素、标签也放不下，所以合并了。'));
  for (const it of tail) {
    const row = el('div', 'pm-row');
    row.appendChild(el('span', 'pm-val', `${it.name} · ${fmtBytes(it.value)}`));
    side.appendChild(row);
  }
}

function showWalBlock() {
  const side = monEls.pmSide;
  side.innerHTML = '';
  const sp = (spaceData && spaceData.space) || {};
  side.appendChild(el('div', 'pm-head', 'WAL（预写日志）'));
  side.appendChild(el('div', 'pm-hint',
    `现在 ${fmtBytes(sp.walBytes || 0)}。WAL 是顺序追加的日志，没有「页」和「槽位」可以下钻 ——`
    + '它由 checkpoint 轮转回收，不是 VACUUM。'));
}

/* ---- 5.2.2 面包屑与右栏 ---- */

function renderCrumbs() {
  const box = monEls.mapCrumbs;
  if (!box) return;
  box.innerHTML = '';
  const root = el('button', 'map-crumb' + (mapLevel === 'db' ? ' here' : ''), '库');
  root.type = 'button';
  if (mapLevel !== 'db') root.addEventListener('click', backToDb);
  box.appendChild(root);

  if (mapLevel === 'db') {
    if (mapSelTable) {
      box.appendChild(el('span', 'map-crumb-sep', '›'));
      box.appendChild(el('span', 'map-crumb here', mapSelTable));
    }
    return;
  }
  box.appendChild(el('span', 'map-crumb-sep', '›'));
  const t = el('button', 'map-crumb', pmSel.table);
  t.type = 'button';
  t.title = '回到库视图（Esc）';
  t.addEventListener('click', backToDb);
  box.appendChild(t);
  box.appendChild(el('span', 'map-crumb-sep', '›'));
  box.appendChild(el('span', 'map-crumb here', pmSel.kind === 'index' ? `索引 ${pmSel.index}` : '数据页'));
}

// 右栏在库视图：没选中就是全库汇总，选中了就是那张表的详情。
function renderRailDb() {
  const side = monEls.pmSide;
  if (!side) return;
  side.innerHTML = '';
  if (mapSelTable && mapTableOf(mapSelTable)) {
    drawTableCard(side, mapTableOf(mapSelTable));
    return;
  }
  const sp = (spaceData && spaceData.space) || {};
  const tables = (spaceData && spaceData.tables) || [];
  side.appendChild(el('div', 'pm-head', '全库'));
  const kv = el('div', 'pm-kv');
  const add = (k, v) => {
    kv.appendChild(el('span', null, k));
    kv.appendChild(el('b', null, v));
  };
  add('数据', fmtBytes(sp.dataBytes || 0));
  add('索引', fmtBytes(sp.indexBytes || 0));
  add('大字段', fmtBytes(sp.lobBytes || 0));
  add('WAL', fmtBytes(sp.walBytes || 0));
  add('表数', `${tables.length}`);
  add('页数', `${sp.pages || 0}`);
  side.appendChild(kv);
  side.appendChild(el('div', 'pm-hint',
    '面积正比于字节 —— 一块大不代表行多，只代表它占盘多（索引经常比数据本身还大）。\n'
    + '单击看这张表，双击直接进它的页地图；切「按文件」能把每棵索引单独铺出来。'));
}

function drawTableCard(side, t) {
  side.appendChild(el('div', 'pm-head', t.name));
  const kv = el('div', 'pm-kv');
  const add = (k, v) => {
    kv.appendChild(el('span', null, k));
    kv.appendChild(el('b', null, v));
  };
  add('数据', `${fmtBytes(t.bytes || 0)} · ${t.pages || 0} 页`);
  add('索引', `${fmtBytes(t.indexBytes || 0)} · ${(t.indexes || []).length} 棵`);
  if (t.lobBytes) add('大字段', fmtBytes(t.lobBytes));
  add('行数', fmtCount(t.rows || 0));
  const fillOk = t.fillKnown && (t.pages || 0) >= 8;
  add('页密度', fillOk ? `${((t.fill || 0) * 100).toFixed(0)}%` : `${t.pages || 0} 页太小，密度没意义`);
  add('上次整理', fmtSince(t.vacuumAt));
  side.appendChild(kv);

  if ((t.indexes || []).length) {
    side.appendChild(el('div', 'pm-hint', '索引（点一棵直接看它的页）：'));
    for (const ix of t.indexes) {
      const row = el('div', 'pm-row linkable');
      row.appendChild(el('span', 'pm-val', `${ix.name} · ${fmtBytes(ix.bytes)} · ${ix.pages} 页`));
      row.title = '点一下：打开这棵索引的页地图';
      row.addEventListener('click', () => enterTable(t.name, 'index', ix.name));
      side.appendChild(row);
    }
  }
  const go = el('button', 'space-rescan', '打开页地图 ›');
  go.type = 'button';
  go.style.marginTop = '10px';
  go.addEventListener('click', () => enterTable(t.name, 'data', ''));
  side.appendChild(go);
}

/* ---- 5.2.3 表视图：页网格 ---- */

function railHint(text) {
  const side = monEls.pmSide;
  if (!side) return;
  side.innerHTML = '';
  side.appendChild(el('div', 'pm-hint', text));
}

function enterTable(table, kind, index) {
  if (!table) return;
  mapLevel = 'table';
  mapSelTable = table;
  pmSel = { table, kind: kind || 'data', index: index || '' };
  pmMap = null;
  pmOpen = -1; pmDetail = null; pmLink = null;
  resetMapZoom();
  renderMap();
  railHint('点左边任意一格，看这一页的结构、文本和字节。');
  loadPageMap(false);
}

function backToDb() {
  if (mapLevel !== 'table') return;
  mapLevel = 'db';
  pmOpen = -1; pmDetail = null; pmLink = null;
  resetMapZoom();
  renderMap();
}

function selectTarget(table, kind, index) {
  if (pmSel.table === table && pmSel.kind === kind && (pmSel.index || '') === (index || '')) return;
  pmSel = { table, kind, index: index || '' };
  pmMap = null;
  pmOpen = -1; pmDetail = null; pmLink = null;
  renderFileBar();
  renderCrumbs();
  railHint('点左边任意一格，看这一页的结构、文本和字节。');
  return loadPageMap(false);
}

function renderFileBar() {
  const box = monEls.mapFiles;
  if (!box) return;
  box.innerHTML = '';
  const t = mapTableOf(pmSel.table);
  const add = (label, kind, index, tip) => {
    const on = pmSel.kind === kind && (pmSel.index || '') === (index || '');
    const b = el('button', 'map-file' + (on ? ' on' : ''), label);
    b.type = 'button';
    if (tip) b.title = tip;
    b.addEventListener('click', () => selectTarget(pmSel.table, kind, index));
    box.appendChild(b);
  };
  add('数据页', 'data', '', `${t ? t.pages : 0} 页`);
  for (const ix of (t && t.indexes) || []) add(ix.name, 'index', ix.name, `${ix.pages} 页 · ${fmtBytes(ix.bytes)}`);
  if (!(t && (t.indexes || []).length)) box.appendChild(el('span', 'map-note', '这张表没有二级索引'));
}

// 格子大小随画布与页数自适应：目标是「尽量一屏铺完」，而不是固定 12px。
function fitCell(box, count) {
  const W = box.clientWidth, H = box.clientHeight;
  if (!W || !H) return;
  const gap = count > 2500 ? 2 : (count > 800 ? 3 : 4);
  let cell = Math.floor(Math.sqrt((W * H) / Math.max(1, count)) - gap);
  // 上限给到 72：页少的表就该画成大方块，卡在 30 会让一张 1 页的表缩在左上角像坏了。
  cell = Math.max(5, Math.min(72, cell));
  // 放大：格子直接乘倍数，超过画布就滚动 —— 页数太多（格子掉到 5px 下限）时靠这个看清
  if (mapZoom > 1) {
    const z = Math.min(96, Math.max(5, Math.round(cell * mapZoom)));
    box.style.setProperty('--pm-cell', `${z}px`);
    box.style.setProperty('--pm-gap', `${gap}px`);
    box.classList.remove('center');
    return;
  }
  // 再按「行数×行高」验一遍：flex 换行会有取整误差，差 1px 就冒出滚动条，
  // 而滚动条一出现又压缩可用宽度 —— 干脆缩到放得下为止。
  for (; cell > 5; cell--) {
    const cols = Math.max(1, Math.floor((W + gap) / (cell + gap)));
    const rows = Math.ceil(count / cols);
    if (rows * (cell + gap) - gap <= H) break;
  }
  box.style.setProperty('--pm-cell', `${cell}px`);
  box.style.setProperty('--pm-gap', `${gap}px`);
  // 铺不满一半就居中，不然一两个格子孤零零贴在左上角
  const used = count * (cell + gap) * (cell + gap);
  box.classList.toggle('center', used < W * H * 0.5);
}

function pmMapMatches() {
  return !!pmMap && pmMap.table === pmSel.table && pmMap.kind === pmSel.kind
    && (pmMap.index || '') === (pmSel.index || '');
}

async function loadPageMap(append) {
  const table = pmSel.table;
  if (!table) return;
  const key = `${table}|${pmSel.kind}|${pmSel.index || ''}`;
  if (!append) {
    const hit = pmCache.get(key);
    if (hit) { pmMap = hit; renderPageMap(); return; }
    pmMap = null;
    if (monEls.mapNote) monEls.mapNote.textContent = '正在扫页头（只读目录，不进缓冲池）…';
  }
  const from = append && pmMap && pmMap.next ? pmMap.next : 0;
  const q = `table=${encodeURIComponent(table)}&kind=${pmSel.kind}`
    + (pmSel.index ? `&index=${encodeURIComponent(pmSel.index)}` : '')
    + `&from=${from}`;
  let res;
  try {
    res = await api('/api/pages?' + q);
  } catch (e) {
    if (monEls.pmGrid) monEls.pmGrid.innerHTML = '';
    if (monEls.mapNote) monEls.mapNote.textContent = `扫描失败：${e.message}`;
    return;
  }
  if (key !== `${pmSel.table}|${pmSel.kind}|${pmSel.index || '' }`) return;  // 期间切走了
  if (append && pmMap) {
    pmMap.pages = (pmMap.pages || []).concat(res.pages || []);
    pmMap.next = res.next || 0;
    pmMap.bytes = (pmMap.bytes || 0) + (res.bytes || 0);
  } else {
    pmMap = res;
  }
  pmCache.set(key, pmMap);
  renderPageMap();
}

function renderPageMapError(msg) {
  if (monEls.pmGrid) monEls.pmGrid.innerHTML = '';
  if (monEls.mapNote) monEls.mapNote.textContent = `扫描失败：${msg}`;
}

// 一格 = 一页。颜色按「这一页有多满」分档；有洞的页优先标橙（那才是能整理的地方）。
function pmCellClass(p) {
  if (p.bad) return 'bad';
  if (pmSel.kind === 'index') {
    if (p.role === 'header') return 'hdr';
    if (p.role === 'inner') return 'inner';
    return p.live > 0 ? 'idx' : 'idx-empty';
  }
  if (p.hole > 0) return 'hole';
  if (!p.used) return 'empty';
  const ratio = p.used / 8192;
  if (ratio >= 0.75) return 'full';
  if (ratio >= 0.35) return 'mid';
  return 'sparse';
}

function renderPageMap() {
  const box = monEls.pmGrid;
  if (!box || !pmMap) return;
  const pages = pmMap.pages || [];
  fitCell(box, pages.length || pmMap.total || 1);
  box.innerHTML = '';
  const frag = document.createDocumentFragment();
  for (const p of pages) {
    const c = el('div', `pm-c ${pmCellClass(p)}`);
    c.dataset.no = String(p.no);
    if (p.no === pmOpen) c.classList.add('on');
    c.title = `第 ${p.no} 页 · ${p.live} 条`
      + (pmSel.kind === 'index' ? '' : ` · 已用 ${fmtBytes(p.used)} · 空闲 ${fmtBytes(p.free)}`)
      + (p.hole ? ` · 洞 ${fmtBytes(p.hole)}` : '')
      + (p.bad ? ' · 页头损坏' : '');
    c.addEventListener('click', () => openPage(p.no));
    frag.appendChild(c);
  }
  box.appendChild(frag);

  const kindText = pmSel.kind === 'index' ? `索引 ${pmSel.index}` : '数据页';
  if (monEls.pmLegend) {
    monEls.pmLegend.innerHTML = '';
    const parts = pmSel.kind === 'index'
      ? [['hdr', '头页'], ['leaf', '叶子'], ['inner', '内部节点'], ['idx-empty', '空叶']]
      : [['full', '接近满'], ['mid', '中等'], ['sparse', '稀疏'], ['empty', '整页空'], ['hole', '有洞']];
    parts.push(['bad', '坏页']);
    for (const [cls, label] of parts) {
      const item = el('span', 'space-item');
      const dot = el('span', 'pm-c ' + (cls === 'leaf' ? 'idx' : cls));
      dot.style.cursor = 'default';
      item.appendChild(dot);
      item.appendChild(el('span', null, label));
      monEls.pmLegend.appendChild(item);
    }
  }
  if (monEls.mapNote) {
    const shown = pages.length;
    const more = shown < pmMap.total ? ` · 已画 ${shown} 页（${fmtBytes(pmMap.bytes || 0)}）` : ` · 读了 ${fmtBytes(pmMap.bytes || 0)}`;
    monEls.mapNote.textContent = `${pmSel.table} · ${kindText} · 共 ${pmMap.total} 页${more}`;
  }
  if (monEls.pmMore) {
    monEls.pmMore.classList.toggle('hidden', !pmMap.next);
    monEls.pmMore.textContent = `继续扫描（已画 ${pages.length} / ${pmMap.total} 页）`;
  }
}

async function openPage(no) {
  pmOpen = no;
  for (const c of monEls.pmGrid.querySelectorAll('.pm-c.on')) c.classList.remove('on');
  const cell = monEls.pmGrid.querySelector(`.pm-c[data-no="${no}"]`);
  if (cell) {
    cell.classList.add('on');
    cell.scrollIntoView({ block: 'nearest' });
  }
  const side = monEls.pmSide;
  side.innerHTML = '';
  side.appendChild(el('div', 'pm-hint', `正在读第 ${no} 页…`));
  const q = `table=${encodeURIComponent(pmSel.table)}&kind=${pmSel.kind}`
    + (pmSel.index ? `&index=${encodeURIComponent(pmSel.index)}` : '') + `&no=${no}`;
  try {
    const d = await api('/api/page?' + q);
    if (pmOpen !== no) return;
    pmDetail = d;
    renderPageDetail(d);
  } catch (e) {
    side.innerHTML = '';
    side.appendChild(el('div', 'pm-hint', '读取失败：' + e.message));
  }
}

function renderPageDetail(d) {
  const side = monEls.pmSide;
  side.innerHTML = '';
  const head = el('div', 'pm-head',
    `第 ${d.no} 页 · ${d.kind === 'index' ? '索引 ' + d.index : '数据页'}`
    + (d.crc === 'ok' ? '' : ` · ${d.crc === 'zero' ? '空页' : '页头异常'}`));
  side.appendChild(head);

  const kv = el('div', 'pm-kv');
  const addKV = (k, v) => {
    kv.appendChild(el('span', null, k));
    kv.appendChild(el('b', null, v));
  };
  if (d.indexHeader) {
    addKV('根页', `${d.indexHeader.root}`);
    addKV('高度', `${d.indexHeader.height}`);
    addKV('条目', `${fmtCount(d.indexHeader.count)}`);
    addKV('代次', `${d.indexHeader.gen}`);
    if (d.indexHeader.ident) addKV('身份', d.indexHeader.ident);
  } else if (d.data) {
    addKV('槽位', `${d.data.slots}`);
    addKV('空闲', `${d.data.freeHi - d.data.freeLo} B`);
    addKV('LSN', `0x${(d.data.lsn >>> 0).toString(16)}`);
  } else if (d.indexPage) {
    addKV('类型', d.indexPage.leaf ? '叶子' : '内部节点');
    addKV('条目', `${d.indexPage.count}`);
    addKV('左右页', `${d.indexPage.prev} / ${d.indexPage.next}`);
  }
  side.appendChild(kv);

  const tabs = el('div', 'pm-tabs');
  for (const [k, label] of [['struct', '结构'], ['text', '文本'], ['hex', '十六进制']]) {
    const b = el('button', 'pm-tab' + (pmTab === k ? ' on' : ''), label);
    b.type = 'button';
    b.addEventListener('click', () => { pmTab = k; renderPageDetail(d); });
    tabs.appendChild(b);
  }
  side.appendChild(tabs);

  if (pmLink) side.appendChild(pmLinkBox(pmLink));

  const body = el('div', 'pm-tabbody');
  if (pmTab === 'struct') body.appendChild(pmStructView(d));
  else if (pmTab === 'text') body.appendChild(pmTextView(d));
  else body.appendChild(pmHexView(d));
  side.appendChild(body);
}

// 关联链：索引叶子里的 RID → 堆页那一行 → 行内大字段。点 RID 就走这一条。
// 后端 LocateRID 只扫槽位目录、命中即停，同样不进缓冲池。
async function locateRID(rid) {
  const table = pmSel.table;
  let d;
  try {
    d = await api(`/api/rid?table=${encodeURIComponent(table)}&rid=${rid}`);
  } catch (e) {
    pmLink = null;
    if (monEls.pmSide) monEls.pmSide.appendChild(el('div', 'pm-hint', '定位失败：' + e.message));
    return;
  }
  // 从索引视图点过来时，先切到「数据页」并等它扫完，再打开那一页 ——
  // 否则页面还停在索引的网格上，高亮会落空。
  // 注意：selectTarget 会清 pmLink（换目标就不该留着上一页的关联），所以它之后再放回来。
  if (pmSel.kind !== 'data') await selectTarget(table, 'data', '');
  pmLink = d;
  if (pmOpen === d.page && pmDetail) renderPageDetail(pmDetail);
  else await openPage(d.page);
}

// pmLinkBox 是检查器顶上那块「这一项指到哪」。
function pmLinkBox(d) {
  const box = el('div', 'pm-link');
  const head = el('div', 'pm-link-head');
  head.appendChild(el('span', 'pm-link-tag', '关联'));
  head.appendChild(el('span', null,
    `RID ${d.rid} → 堆页 ${d.page} 槽 ${d.slot}` + (d.live ? '' : '（空槽：这一行已被删）')));
  box.appendChild(head);

  if ((d.cols || []).length) box.appendChild(el('div', 'pm-link-row', '列值：' + d.cols.join(' | ')));
  const lobs = d.lobs || [];
  if (!lobs.length) {
    box.appendChild(el('div', 'pm-link-row', '这一行没有大字段'));
  } else {
    for (const l of lobs) {
      const okText = l.headerOk ? '头校验通过' : '头对不上（偏移可能错了）';
      box.appendChild(el('div', 'pm-link-row' + (l.headerOk ? '' : ' bad'),
        `列 ${l.col + 1} · 大字段 ${fmtBytes(l.len)} @${l.off} · ${okText}`
        + (l.preview ? ` · 预览「${l.preview}」` : '')));
    }
    box.appendChild(el('div', 'pm-link-row',
      `.lob 文件共 ${fmtBytes(d.lobFileBytes || 0)}，这一行引用 `
      + `${fmtBytes(lobs.reduce((a, x) => a + x.len, 0))}；`
      + '其余是别的行 —— UPDATE 留下的旧正文也只在这里，只有 VACUUM FULL 能回收'));
  }
  return box;
}

function pmStructView(d) {
  const box = el('div', null);
  if (d.indexHeader) {
    const h = d.indexHeader;
    for (const [k, v] of [['格式版本', h.version], ['根页', h.root], ['高度', h.height],
      ['条目总数', fmtCount(h.count)], ['代次', h.gen], ['身份', h.ident || '—']]) {
      const row = el('div', 'pm-row');
      row.appendChild(el('span', 'pm-no', ''));
      row.appendChild(el('span', 'pm-val', `${k}：${v}`));
      box.appendChild(row);
    }
    return box;
  }
  if (d.data) {
    for (const e of d.data.entries || []) {
      const row = el('div', 'pm-row' + (e.live ? '' : ' dead'));
      if (pmLink && pmLink.rid === e.gid) row.classList.add('on');
      row.appendChild(el('span', 'pm-no', `#${e.no}`));
      const val = el('span', 'pm-val',
        (e.live ? `RID ${e.gid} · ${e.len} B · ` : '空槽 · ')
        + ((e.cols || []).join(' | ') || (e.note || '')));
      row.appendChild(val);
      if (e.live) {
        row.title = `偏移 ${e.off} · 长度 ${e.len} · RID ${e.gid} · 点一下看大字段`;
        row.addEventListener('click', () => locateRID(e.gid));
      }
      box.appendChild(row);
    }
  } else if (d.indexPage) {
    for (const e of d.indexPage.entries || []) {
      const row = el('div', 'pm-row');
      row.appendChild(el('span', 'pm-no', `#${e.i}`));
      const label = e.vals && e.vals.length ? e.vals.join(' | ') : e.key;
      const tail = d.indexPage.leaf ? `RID ${e.val}` : `子页 ${e.val}`;
      const val = el('span', 'pm-val', label + ' → ' + tail);
      row.appendChild(val);
      if (d.indexPage.leaf) {
        row.title = '点一下：跳到这一行在堆页里的位置（并看它挂的大字段）';
        row.classList.add('linkable');
        row.addEventListener('click', () => locateRID(e.val));
      } else {
        row.title = `键 ${e.len} 字节 · 子页 ${e.val}`;
      }
      box.appendChild(row);
    }
  }
  if (!box.children.length) box.appendChild(el('div', 'pm-hint', '这一页里没有条目'));
  return box;
}

function pmTextView(d) {
  const box = el('div', null);
  const runs = d.texts || [];
  if (!runs.length) box.appendChild(el('div', 'pm-hint', '这一页里没找到看起来像文本的片段'));
  for (const t of runs) {
    const row = el('div', 'pm-text');
    row.appendChild(el('span', 'off', '0x' + t.off.toString(16).padStart(4, '0')));
    row.appendChild(el('span', 's', t.text));
    box.appendChild(row);
  }
  return box;
}

// 十六进制：一页 8KB = 512 行，整页渲染（一行 16 字节），命中文本片段的行高亮。
function pmHexView(d) {
  const wrap = el('div', 'pm-hex');
  let bytes;
  try {
    const bin = atob(d.raw || '');
    bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  } catch (e) {
    wrap.textContent = '原始字节解析失败：' + e.message;
    return wrap;
  }
  const runs = (d.texts || []).map(t => [t.off, t.off + t.len]);
  const hex = '0123456789abcdef';
  const lines = [];
  for (let off = 0; off < bytes.length; off += 16) {
    let hx = '';
    let ascii = '';
    for (let i = 0; i < 16; i++) {
      const b = bytes[off + i];
      hx += b === undefined ? '   ' : hex[b >> 4] + hex[b & 15] + ' ';
      ascii += b === undefined ? ' ' : (b >= 32 && b < 127 ? String.fromCharCode(b) : '·');
    }
    lines.push(`${off.toString(16).padStart(4, '0')}  ${hx} |${ascii}|`);
  }
  const frag = document.createDocumentFragment();
  for (let i = 0; i < lines.length; i++) {
    const off = i * 16;
    const hl = runs.some(([a, b]) => off < b && off + 16 > a);
    frag.appendChild(el('div', 'ln' + (hl ? ' hl' : ''), lines[i]));
  }
  wrap.appendChild(frag);
  return wrap;
}

/* ---- 5.2.4 空间地图的整体切换与控件 ---- */

function applyRailWidth() {
  if (!monEls.mapRail) return;
  const w = Math.max(MAP_RAIL_MIN, Math.min(MAP_RAIL_MAX, mapRailW || MAP_RAIL_DEFAULT));
  monEls.mapRail.style.width = `${w}px`;
}

function renderMap() {
  if (!monEls.mapTree || monPageTab !== 'map') return;
  const l2 = mapLevel === 'table';
  monEls.mapBack.classList.toggle('hidden', !l2);
  monEls.mapGroup.classList.toggle('hidden', l2);
  monEls.mapJump.classList.toggle('hidden', !l2);
  monEls.mapFiles.classList.toggle('hidden', !l2 || !pmSel.table);
  monEls.mapTree.classList.toggle('hidden', l2);
  monEls.pmGrid.classList.toggle('hidden', !l2);
  renderZoomHud();
  renderCrumbs();

  if (!l2) {
    if (monEls.mapFiles) monEls.mapFiles.innerHTML = '';
    if (monEls.pmMore) monEls.pmMore.classList.add('hidden');
    renderTreemap();
    renderRailDb();
    renderMapLegend();
    const sp = (spaceData && spaceData.space) || {};
    const total = (sp.dataBytes || 0) + (sp.indexBytes || 0) + (sp.lobBytes || 0) + (sp.walBytes || 0);
    const n = ((spaceData && spaceData.tables) || []).length;
    const noSnap = ((spaceData && spaceData.tables) || []).filter(t => !(t.bytes > 0)).length;
    if (monEls.mapNote) {
      monEls.mapNote.textContent = `${n} 张表 · 合计 ${fmtBytes(total)}`
        + (noSnap ? ` · ${noSnap} 张还没落过盘（不在图上）` : '')
        + ' · 面积 = 字节';
    }
    return;
  }

  renderFileBar();
  if (pmMapMatches()) {
    renderPageMap();
  } else {
    monEls.pmGrid.innerHTML = '';
    if (monEls.pmLegend) monEls.pmLegend.innerHTML = '';
    if (monEls.pmMore) monEls.pmMore.classList.add('hidden');
    if (monEls.mapNote) monEls.mapNote.textContent = '正在扫页头…';
  }
}

function renderMapLegend() {
  if (!monEls.pmLegend) return;
  monEls.pmLegend.innerHTML = '';
  const walOnly = mapGroup === 'file';
  const parts = walOnly
    ? [['s-data', '数据'], ['s-index', '索引'], ['s-lob', '大字段']]
    : [['s-data', '数据'], ['s-index', '索引'], ['s-lob', '大字段']];
  for (const [cls, label] of parts) {
    const item = el('span', 'space-item');
    item.appendChild(el('span', `space-dot ${cls}`));
    item.appendChild(el('span', null, label));
    monEls.pmLegend.appendChild(item);
  }
  const wal = el('span', 'space-item');
  wal.appendChild(el('span', 'space-dot s-wal'));
  wal.appendChild(el('span', null, 'WAL'));
  monEls.pmLegend.appendChild(wal);
}

// 从「存储」页的空间表点过来：直接进那张表的页地图。
function gotoMap(table) {
  mapLevel = 'table';
  mapSelTable = table || '';
  pmSel = { table: table || '', kind: 'data', index: '' };
  pmMap = null;
  pmOpen = -1; pmDetail = null; pmLink = null;
  setMonPage('map');
  if (pmSel.table) {
    railHint('点左边任意一格，看这一页的结构、文本和字节。');
    loadPageMap(false);
  }
}

async function refreshMap() {
  pmCache.clear();
  resetMapZoom();
  await loadSpace();
  if (mapLevel === 'table') await loadPageMap(false);
  else renderMap();
}

if (monEls.mapBack) monEls.mapBack.addEventListener('click', backToDb);
if (monEls.mapRefresh) monEls.mapRefresh.addEventListener('click', refreshMap);
if (monEls.pmMore) monEls.pmMore.addEventListener('click', () => loadPageMap(true));
if (monEls.mapGroup) {
  monEls.mapGroup.addEventListener('click', e => {
    const b = e.target.closest('button[data-group]');
    if (!b) return;
    mapGroup = b.dataset.group;
    for (const x of monEls.mapGroup.querySelectorAll('button')) {
      x.classList.toggle('on', x.dataset.group === mapGroup);
    }
    resetMapZoom();     // 换了分组就是另一张图，缩放没有意义
    renderTreemap();
    renderMapLegend();
  });
}
if (monEls.mapJump) {
  monEls.mapJump.addEventListener('keydown', e => {
    if (e.key !== 'Enter') return;
    const n = parseInt(e.target.value, 10);
    if (!Number.isFinite(n) || n < 0) return;
    e.target.value = '';
    const total = (pmMap && pmMap.total) || 0;
    if (total && n >= total) {
      if (monEls.mapNote) monEls.mapNote.textContent = `这个文件只有 ${total} 页（0 ~ ${total - 1}）`;
      return;
    }
    if (monEls.pmGrid && monEls.pmGrid.querySelector(`.pm-c[data-no="${n}"]`)) openPage(n);
    else if (monEls.mapNote) monEls.mapNote.textContent = `第 ${n} 页还没画出来（点「继续扫描」）`;
  });
}

/* ---- 缩放交互：滚轮（以光标为锚）+ 拖动平移 + 按钮 ---- */

let treeSuppressClick = false;
if (monEls.mapTree) {
  monEls.mapTree.addEventListener('wheel', e => {
    if (monPageTab !== 'map' || mapLevel !== 'db') return;
    e.preventDefault();
    setMapZoom(mapZoom * Math.exp(-e.deltaY * 0.0018), { x: e.clientX, y: e.clientY });
  }, { passive: false });

  let drag = null;
  monEls.mapTree.addEventListener('pointerdown', e => {
    if (mapLevel !== 'db' || mapZoom <= 1 || e.button !== 0) return;
    drag = { sx: e.clientX, sy: e.clientY, px: tvPan.x, py: tvPan.y, moved: false };
    try { monEls.mapTree.setPointerCapture(e.pointerId); } catch (_) { /* 老浏览器 */ }
  });
  monEls.mapTree.addEventListener('pointermove', e => {
    if (!drag) return;
    const dx = e.clientX - drag.sx, dy = e.clientY - drag.sy;
    if (!drag.moved && Math.abs(dx) + Math.abs(dy) < 5) return;
    drag.moved = true;
    monEls.mapTree.classList.add('panning');
    tvPan.x = drag.px + dx;
    tvPan.y = drag.py + dy;
    clampTreePan(monEls.mapTree.clientWidth, monEls.mapTree.clientHeight);
    applyTreePan();
  });
  const endDrag = () => {
    if (!drag) return;
    const moved = drag.moved;
    drag = null;
    monEls.mapTree.classList.remove('panning');
    if (!moved) return;
    // 拖完浏览器还会补一个 click：别让它顺手把这块选中、或者当成双击进表
    treeSuppressClick = true;
    setTimeout(() => { treeSuppressClick = false; }, 0);
  };
  monEls.mapTree.addEventListener('pointerup', endDrag);
  monEls.mapTree.addEventListener('pointercancel', endDrag);
  // 捕获阶段拦：格子自己的 click 在冒泡阶段
  monEls.mapTree.addEventListener('click', e => {
    if (treeSuppressClick) {
      e.stopPropagation();
      e.preventDefault();
    }
  }, true);
}

if (monEls.mapZoomIn) monEls.mapZoomIn.addEventListener('click', () => setMapZoom(mapZoom * 1.5, null));
if (monEls.mapZoomOut) monEls.mapZoomOut.addEventListener('click', () => setMapZoom(mapZoom / 1.5, null));
if (monEls.mapZoomReset) {
  monEls.mapZoomReset.addEventListener('click', () => {
    resetMapZoom();
    if (mapLevel === 'table') renderPageMap();
    else renderTreemap();
  });
}

// 右栏拖宽：认在 localStorage，重开面板还是这个宽度。
(function initRailDrag() {
  const grip = monEls.mapGrip, rail = monEls.mapRail, body = monEls.mapBody;
  if (!grip || !rail || !body) return;
  let startX = 0, startW = 0, dragging = false;
  grip.addEventListener('pointerdown', e => {
    dragging = true;
    startX = e.clientX;
    startW = rail.getBoundingClientRect().width;
    grip.classList.add('drag');
    grip.setPointerCapture(e.pointerId);
    e.preventDefault();
  });
  grip.addEventListener('pointermove', e => {
    if (!dragging) return;
    const limit = Math.max(MAP_RAIL_MIN, Math.min(MAP_RAIL_MAX, body.clientWidth - 320));
    const w = Math.max(MAP_RAIL_MIN, Math.min(limit, startW - (e.clientX - startX)));
    rail.style.width = `${w}px`;
    mapRailW = w;
  });
  const stop = () => {
    if (!dragging) return;
    dragging = false;
    grip.classList.remove('drag');
    try { localStorage.setItem('godb.mapRailW', String(Math.round(mapRailW))); } catch (_) { /* 隐私模式 */ }
    if (mapLevel === 'table') renderPageMap();
    else renderTreemap();
  };
  grip.addEventListener('pointerup', stop);
  grip.addEventListener('pointercancel', stop);
})();

// 窗口尺寸变了要重铺：格子大小和树图面积都是按画布算的。
let mapResizeTimer = 0;
window.addEventListener('resize', () => {
  if (monPageTab !== 'map') return;
  clearTimeout(mapResizeTimer);
  mapResizeTimer = setTimeout(() => {
    if (mapLevel === 'table') renderPageMap();
    else renderTreemap();
  }, 150);
});

/* ---- 5.1 详情：点一行看「到底慢在哪」 ---- */

let slowDetail = null; // { kind: 'sql'|'tx', row, at }

function statCard(k, v, cls, wide) {
  const card = el('div', 'sm-stat' + (wide ? ' wide' : ''));
  card.appendChild(el('div', 'sm-stat-k', k));
  card.appendChild(el('div', 'sm-stat-v' + (cls ? ' ' + cls : ''), v));
  return card;
}

// 只有查询能 EXPLAIN（godb 的限制）：写语句给了按钮点了也是报错，不如不给。
function explainableSQL(s) {
  const t = String(s || '').trim().replace(/^\(+/, '').toUpperCase();
  return t.startsWith('SELECT') || t.startsWith('WITH') || t.startsWith('VALUES');
}

function openSlowDetail(kind, r) {
  if (!r) return;
  const m = monData;
  slowDetail = { kind, row: r, at: m && m.nowMs ? m.nowMs : Date.now() };

  const stats = document.getElementById('smStats');
  const sqlBox = document.getElementById('smSQL');
  const sqlHint = document.getElementById('smSQLHint');
  const sampleWrap = document.getElementById('smSampleWrap');
  const sampleBox = document.getElementById('smSample');
  const planWrap = document.getElementById('smPlanWrap');
  const planBox = document.getElementById('smPlan');
  const planHint = document.getElementById('smPlanHint');
  const hint = document.getElementById('smHint');
  stats.innerHTML = '';
  sqlBox.textContent = '';
  sampleBox.textContent = '';
  planBox.textContent = '';
  planHint.textContent = '';
  hint.textContent = '';
  document.getElementById('smDetHead').innerHTML = '';
  document.getElementById('smDetBody').innerHTML = '';
  document.getElementById('smDetPager').innerHTML = '';
  document.getElementById('smDetailHint').textContent = '';

  // 「多久以前」按快照时刻算，不用 Date.now()：前后端时钟可能差几秒，
  // 混用会出现「3 秒前发生的，显示成 2 秒前」。
  const agoMs = v => (slowDetail.at && v ? Math.max(0, slowDetail.at - v) : 0);

  if (kind === 'sql') {
    document.getElementById('smTitle').textContent = '慢 SQL 详情';
    sqlHint.textContent = ' 参数已换成 ?，同结构的执行合并成这一行';
    sqlBox.textContent = r.sql || '—';
    const sample = r.sample || '';
    sampleWrap.classList.toggle('hidden', !sample || sample === r.sql);
    sampleBox.textContent = sample;

    stats.appendChild(statCard('执行次数', r.running ? '执行中' : fmtCount(r.n)));
    stats.appendChild(statCard('累计耗时', fmtNs(r.totalNs)));
    stats.appendChild(statCard('平均', r.running ? '—' : fmtNs(r.avgNs)));
    stats.appendChild(statCard('最慢一次', fmtNs(r.maxNs), r.maxNs / 1e6 >= 200 ? 'warn' : ''));
    if (!r.running) {
      stats.appendChild(statCard('最快一次', fmtNs(r.minNs)));
      stats.appendChild(statCard('首次执行', fmtAgo(agoMs(r.firstMs))));
      stats.appendChild(statCard('最近执行', fmtAgo(agoMs(r.lastMs))));
    }
    stats.appendChild(statCard('涉及表', (r.tables || []).join(', ') || '—'));
    stats.appendChild(statCard('读写', r.write ? '写' : '读'));

    // EXPLAIN 得用原文：归一化后的 ? 直接拿去跑是语法错误
    const canExplain = !r.write && !r.running && explainableSQL(sample || r.sql);
    document.getElementById('btnSlowExplain').classList.toggle('hidden', !canExplain);
    hint.textContent = r.running
      ? '这条还在执行，明细要等它结束之后才有。'
      : '同一个结构只占一行统计，执行明细在下面 —— 看「时刻」能分辨是偶发还是持续。';
  } else {
    document.getElementById('smTitle').textContent = '慢事务详情';
    document.getElementById('btnSlowExplain').classList.add('hidden');
    sqlHint.textContent = ' 事务按「涉及的表集合 + 读写」分组';
    sqlBox.textContent = (r.write ? '写事务' : '只读事务') + ' · ' +
      ((r.tables || []).join(', ') || '(没有碰表)');
    sampleWrap.classList.add('hidden');

    stats.appendChild(statCard('提交次数', fmtCount(r.n)));
    stats.appendChild(statCard('累计时长', fmtMs(r.totalMs)));
    stats.appendChild(statCard('平均', fmtMs(r.avgMs)));
    stats.appendChild(statCard('最长一次', fmtMs(r.maxMs), r.maxMs >= 3000 ? 'warn' : ''));
    stats.appendChild(statCard('最短一次', fmtMs(r.minMs)));
    stats.appendChild(statCard('首次出现', fmtAgo(agoMs(r.firstMs))));
    stats.appendChild(statCard('最近一次', fmtAgo(agoMs(r.lastMs))));
    hint.textContent = '时长是从 BEGIN 到 COMMIT/ROLLBACK 的总时间，含等锁和空闲 —— ' +
      '「挂起不提交」也会记在这里。';
  }

  document.getElementById('slowModal').classList.remove('hidden');
  loadSlowDetail(kind, kind === 'sql' ? r.sql : r.key);
}

/* ---- 5.2 明细：点开之后才去拿 ---- */
const SM_PAGE = 10;
let smDetail = null; // { kind, key, rows, page }

async function loadSlowDetail(kind, key) {
  const hint = document.getElementById('smDetailHint');
  smDetail = { kind, key, rows: [], page: 0 };
  hint.textContent = ' 加载中…';

  let data = null;
  try {
    const res = await fetch('/api/monitor/detail', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ kind, key }),
    });
    data = await res.json();
    if (!res.ok) {
      hint.textContent = ' ' + ((data && data.error) || '拿不到明细');
      return;
    }
  } catch (e) {
    hint.textContent = ' 拿不到明细（连接断开）';
    return;
  }
  // 拉的过程中用户可能已经点了别的一行
  if (!smDetail || smDetail.key !== key) return;

  smDetail.rows = data.details || [];
  // 计划只留最近一次慢于 5ms 的那份，比列表里每次执行都存计划省得多
  if (data.plan && data.plan.length) {
    document.getElementById('smPlan').textContent = data.plan.join('\n');
    document.getElementById('smPlanHint').textContent = '（最近一次慢于 5ms 的执行的计划）';
    document.getElementById('smPlanWrap').classList.remove('hidden');
  }
  document.getElementById('smDetCap').textContent = String(smDetail.rows.length);
  hint.textContent = smDetail.rows.length ? ' 按耗时降序' : ' 还没有明细';
  renderSmDetail();
}

function renderSmDetail() {
  if (!smDetail) return;
  const head = document.getElementById('smDetHead');
  const body = document.getElementById('smDetBody');
  const pager = document.getElementById('smDetPager');
  head.innerHTML = '';
  body.innerHTML = '';
  pager.innerHTML = '';

  const isSQL = smDetail.kind === 'sql';
  const cols = isSQL
    ? [['c-num', '耗时'], ['c-num', '扫描'], ['c-num', '返回'], ['c-sess', '会话'], ['c-num', '时刻']]
    : [['c-num', '时长'], ['c-num', '语句数'], ['c-sess', '会话'], ['c-num', '时刻']];
  for (const pair of cols) head.appendChild(el('th', pair[0], pair[1]));

  const rows = smDetail.rows;
  const pages = Math.max(1, Math.ceil(rows.length / SM_PAGE));
  if (smDetail.page > pages - 1) smDetail.page = pages - 1;
  if (smDetail.page < 0) smDetail.page = 0;
  const start = smDetail.page * SM_PAGE;
  const at = slowDetail ? slowDetail.at : Date.now();

  rows.slice(start, start + SM_PAGE).forEach(d => {
    const tr = el('tr');
    if (isSQL) {
      tr.appendChild(el('td', 'c-num', fmtNs(d.tookNs)));
      tr.appendChild(el('td', 'c-num', d.scanned > 0 ? fmtCount(d.scanned) : '—'));
      const out = d.rows > 0 ? d.rows : (d.affected > 0 ? -d.affected : 0);
      tr.appendChild(el('td', 'c-num', out === 0 ? '—' : fmtCount(Math.abs(out))));
    } else {
      tr.appendChild(el('td', 'c-num', fmtMs(d.durMs)));
      tr.appendChild(el('td', 'c-num', fmtCount(d.stmts || 0)));
    }
    tr.appendChild(el('td', 'c-sess mono', sessionShort(d.session)));
    tr.appendChild(el('td', 'c-num', d.atMs ? fmtAgo(Math.max(0, at - d.atMs)) : '—'));
    body.appendChild(tr);
  });

  if (rows.length > SM_PAGE) {
    pager.appendChild(el('span', 'pg-info',
      `第 ${smDetail.page + 1}/${pages} 页 · 共 ${rows.length} 次`));
    const mk = (label, disabled, delta) => {
      const b = el('button', 'pg-btn', label);
      b.type = 'button';
      b.disabled = disabled;
      b.addEventListener('click', () => {
        smDetail.page += delta;
        renderSmDetail();
      });
      pager.appendChild(b);
    };
    mk('‹ 上一页', smDetail.page <= 0, -1);
    mk('下一页 ›', smDetail.page >= pages - 1, +1);
  }
}

function closeSlowDetail() {
  document.getElementById('slowModal').classList.add('hidden');
  smDetail = null;
}

// 「3 分钟前」这类相对时间：详情里比绝对时间更好读。
// 每行耗时：小到 0.001ms 时改报微秒 —— 一律 toFixed(2) 会全变成 0.00，
// 而「每行 0.8µs」和「每行 0.05µs」是可以拿来对比的。
function fmtPerRow(tookMs, rows) {
  const per = tookMs / rows;
  if (per >= 1) return per.toFixed(1) + ' ms';
  if (per >= 0.01) return per.toFixed(2) + ' ms';
  if (per >= 0.00001) return (per * 1000).toFixed(1) + ' µs';
  return '<0.01 µs';
}

function fmtAgo(ms) {
  if (ms < 1000) return '刚刚';
  if (ms < 60000) return Math.round(ms / 1000) + ' 秒前';
  if (ms < 3600000) return Math.round(ms / 60000) + ' 分钟前';
  return Math.round(ms / 3600000) + ' 小时前';
}

function slowDetailSQL() {
  if (!slowDetail) return '';
  const r = slowDetail.row;
  return r.sample || r.sql || '';
}

document.getElementById('btnSlowDone').addEventListener('click', closeSlowDetail);
document.getElementById('btnSlowClose').addEventListener('click', closeSlowDetail);
document.getElementById('btnSlowEditor').addEventListener('click', () => {
  const sql = slowDetailSQL();
  if (!sql) return;
  editor.value = sql;
  closeSlowDetail();
  toggleMonitor(false);
});
document.getElementById('btnSlowExplain').addEventListener('click', () => {
  const sql = slowDetailSQL();
  if (!sql) return;
  closeSlowDetail();
  explainSQL(sql);
});

/* ---- 6. 超时开关 ---- */

const TIMEOUT_FIELDS = [
  { key: 'slowSqlSec', name: '慢 SQL 超时', tip: '单条语句跑超过它就回滚所在事务（引擎不支持中途打断，是跑完再回滚）' },
  { key: 'slowTxIdleSec', name: '慢事务超时', tip: '事务空闲（距最后一条 SQL）超过它就回滚，兜住「SQL 都快但不提交」' },
  { key: 'lockWaitSec', name: '等锁超时', tip: '等表锁超过它放弃整个事务；不限时只靠死锁检测兜底' },
  { key: 'queueWaitSec', name: '排队超时', tip: '排队等执行槽位超过它就快速失败（背压）' },
];

function renderTimeouts(t) {
  // 只在第一次构建结构，之后只refresh 状态文字：每 2 秒重建输入框会把
  // 正在输入的数字和光标一起冲掉。
  if (!monTimeoutBuilt) {
    monEls.timeouts.innerHTML = '';
    for (const f of TIMEOUT_FIELDS) {
      const row = el('div', 'to-row');
      row.appendChild(el('span', 'to-name', f.name)).title = f.tip;

      const inp = el('input', 'to-input');
      inp.type = 'number';
      inp.min = '0';
      inp.max = '86400';
      inp.step = '1';
      inp.placeholder = '不限';
      inp.id = 'to-' + f.key;
      inp.setAttribute('aria-label', f.name + '（秒）');
      inp.addEventListener('input', () => { monEls.timeoutHint.textContent = ''; });
      row.appendChild(inp);

      row.appendChild(el('span', 'to-unit', '秒'));
      row.appendChild(el('span', 'to-state', ''));
      monEls.timeouts.appendChild(row);
    }
    monTimeoutBuilt = true;
  }
  if (monSavingTimeouts) return; // 保存中不要覆盖用户刚输入的值
  for (const f of TIMEOUT_FIELDS) {
    const inp = document.getElementById('to-' + f.key);
    if (!inp) continue;
    const v = t[f.key] || 0;
    // 用户正在改这一项时不要抢他的输入
    if (document.activeElement !== inp) inp.value = v > 0 ? String(v) : '';
    const row = inp.parentNode;
    const st = row.querySelector('.to-state');
    st.textContent = v > 0 ? '已开启' : '⚠ 不限';
    st.className = 'to-state ' + (v > 0 ? 'on' : 'off');
  }
}

async function saveTimeouts() {
  if (monSavingTimeouts) return;
  const body = {};
  for (const f of TIMEOUT_FIELDS) {
    const inp = document.getElementById('to-' + f.key);
    const raw = inp ? parseInt(inp.value, 10) : 0;
    const v = isNaN(raw) || raw < 0 ? 0 : Math.min(raw, 86400);
    body[f.key] = v;
  }
  monSavingTimeouts = true;
  monEls.timeoutHint.textContent = '保存中…';
  try {
    const r = await api('/api/monitor/timeouts', body);
    if (r && r.error) {
      monEls.timeoutHint.textContent = r.error;
      return;
    }
    monEls.timeoutHint.textContent = r && r.persisted
      ? '已保存到 config.json 并立即生效'
      : '已生效（本次运行有效，没有配置文件可写）';
    if (monData) monData.timeouts = body;
    renderTimeouts(body);
    toast('超时设置已应用');
  } catch (e) {
    monEls.timeoutHint.textContent = '保存失败：' + e.message;
  } finally {
    monSavingTimeouts = false;
  }
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
    // 事务打开是「有事发生」，药丸必须露出来（它挂着持锁数、未提交行数）
    txPill.classList.remove('hidden');
  } else {
    txBar.classList.add('hidden');
    // 「空闲」是默认状态，顶栏圆点绿色就等于说完了，留着只是占地方。
    // 药丸只在真的有事（事务中 / 已中止）时才出现。
    txPill.textContent = '空闲';
    txPill.className = 'pill pill-idle';
    txPill.classList.add('hidden');
  }
  lastTx = { active: !!tx.active, aborted: !!tx.aborted, changes: tx.changes || 0 };
  syncTopDot();
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

// 窄屏「更多」菜单：执行计划 / 清空 在 ≤720px 被藏起来，这里给一条出路。
(function initMoreMenu() {
  const btn = document.getElementById('btnMore');
  const menu = document.getElementById('moreMenu');
  const setOpen = on => {
    menu.classList.toggle('hidden', !on);
    btn.setAttribute('aria-expanded', on ? 'true' : 'false');
  };
  btn.addEventListener('click', ev => {
    ev.stopPropagation();
    setOpen(menu.classList.contains('hidden'));
  });
  menu.addEventListener('click', ev => {
    const act = ev.target.closest('button')?.dataset.act;
    if (!act) return;
    setOpen(false);
    document.getElementById(act === 'explain' ? 'btnExplain' : 'btnClear').click();
  });
  document.addEventListener('click', ev => {
    if (!menu.classList.contains('hidden') && !ev.target.closest('#moreMenu, #btnMore')) setOpen(false);
  });
  document.addEventListener('keydown', ev => {
    if (ev.key === 'Escape' && !menu.classList.contains('hidden')) setOpen(false);
  });
  // 转回宽屏时菜单要收起来，否则按钮回来了、菜单还飘着
  window.addEventListener('resize', () => {
    if (window.innerWidth > 720) setOpen(false);
  });
})();
document.getElementById('btnFormat').addEventListener('click', formatSQL);
// 搜索表名：图标按钮展开输入框，边打边过滤；Esc 收起并清空。
document.getElementById('btnTableSearch').addEventListener('click', () => {
  const sidebar = document.querySelector('.sidebar');
  const wrap = document.getElementById('sbSearchWrap');
  const on = !sidebar.classList.contains('searching');
  sidebar.classList.toggle('searching', on);
  wrap.classList.toggle('hidden', !on);
  if (on) {
    document.getElementById('sbSearch').focus();
    document.getElementById('sbSearch').select();
  } else {
    setSchemaQuery('');
  }
});

function setSchemaQuery(q) {
  schemaQuery = q;
  document.getElementById('sbSearch').value = q;
  const sb = document.querySelector('.sidebar');
  if (!q) {
    sb.classList.remove('searching');
    document.getElementById('sbSearchWrap').classList.add('hidden');
  }
  renderSchema(lastSchemaTables);
}

document.getElementById('sbSearch').addEventListener('input', ev => {
  schemaQuery = ev.target.value;
  renderSchema(lastSchemaTables);
});
document.getElementById('sbSearch').addEventListener('keydown', ev => {
  if (ev.key === 'Escape') {
    ev.stopPropagation();
    setSchemaQuery('');
  }
});
document.getElementById('sbSearchClear').addEventListener('click', () => {
  setSchemaQuery('');
  document.getElementById('sbSearch').focus();
});

document.getElementById('btnRefresh').addEventListener('click', loadSchema);
document.getElementById('btnMonitor').addEventListener('click', () => toggleMonitor());
document.getElementById('btnMonClose').addEventListener('click', () => toggleMonitor(false));
document.getElementById('btnMonRefresh').addEventListener('click', () => {
  if (monPaused) setMonPaused(false); // 手动刷新 = 隐含继续
  else pollMonitor();
});
document.getElementById('btnMonPause').addEventListener('click', () => setMonPaused(!monPaused));
document.getElementById('btnMonSaveTimeouts').addEventListener('click', saveTimeouts);

// 齿轮：点开 / 再点收起；点浮层外面也收起（点到输入框里不算外面）
document.getElementById('btnMonSettings').addEventListener('click', (ev) => {
  ev.stopPropagation();
  setMonSettings(!monSettingsOpen());
});
document.getElementById('btnMonSetClose').addEventListener('click', () => setMonSettings(false));
monPanel.addEventListener('click', (ev) => {
  if (!monSettingsOpen()) return;
  if (monEls.setPop.contains(ev.target) || monEls.setBtn.contains(ev.target)) return;
  setMonSettings(false);
});

// 诊断区 Tab
monEls.tabs.addEventListener('click', (ev) => {
  const b = ev.target.closest('.mon-tab');
  if (b) setMonTab(b.dataset.tab, true);
});
document.getElementById('btnCommit').addEventListener('click', () => txAction('commit'));
document.getElementById('btnRollback').addEventListener('click', () => txAction('rollback'));

/* ---------------- 危险操作确认（手打库名） ---------------- */

// 当前库名：徽标上的文本就是服务端 /api/dbs 返回的 current。
function currentDBName() {
  return ((document.getElementById('dbBadge') || {}).textContent || '').trim();
}

// 两次 confirm() 在「手快、误双击、赶时间改配置」的时候基本等于没拦 ——
// 而且它是浏览器原生弹框，视觉上和别的东西没区别（2026-10-28 那次就是这么丢的表）。
// 手打库名要眼睛看着键盘一个个敲，几乎不可能顺手完成，这是 GitHub 删仓库那套。
//
//   confirmDanger({ title, desc, items:[{name, meta}], word, okText }) -> Promise<boolean>
function confirmDanger(opt) {
  const modal = document.getElementById('confirmModal');
  const input = document.getElementById('cfInput');
  const okBtn = document.getElementById('cfOk');
  const hint = document.getElementById('cfHint');

  document.getElementById('cfTitle').textContent = opt.title || '危险操作';
  document.getElementById('cfDesc').textContent = opt.desc || '';
  document.getElementById('cfWord').textContent = opt.word;
  okBtn.textContent = opt.okText || '确认';

  const list = document.getElementById('cfList');
  list.innerHTML = '';
  const items = opt.items || [];
  for (const it of items) {
    const row = el('div', 'cf-item');
    row.appendChild(el('span', 'cf-name', it.name));
    row.appendChild(el('span', 'cf-meta', it.meta || ''));
    list.appendChild(row);
  }
  list.classList.toggle('hidden', items.length === 0);

  input.value = '';
  input.placeholder = opt.word;
  input.classList.remove('ok');
  hint.textContent = '';
  hint.className = 'modal-hint';
  okBtn.disabled = true;

  modal.classList.remove('hidden');
  input.focus();

  return new Promise(resolve => {
    let done = false;
    const finish = v => {
      if (done) return;
      done = true;
      modal.classList.add('hidden');
      resolve(v);
    };
    const sync = () => {
      const hit = input.value.trim() === opt.word;
      okBtn.disabled = !hit;
      input.classList.toggle('ok', hit);
      if (hit) hint.textContent = '';
    };
    input.oninput = sync;
    input.onkeydown = e => {
      if (e.key === 'Enter') {
        e.preventDefault();
        e.stopPropagation();
        if (!okBtn.disabled) finish(true);
      } else if (e.key === 'Escape') {
        e.preventDefault();
        // 别让全局 Esc 处理器顺手把底下的设置页也关掉
        e.stopPropagation();
        finish(false);
      }
    };
    okBtn.onclick = () => { if (!okBtn.disabled) finish(true); };
    document.getElementById('cfCancel').onclick = () => finish(false);
    document.getElementById('cfClose').onclick = () => finish(false);
    // 点遮罩也算取消：手滑点到外面不该有任何后果
    modal.onclick = e => { if (e.target === modal) finish(false); };
  });
}

// 把当前库的表整理成确认框里的清单（行数取自 schemaCache）
function schemaItems() {
  return schemaCache.map(t => ({ name: t.name, meta: (t.rows || 0) + ' 行' }));
}

// 这两个原来是顶栏按钮，现在收在设置页里（低频 + 一个会清库），实现不变
async function loadSample() {
  const word = currentDBName() || 'godb';
  const n = schemaCache.length;
  const ok = await confirmDanger({
    title: '载入示例数据',
    desc: n
      ? `当前库「${word}」里的 ${n} 张表会被删除，然后重建 departments / employees 并灌入示例数据。`
      : `当前库「${word}」是空的，将创建 departments / employees 并灌入示例数据。`,
    items: schemaItems(),
    word,
    okText: '清空并重建',
  });
  if (!ok) return;
  try {
    await api('/api/sample', {});
    await loadSchema();
    editor.value = EXAMPLES.select;
    autoSize();
    run();
    closeSettings();
  } catch (e) {
    alert('载入失败：' + e.message);
  }
}

async function resetDatabase() {
  const cur = currentDBName() || 'godb';
  const n = schemaCache.length;
  const ok = await confirmDanger({
    title: '清空当前库',
    desc: n
      ? `「${cur}」里的 ${n} 张表会被全部删除。表结构和数据先进回收站，可以用命令行找回。`
      : `「${cur}」里现在没有任何表。`,
    items: schemaItems(),
    word: cur,
    okText: '清空',
  });
  if (!ok) return;
  try {
    await api('/api/reset', {});
    await loadSchema();
    clearResults('数据库已清空');
    updateTx({ active: false });
    closeSettings();
  } catch (e) {
    alert('清空失败：' + e.message);
  }
}

/* ---------------- 帮助（示例查询 / 支持的语法 / 快捷键） ---------------- */

const helpModal = document.getElementById('helpModal');
const helpExamples = document.getElementById('helpExamples');

// 示例的标题和一句说明。SQL 本体仍然只在 EXAMPLES 里存一份，别抄两遍。
const EXAMPLE_META = [
  ['select', '全部员工', '最简单的 SELECT，先看清表里有什么'],
  ['group', '部门统计', 'GROUP BY + 聚合，LEFT JOIN 把没有人的部门也带上'],
  ['join', '左连接', '没有部门的员工也会出现（部门列为 NULL）'],
  ['tx', '事务演示', 'BEGIN 后改数据，先看效果再决定提交还是回滚'],
  ['ddl', '新建表', 'SERIAL 主键、UNIQUE、DEFAULT now() 一次看全'],
  ['explain', '看执行计划', 'EXPLAIN 看优化器走索引还是全表扫'],
  ['alter', '改表结构', '加列再删列，DDL 立即生效'],
  ['funcs', '函数与 interval', 'random / lpad / unnest / 时间加减 / 库大小，不依赖示例表'],
];

function renderHelpExamples() {
  helpExamples.innerHTML = '';
  for (const [key, name, note] of EXAMPLE_META) {
    const b = el('button', 'help-ex-item');
    b.type = 'button';
    b.dataset.ex = key;
    b.appendChild(el('span', 'help-ex-name', name));
    b.appendChild(el('span', 'help-ex-note', note));
    helpExamples.appendChild(b);
  }
}

const helpDataHint = document.getElementById('helpDataHint');
const btnHelpSample = document.getElementById('btnHelpSample');

// 手册里「读」的示例要求当前库有示例表。别人打开时库可能是空的（新建的库、
// 或刚清空过），那时示例会全报 relation "employees" does not exist —— 与其让人猜，
// 不如在页脚直接说清当前状态，并把「补齐」的按钮放在手边。
function updateHelpDataHint() {
  const names = new Set(schemaCache.map(t => t.name));
  const ok = names.has('departments') && names.has('employees');
  helpDataHint.textContent = ok
    ? '示例数据已就绪（departments / employees）· 补全按当前库的真实表和列提示'
    : '当前库没有示例表，上面「读」的示例会报 relation does not exist';
  btnHelpSample.classList.toggle('hidden', ok);
}

async function openHelp() {
  renderHelpExamples();
  updateHelpDataHint();          // 先按缓存给个即时反馈
  helpModal.classList.remove('hidden');
  // #help 深链一进页面就打开帮助时，schemaCache 还没加载完，直接判「没有示例表」
  // 会误报（实测截图两张完全一样就是踩在这个时机上）。补拉一次再校正。
  try { await loadSchema(); } catch (e) {}
  updateHelpDataHint();
}

// 在帮助里当场补齐示例数据。这里不复用 loadSample()：那个会顺手把示例 SQL
// 填进编辑器并执行，而用户此刻只是想看手册。
//
// 注意这个按钮在「缺示例表」时就会露出来，库里可能有自己的表 —— 那种情况下
// /api/sample 会把它们全清掉，所以必须走手打库名那一套。
btnHelpSample.addEventListener('click', async () => {
  const word = currentDBName() || 'godb';
  const n = schemaCache.length;
  if (n > 0) {
    const ok = await confirmDanger({
      title: '补齐示例数据',
      desc: `当前库「${word}」里有 ${n} 张表，补齐示例会先全部删除，再重建 departments / employees。`,
      items: schemaItems(),
      word,
      okText: '清空并重建',
    });
    if (!ok) return;
  } else if (!confirm(`将在空的「${word}」里创建示例表 departments / employees，继续？`)) {
    return;
  }
  try {
    await api('/api/sample', {});
    await loadSchema();
    updateHelpDataHint();
  } catch (e) {
    alert('载入失败：' + e.message);
  }
});

function closeHelp() {
  helpModal.classList.add('hidden');
}

document.getElementById('btnHelp').addEventListener('click', openHelp);
document.getElementById('btnHelpClose').addEventListener('click', closeHelp);
document.getElementById('btnHelpDone').addEventListener('click', closeHelp);
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !helpModal.classList.contains('hidden')) closeHelp();
});

// 帮助手册的目录跳转。手册按由浅入深分了 10 级 + 不支持，篇幅较长，
// 光靠滚轮找很累。用事件委托（内容虽然当前是静态的，但保持和别的弹窗一致）。
// scrollIntoView 会自己找最近的滚动祖先 —— 这里就是 modal-body。
document.addEventListener('click', ev => {
  const go = ev.target.closest('[data-goto]');
  if (!go) return;
  const target = document.getElementById(go.dataset.goto);
  if (target && target.scrollIntoView) target.scrollIntoView({ block: 'start' });
});

// 点示例只填进编辑器、不自动执行：避免手一滑就把库改了
helpExamples.addEventListener('click', ev => {
  const item = ev.target.closest('.help-ex-item');
  if (!item) return;
  editor.value = EXAMPLES[item.dataset.ex] || '';
  autoSize();
  closeHelp();
  editor.focus();
});

// 编辑器变更后：跟随内容长高 + 记到本机（关掉浏览器再开还在）
function onEditorChange() {
  autoSize();
  localStorage.setItem('godb.sql', editor.value);
}

// 原生 textarea 的兜底快捷键（CodeMirror 没起来时才用得上）
editorEl.addEventListener('keydown', ev => {
  if ((ev.ctrlKey || ev.metaKey) && ev.key === 'Enter') {
    ev.preventDefault();
    run();
    return;
  }
  // Ctrl+Shift+F：美化。注意 Shift 会让 ev.key 变成 'F'
  if ((ev.ctrlKey || ev.metaKey) && ev.shiftKey && (ev.key === 'F' || ev.key === 'f')) {
    ev.preventDefault();
    formatSQL();
    return;
  }
  if (ev.key === 'Tab') {
    ev.preventDefault();
    const start = editorEl.selectionStart;
    const end = editorEl.selectionEnd;
    editorEl.value = editorEl.value.slice(0, start) + '  ' + editorEl.value.slice(end);
    editorEl.selectionStart = editorEl.selectionEnd = start + 2;
    onEditorChange();
  }
});
editorEl.addEventListener('input', onEditorChange);


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

function closeSettings(force) {
  // 有未保存的改动就拦一下：这些输入框在五个分组里，随手点 × 就丢得没痕迹。
  // 保存成功走的是 closeSettings(true)，不该被自己拦。
  if (!force) {
    const n = dirtyIds().length + (setClearPw ? 1 : 0);
    if (n && !confirm('有 ' + n + ' 处改动还没保存，确定关闭？')) return;
  }
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

/* ---- 设置：五个分组 ----
   分组只解决「找得到」，保存仍然是一次整体提交。切组不重新渲染 DOM，
   否则在「连接」里改了一半再去看「性能」，回来改动就丢了。 */
const SET_PANES = [
  { id: 'conn', name: '连接', render: renderConnPane },
  { id: 'sec', name: '安全', render: renderSecPane },
  { id: 'perf', name: '性能', render: renderPerfPane },
  { id: 'look', name: '外观', render: renderLookPane },
  { id: 'data', name: '数据', render: renderDataPane },
];

const settingsTabs = document.getElementById('settingsTabs');
const settingsDirty = document.getElementById('settingsDirty');
let setPane = 'conn';
let setClearPw = false; // 点了「清除」：保存时把已有密码清掉

function buildSettingsTabs() {
  settingsTabs.innerHTML = '';
  for (const p of SET_PANES) {
    const b = el('button', 'set-tab');
    b.type = 'button';
    b.dataset.pane = p.id;
    b.setAttribute('role', 'tab');
    b.textContent = p.name;
    b.addEventListener('click', () => setSettingsPane(p.id));
    settingsTabs.appendChild(b);
  }
}

function setSettingsPane(id) {
  if (!SET_PANES.some(p => p.id === id)) id = 'conn';
  setPane = id;
  for (const b of settingsTabs.children) {
    const on = b.dataset.pane === id;
    b.classList.toggle('on', on);
    b.setAttribute('aria-selected', String(on));
  }
  for (const box of settingsBody.querySelectorAll('.set-pane')) {
    box.classList.toggle('hidden', box.dataset.pane !== id);
  }
  settingsBody.scrollTop = 0;
  refreshFooter();
}

// dirtyIds 返回「和打开时不一样」的输入框数量依据。
// 只比 value/checked，不比 disabled —— PG 端口灰着的时候值仍然要能提交。
function dirtyIds() {
  const out = [];
  for (const n of settingsBody.querySelectorAll('input')) {
    if (n.dataset.init === undefined) continue;
    const cur = n.type === 'checkbox' ? String(n.checked) : n.value;
    if (cur !== n.dataset.init) out.push(n.id || n.name || 'input');
  }
  return out;
}

// invalidInputs 把超出 min/max 的输入框标红并返回，用来挡住「端口填了 70000 再点保存」。
function invalidInputs() {
  const bad = [];
  for (const n of settingsBody.querySelectorAll('input[type="number"]')) {
    if (n.disabled) {
      n.classList.remove('bad');
      continue;
    }
    const v = n.value.trim();
    const num = Number(v);
    const ok = v !== '' && Number.isFinite(num) &&
      (n.min === '' || num >= Number(n.min)) &&
      (n.max === '' || num <= Number(n.max));
    n.classList.toggle('bad', !ok);
    if (!ok) bad.push(n);
  }
  return bad;
}

function refreshDirty() {
  const bad = invalidInputs();
  const n = dirtyIds().length + (setClearPw ? 1 : 0);
  if (bad.length) {
    settingsDirty.textContent = bad.length + ' 处输入不合法';
    settingsDirty.className = 'modal-hint err';
  } else if (n) {
    settingsDirty.textContent = n + ' 处未保存';
    settingsDirty.className = 'modal-hint dirty';
  } else {
    settingsDirty.textContent = '';
    settingsDirty.className = 'modal-hint';
  }
  const btn = document.getElementById('btnSettingsSave');
  btn.disabled = bad.length > 0 || !settingsData || !settingsData.runtime.canEdit;
  refreshFooter();
}

// 「外观」是浏览器本地偏好、不进保存，所以这一页没有改动时底部按钮就是「关闭」；
// 如果还压着别处的改动，按钮仍然是「保存并应用」，别让人以为没处可存。
function refreshFooter() {
  const btn = document.getElementById('btnSettingsSave');
  const dirty = dirtyIds().length + (setClearPw ? 1 : 0);
  if (setPane === 'look' && dirty === 0 && settingsData && settingsData.runtime.canEdit) {
    btn.textContent = '关闭';
    btn.dataset.mode = 'close';
  } else {
    btn.textContent = '保存并应用';
    btn.dataset.mode = 'save';
  }
}

function statLine(k, v) {
  const s = el('span', '', k + ' ');
  s.appendChild(el('b', '', v));
  return s;
}

/* ---- 连接：账号、端口、监听 ---- */
function renderConnPane(box, d, rt) {
  box.appendChild(el('div', 'set-lead',
    '账号和端口都属于「怎么连进来」，改完立即生效。'));

  box.appendChild(setRow('用户名', textInput('setAuthUser', d.auth.user, 'godb')));

  const pwWrap = el('div', 'pw-wrap');
  const pw = textInput('setAuthPassword', '', d.auth.hasPassword ? '已设置，留空则不修改' : '设置密码', 'password');
  pwWrap.appendChild(pw);
  if (d.auth.hasPassword) {
    pwWrap.appendChild(el('span', 'set-badge ok', '已设置'));
    const clearBtn = el('button', 'set-linkbtn', '清除');
    clearBtn.type = 'button';
    clearBtn.title = '保存时清掉已有密码';
    clearBtn.addEventListener('click', () => {
      setClearPw = !setClearPw;
      clearBtn.textContent = setClearPw ? '取消清除' : '清除';
      clearBtn.classList.toggle('on', setClearPw);
      pw.disabled = setClearPw;
      pw.value = '';
      refreshDirty();
    });
    pwWrap.appendChild(clearBtn);
  }
  box.appendChild(setRow('密码', pwWrap));
  box.appendChild(el('div', 'set-hint',
    '密码以 PBKDF2-SHA256 哈希存在配置文件里，接口不会回显；留空表示保持不变。'));

  box.appendChild(el('div', 'set-sec', '端口'));
  box.appendChild(setRow('控制台端口', numInput('setHTTPPort', d.network.httpPort, 1, 65535),
    '当前 ' + (rt.httpAddr || '—')));
  box.appendChild(setRow('PostgreSQL', checkInput('setPGEnabled', d.network.pgEnabled), '启用 PG 协议端口'));
  const pgPort = numInput('setPGPort', d.network.pgPort, 1, 65535);
  pgPort.disabled = !d.network.pgEnabled;
  box.appendChild(setRow('PG 端口', pgPort, '当前 ' + (rt.pgAddr || '未启用')));
  box.appendChild(el('div', 'set-hint',
    '改控制台端口后要用新地址重新打开页面；改 PG 端口会断开现有客户端，重连即可。'));

  const pgEnabled = box.querySelector('#setPGEnabled');
  pgEnabled.addEventListener('change', () => {
    pgPort.disabled = !pgEnabled.checked;
    refreshDirty();
  });
}

/* ---- 安全：暴露面都放这一组 ---- */
function renderSecPane(box, d, rt) {
  box.appendChild(el('div', 'set-lead', '这里的开关决定「谁能连进来」，改完立即生效。'));

  box.appendChild(setRow('必须密码', checkInput('setRequirePw', d.auth.requirePassword),
    '连接 PG 时必须输入密码'));
  box.appendChild(setRow('控制台远程', checkInput('setHTTPRemote', d.network.httpRemote),
    '允许其它计算机访问网页控制台'));
  box.appendChild(setRow('PG 远程', checkInput('setPGRemote', d.network.pgRemote),
    '允许其它计算机连接 PG 端口'));
  box.appendChild(el('div', 'set-hint',
    '当前监听：控制台 ' + (rt.httpAddr || '—') + '，PG ' + (rt.pgAddr || '未启用') +
    '。关掉远程时只绑定 127.0.0.1。'));

  box.appendChild(el('div', 'set-warn',
    '网页控制台本身不登录：能访问控制台的人可以执行任意 SQL。配置的修改只接受本机请求，' +
    '所以远程只能查看。PG 协议是明文传输（没有 TLS），跨公网请自行套隧道。'));
}

/* ---- 性能：并发、内存、落盘 ---- */
function renderPerfPane(box, d, rt) {
  box.appendChild(el('div', 'set-lead', '三个旋钮都影响吞吐和内存，改完立即生效（缓冲池除外）。'));

  box.appendChild(el('div', 'set-sec', '并发'));
  const pf = d.performance || {};
  box.appendChild(setRow('最大并发数', numInput('setPerfMaxConc', pf.maxConcurrent ?? 32, 0, 10000)));
  box.appendChild(setRow('排队上限', numInput('setPerfMaxQueued', pf.maxQueued ?? 256, 0, 100000)));
  box.appendChild(el('div', 'set-hint',
    '同时执行 SQL 的上限（0 = 不限），超出的排队；排队超过上限快速失败（SQLSTATE 53300）。' +
    '排队超时在「监控」面板的超时开关里设。'));

  box.appendChild(el('div', 'set-sec', '内存'));
  box.appendChild(setRow('缓冲池页数', numInput('setPoolPages', d.storage.bufferPoolPages, 0, 1048576), '页'));
  box.appendChild(el('div', 'set-hint',
    '一页 8KB，全库共享（不是每张表各一份）。池满后久未访问的页会被淘汰。' +
    '默认 4096 页 = 32MB；改完要重启才生效。'));

  box.appendChild(el('div', 'set-sec', '落盘节奏'));
  box.appendChild(setRow('每多少次提交', numInput('setCpCommits', d.storage.checkpointCommits, 0, 1000000), '次'));
  box.appendChild(setRow('每隔多少秒', numInput('setCpSeconds', d.storage.checkpointSeconds, 0, 86400), '秒'));
  box.appendChild(setRow('WAL 超过', numInput('setCpWALMB', d.storage.checkpointWalMB, 0, 65536), 'MB'));
  box.appendChild(el('div', 'set-hint',
    '三条任一满足就落盘（刷脏页 + 轮转 WAL），填 0 关闭该条。提交时的 fsync 始终同步执行。'));
}

/* ---- 外观：浏览器本地偏好 ---- */
function renderLookPane(box) {
  box.appendChild(el('div', 'set-lead',
    '皮肤和字号改完立刻生效，只存在这台浏览器，不用保存。'));

  const curTheme = document.documentElement.getAttribute('data-theme') || 'dark';
  const picks = el('div', 'theme-picks');
  for (const t of THEME_LIST) {
    const btn = el('button', 'theme-pick');
    btn.type = 'button';
    btn.dataset.theme = t.id;
    btn.setAttribute('aria-pressed', String(t.id === curTheme));
    btn.appendChild(el('span', 'theme-dot theme-dot-' + t.id));
    btn.appendChild(el('span', '', t.name));
    picks.appendChild(btn);
  }
  box.appendChild(picks);

  const fsCtl = el('div', 'fs-ctl');
  fsCtl.setAttribute('role', 'group');
  fsCtl.setAttribute('aria-label', '字号');
  const fsMinus = el('button', 'fs-btn', 'A−');
  fsMinus.type = 'button';
  fsMinus.dataset.fsStep = '-1';
  fsMinus.title = '缩小字号';
  const fsVal = el('span', 'fs-value', currentFsPct() + '%');
  fsVal.title = '当前字号';
  const fsPlus = el('button', 'fs-btn', 'A+');
  fsPlus.type = 'button';
  fsPlus.dataset.fsStep = '1';
  fsPlus.title = '放大字号';
  fsCtl.appendChild(fsMinus);
  fsCtl.appendChild(fsVal);
  fsCtl.appendChild(fsPlus);
  box.appendChild(setRow('字号', fsCtl));
  box.appendChild(el('div', 'set-hint',
    '这一页没有「保存」这回事：改了就生效，底部按钮会变成「关闭」。'));
}

/* ---- 数据：危险操作 + 现状 ---- */
function renderDataPane(box, d, rt) {
  const danger = el('div', 'set-danger');
  danger.appendChild(el('div', 'set-sec', '危险操作'));
  const acts = el('div', 'set-actions');
  const sampleBtn = el('button', 'btn btn-ghost btn-sm', '载入示例数据');
  sampleBtn.type = 'button';
  sampleBtn.title = '清空当前库并重建 departments / employees';
  sampleBtn.addEventListener('click', loadSample);
  const resetBtn = el('button', 'btn btn-ghost btn-sm btn-danger-text', '清空当前库');
  resetBtn.type = 'button';
  resetBtn.title = '删除当前库里的所有表（不可撤销）';
  resetBtn.addEventListener('click', resetDatabase);
  acts.appendChild(sampleBtn);
  acts.appendChild(resetBtn);
  danger.appendChild(setRow('维护', acts));
  danger.appendChild(el('div', 'set-hint',
    '两条都要手打库名才能确认（防手滑），且只作用于当前库（' +
    ((document.getElementById('dbBadge') || {}).textContent || '当前库') +
    '）。删掉的东西会先进回收站，停服后可用 -restore-trash 找回。'));
  box.appendChild(danger);

  box.appendChild(el('div', 'set-sec', '当前状态'));
  const cp = rt.checkpoint || {};
  const stats = el('div', 'set-stats');
  stats.appendChild(statLine('累计落盘', (cp.runs || 0) + ' 次'));
  stats.appendChild(statLine('上次落盘', cp.lastAt ? (cp.secondsAgo || 0) + ' 秒前' : '尚未'));
  stats.appendChild(statLine('之后已提交', (cp.commitsSince || 0) + ' 次'));
  stats.appendChild(statLine('上次写入', fmtBytes(cp.bytesWritten || 0)));
  stats.appendChild(statLine('累计写入', fmtBytes(cp.bytesTotal || 0)));
  stats.appendChild(statLine('当前 WAL', fmtBytes(cp.walBytes || 0)));
  const dbN = rt.databases || 1;
  stats.appendChild(statLine('库数', dbN + ' 个'));
  stats.appendChild(statLine('数据文件', fmtBytes(rt.dataBytes || 0)));
  // 多库时这些是全部库的合计，得写清楚，否则会以为「数据丢了 / 变多了」
  stats.appendChild(statLine('表 / 行数', (rt.tables || 0) + ' / ' + (rt.rows || 0) + (dbN > 1 ? '（全部库合计）' : '')));
  box.appendChild(stats);
  if (dbN > 1) {
    box.appendChild(el('div', 'set-hint',
      '以上表数、行数、数据文件都是 ' + dbN + ' 个库的合计。各库的明细在「表结构」标题旁的库名里。'));
  }
}

function renderSettings() {
  const d = settingsData;
  const rt = d.runtime || {};
  settingsBody.innerHTML = '';
  settingsTabs.innerHTML = '';
  setClearPw = false;

  if (!rt.canEdit) {
    settingsBody.appendChild(el('div', 'set-readonly',
      '当前连接来自 ' + (rt.clientIp || '远程序') + '，只能查看。配置的写操作只接受本机（127.0.0.1）请求。'));
  }
  // 告警是「你选的组合有坑」，不属于某一组，放在最上面始终可见
  for (const w of (d.warnings || [])) settingsBody.appendChild(el('div', 'set-warn', w));

  buildSettingsTabs();
  for (const p of SET_PANES) {
    const box = el('div', 'set-pane');
    box.dataset.pane = p.id;
    p.render(box, d, rt);
    settingsBody.appendChild(box);
  }

  // 记住初值：底部「N 处未保存」和关窗确认都靠它
  for (const n of settingsBody.querySelectorAll('input')) {
    n.dataset.init = n.type === 'checkbox' ? String(n.checked) : n.value;
    n.addEventListener('input', refreshDirty);
    n.addEventListener('change', refreshDirty);
  }

  if (!rt.canEdit) {
    // 远程只能查看：输入框全灰。外观是本地偏好，仍然可以点。
    for (const n of settingsBody.querySelectorAll('input')) n.disabled = true;
    for (const n of settingsBody.querySelectorAll('.set-pane:not([data-pane="look"]) button')) n.disabled = true;
  }

  if (!SET_PANES.some(p => p.id === setPane)) setPane = 'conn';
  setSettingsPane(setPane);
  // 刚建出来的按钮需要拿到当前档位的 disabled 状态
  applyFontScale(currentFsPct(), false);
  refreshDirty();
  lockSettingsHeight();
}

// lockSettingsHeight 把弹窗高度钉在「最高的那一组」上。
//
// 五组内容差得多（外观 300 出头、性能近 700），高度跟着内容走的话，点一下 Tab
// 窗口就蹿高或缩回去，鼠标下的按钮跟着跑。钉死之后矮组下方留白，切组纹丝不动。
// 量的时候必须先把卡片高度松开：卡片有固定高度时 body 会被撑开，
// scrollHeight 只会返回可视高度，五组量出来一样高，等于没量。
// 用户拽过右下角（userSize）就还给他设的尺寸 —— 但每次打开都重新量，
// 否则改过字号以后再打开，高度还停在旧的那一档上。
function lockSettingsHeight() {
  const card = settingsModal.querySelector('.modal-card');
  const panes = [...settingsBody.querySelectorAll('.set-pane')];
  if (!card || !panes.length || card.dataset.userSize === '1') return;

  const shown = Math.max(0, panes.findIndex(p => !p.classList.contains('hidden')));
  const hadMax = card.style.maxHeight;
  card.style.height = 'auto';
  card.style.maxHeight = 'none';

  let max = 0;
  for (let i = 0; i < panes.length; i++) {
    panes.forEach((q, j) => q.classList.toggle('hidden', i !== j));
    max = Math.max(max, settingsBody.scrollHeight);
  }
  panes.forEach((q, j) => q.classList.toggle('hidden', j !== shown));

  const head = card.querySelector('.modal-head');
  const foot = card.querySelector('.modal-foot');
  const chrome = (head ? head.offsetHeight : 0)
    + settingsTabs.offsetHeight
    + (foot ? foot.offsetHeight : 0)
    + 2; // 上下边框
  card.style.maxHeight = hadMax;
  if (max <= 0) return; // 弹窗还藏着（display:none）时量不出高度，别把卡片压成 0
  card.style.height = Math.min(Math.ceil(chrome + max), window.innerHeight - 80) + 'px';
}

async function saveSettings() {
  if (!settingsData || !settingsData.runtime.canEdit) return;
  if (invalidInputs().length) {
    setHint('有输入不合法，先改好再保存', 'err');
    return;
  }
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
      bufferPoolPages: val('setPoolPages'),
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
      clearPassword: setClearPw,
    },
    performance: {
      maxConcurrent: val('setPerfMaxConc'),
      maxQueued: val('setPerfMaxQueued'),
    },
    // 超时开关不在这页改（在「监控」面板），但后端是按整体替换的，
    // 不原样带回去就会把用户已开的超时清成 0。
    timeouts: settingsData.timeouts || {},
  };
  setHint('保存中…', '');
  try {
    const res = await api('/api/config', body);
    if (res.error) {
      setHint(res.error, 'err');
      return;
    }
    settingsData = res;
    const afterAddr = res.runtime.httpAddr;
    const moved = beforeAddr && afterAddr && beforeAddr !== afterAddr;
    // 存成功就把弹窗收掉：结果交给 toast，用户接着干活，不用再点一次 ×
    // （改监听地址是唯一要「停一下」的情况，toast 多留几秒把它读完）
    // 失败/被拒仍然留在弹窗里 —— 那句错误得贴着出错的输入框看。
    setClearPw = false;
    closeSettings(true); // 已落盘，别再弹「有改动没保存」
    setHint('');
    if (moved) {
      toast('已保存，控制台换到了 ' + afterAddr + '，请用新地址打开页面', false, 9000);
    } else {
      toast('设置已保存并生效');
    }
    // 端口/库名相关的东西会变，表结构跟着刷新一次
    loadSchema();
  } catch (e) {
    setHint('保存失败：' + e.message, 'err');
  }
}

document.getElementById('btnSettings').addEventListener('click', openSettings);
document.getElementById('btnSettingsClose').addEventListener('click', closeSettings);
document.getElementById('btnSettingsCancel').addEventListener('click', () => closeSettings());
document.getElementById('btnSettingsSave').addEventListener('click', () => {
  // 「外观」组没有可保存的东西，那一页的按钮就是关闭（有别的改动时仍显示「保存并应用」）
  if (document.getElementById('btnSettingsSave').dataset.mode === 'close') {
    closeSettings();
    return;
  }
  saveSettings();
});
document.addEventListener('keydown', e => {
  // 确认框开着的时候 Esc 只该关它自己，别顺手把底下的设置页也关掉
  // （设置页里的两个按钮都会弹这个确认框，叠在一起）。
  const cfOpen = !document.getElementById('confirmModal').classList.contains('hidden');
  if (e.key === 'Escape' && !cfOpen && !settingsModal.classList.contains('hidden')) closeSettings();
  // Esc 先收浮层，再关面板（一层一层退，别一下全关掉）
  if (e.key === 'Escape' && monSettingsOpen()) { setMonSettings(false); return; }
  // 空间地图里 Esc 先退一级（表视图 → 库视图），再按一次才关整个面板 ——
  // 一层一层退，别一下把面板关了让人从头点回来。
  if (e.key === 'Escape' && monOpen && monPageTab === 'map' && mapLevel === 'table') {
    backToDb();
    return;
  }
  if (e.key === 'Escape' && monOpen) toggleMonitor(false);
});

/* ---------------- 窄屏：表结构抽屉 ---------------- */

// 手机上侧栏默认收起（布局见 CSS 的 max-width:720px 段），这里只管状态与交互。
const sidebarToggle = document.getElementById('btnSidebar');
const sidebarScrim = document.getElementById('sidebarScrim');
const narrowMQ = window.matchMedia('(max-width: 720px)');

function setSidebarOpen(open) {
  document.body.classList.toggle('sidebar-open', !!open);
  sidebarToggle.setAttribute('aria-expanded', String(!!open));
}

sidebarToggle.addEventListener('click', () => {
  setSidebarOpen(!document.body.classList.contains('sidebar-open'));
});
sidebarScrim.addEventListener('click', () => setSidebarOpen(false));

// 点表就把抽屉收起来：手机上紧接着要看的是查询结果，不是继续挑表
treeEl.addEventListener('click', ev => {
  if (narrowMQ.matches && ev.target.closest('.table-row')) setSidebarOpen(false);
});

// 转回宽屏时清掉状态，否则 body 上还挂着 class、☰ 的 aria-expanded 也对不上
narrowMQ.addEventListener('change', e => {
  if (!e.matches) setSidebarOpen(false);
});

document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && document.body.classList.contains('sidebar-open')) setSidebarOpen(false);
});

/* ---------------- 启动 ---------------- */

(function init() {
  const saved = localStorage.getItem('godb.sql');
  if (saved) {
    editor.value = saved;
  } else {
    editor.value = EXAMPLES.select;
  }
  initCodeMirror();
  loadEditorHeight();
  autoSize();
  loadSchema();
  pollMonitor();
  setInterval(pollMonitor, 2000);
  // 支持 #locks / #settings / #help 直接打开对应面板
  if (location.hash === '#monitor' || location.hash === '#locks') toggleMonitor(true);
  if (location.hash === '#settings') openSettings();
  if (location.hash === '#help') openHelp();
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
// holdMs：停留时长（毫秒）。默认错误 7s、普通 2.6s；需要用户读一段地址之类的
// 信息时由调用方传更长的值。
function toast(msg, isErr, holdMs) {
  const t = document.getElementById('toast');
  if (!t) return;
  t.textContent = msg;
  t.className = 'toast' + (isErr ? ' err' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = 'toast hidden'; }, holdMs || (isErr ? 7000 : 2600));
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

// 欢迎页的快捷示例：点一下把 EXAMPLES 里的 SQL 填进编辑器（不自动执行，
// 免得在别人的库上误跑一条 UPDATE）。
document.addEventListener('click', ev => {
  const chip = ev.target.closest('.quick-chip');
  if (!chip) return;
  const sql = EXAMPLES[chip.dataset.ex];
  if (!sql) return;
  editor.value = sql;
  autoSize();
  editor.focus();
  // 光标放到末尾，接着就能按执行
  try { if (cm) cm.setCursor({ line: cm.lineCount() - 1, ch: 0 }); } catch (e) {}
});

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

// histQuery 是历史弹窗的过滤词（打开弹窗时清空，输入时实时过滤）
let histQuery = '';

function openHistory() {
  const modal = document.getElementById('historyModal');
  const body = document.getElementById('historyBody');
  const hint = document.getElementById('historyHint');
  const input = document.getElementById('histSearch');
  if (input.value !== histQuery) input.value = histQuery;

  const all = loadHistory();
  const q = histQuery.trim().toLowerCase();
  const list = q ? all.filter(x => x.sql.toLowerCase().includes(q)) : all;

  body.innerHTML = '';
  const bits = [`共 ${all.length} 条`];
  if (q) bits.push(`匹配 ${list.length} 条`);
  bits.push('本机保存');
  hint.textContent = all.length ? bits.join(' · ') : '';

  if (!all.length) {
    body.appendChild(el('div', 'hist-empty', '还没有执行过 SQL。'));
  } else if (!list.length) {
    body.appendChild(el('div', 'hist-empty', `没有匹配「${histQuery.trim()}」的 SQL`));
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
    // 单条删除：按 (sql, at) 定位（同一条 SQL 可能在不同时间出现过），
    // 删完就地重画 —— 不用整份清空，也不用刷新页面。
    const x = el('span', 'hist-x', '×');
    x.title = '从历史里删掉这一条';
    x.addEventListener('click', ev => {
      ev.stopPropagation();
      const rest = loadHistory().filter(it => !(it.sql === item.sql && it.at === item.at));
      try { localStorage.setItem(HISTORY_KEY, JSON.stringify(rest)); } catch (e) {}
      openHistory();
    });
    b.appendChild(x);
    body.appendChild(b);
  }
  modal.classList.remove('hidden');
}

function closeHistory() {
  document.getElementById('historyModal').classList.add('hidden');
}

document.getElementById('btnHistory').addEventListener('click', () => {
  histQuery = '';
  document.getElementById('histSearch').value = '';
  openHistory();
});
document.getElementById('histSearch').addEventListener('input', ev => {
  histQuery = ev.target.value;
  openHistory();
  document.getElementById('histSearch').focus();
});
document.getElementById('histSearch').addEventListener('keydown', ev => {
  if (ev.key === 'Escape') {
    ev.stopPropagation(); // 别把弹窗一起关了
    histQuery = '';
    ev.target.value = '';
    openHistory();
  }
});
document.getElementById('histSearchClear').addEventListener('click', () => {
  histQuery = '';
  document.getElementById('histSearch').value = '';
  openHistory();
  document.getElementById('histSearch').focus();
});
document.getElementById('btnHistoryClose').addEventListener('click', closeHistory);
document.getElementById('btnHistoryCancel').addEventListener('click', closeHistory);
document.getElementById('btnHistoryClear').addEventListener('click', () => {
  if (!confirm('清空本机保存的 SQL 历史？')) return;
  localStorage.removeItem(HISTORY_KEY);
  openHistory();
});
document.addEventListener('keydown', ev => {
  if (ev.key !== 'Escape') return;
  const m = document.getElementById('historyModal');
  if (m && !m.classList.contains('hidden')) closeHistory();
});

/* ---------------- 数据库 ---------------- */

// 库列表与当前库都存在服务端（cookie godb_db），前端只负责展示与切换。
// 所有既有接口都不用改 URL —— cookie 自动带上。
let dbData = null;

async function loadDBs() {
  const sel = document.getElementById('dbSelect');
  let data;
  try {
    data = await api('/api/dbs');
  } catch (e) {
    if (sel) sel.innerHTML = '';
    return;
  }
  dbData = data;
  const badge = document.getElementById('dbBadge');
  if (badge) badge.textContent = data.current || '';
  if (sel) {
    sel.innerHTML = '';
    for (const d of data.dbs || []) {
      const opt = document.createElement('option');
      opt.value = d.name;
      opt.textContent = d.name;
      if (d.name === data.current) opt.selected = true;
      sel.appendChild(opt);
    }
    sel.title = '当前数据库：' + (data.current || '');
  }
  const modal = document.getElementById('dbModal');
  if (modal && !modal.classList.contains('hidden')) renderDBList(data);
}

function dbHint(msg, kind) {
  const n = document.getElementById('dbHint');
  n.textContent = msg || '';
  n.className = 'modal-hint' + (kind ? ' ' + kind : '');
}

function renderDBList(data) {
  const body = document.getElementById('dbList');
  body.innerHTML = '';
  const dbs = data.dbs || [];
  if (!dbs.length) {
    const tr = el('tr');
    const td = el('td', 'dbt-empty', '没有任何库');
    td.colSpan = 5;
    tr.appendChild(td);
    body.appendChild(tr);
    return;
  }
  for (const d of dbs) {
    const tr = el('tr');
    if (d.name === data.current) tr.className = 'cur';

    const nameCell = el('td', 'c-name');
    nameCell.appendChild(el('span', 'db-name', d.name));
    if (d.default) nameCell.appendChild(el('span', 'db-tag', '默认'));
    if (d.name === data.current) nameCell.appendChild(el('span', 'db-tag db-tag-cur', '当前'));
    tr.appendChild(nameCell);

    tr.appendChild(el('td', 'c-num', String(d.tables || 0)));
    tr.appendChild(el('td', 'c-num', (d.rows || 0).toLocaleString()));
    // 占用 = 快照文件 + WAL。只显示快照会让人看到「有 3 行但 0 B」——
    // 数据还在 WAL 里没 checkpoint 而已。
    const used = (d.bytes || 0) + (d.walBytes || 0);
    const usedCell = el('td', 'c-num', fmtBytes(used));
    usedCell.title = `数据 ${fmtBytes(d.bytes || 0)} + WAL ${fmtBytes(d.walBytes || 0)}`;
    tr.appendChild(usedCell);

    const act = el('td', 'c-act');
    if (d.name !== data.current) {
      const sw = el('button', 'btn btn-ghost btn-sm', '切换');
      sw.type = 'button';
      sw.addEventListener('click', () => switchDB(d.name));
      act.appendChild(sw);
    }
    if (!d.default && d.name !== data.current) {
      const del = el('button', 'btn btn-ghost btn-sm btn-danger-text', '删除');
      del.type = 'button';
      del.title = `删除数据库 ${d.name}（连带全部数据）`;
      del.addEventListener('click', () => dropDB(d.name, d.tables || 0));
      act.appendChild(del);
    }
    tr.appendChild(act);
    body.appendChild(tr);
  }
}

async function switchDB(name) {
  try {
    const r = await api('/api/dbs', { action: 'switch', db: name });
    if (r.error) throw new Error(r.error);
  } catch (e) {
    dbHint('切换失败：' + e.message, 'err');
    return;
  }
  // 换了库：结果区清掉，表结构重新拉。**事务状态交给 loadSchema 同步** ——
  // 会话是按库分开的，目标库里这个会话可能正开着事务，不能想当然置成空闲。
  clearResults();
  await loadDBs();
  await loadSchema();
  dbHint('已切换到 ' + name, 'ok');
  const ed = document.getElementById('editor');
  if (ed) ed.placeholder = `-- 当前库：${name}，Ctrl+Enter 执行\nSELECT * FROM …`;
}

async function createDB() {
  const input = document.getElementById('dbNewName');
  const name = input.value.trim();
  if (!name) {
    dbHint('请先填库名', 'err');
    return;
  }
  const btn = document.getElementById('btnDBCreate');
  btn.disabled = true;
  try {
    const r = await api('/api/dbs', { action: 'create', name });
    if (r.error) throw new Error(r.error);
    input.value = '';
    dbHint('已创建空库 ' + r.name, 'ok');
    await loadDBs();
  } catch (e) {
    dbHint('新建失败：' + e.message, 'err');
  } finally {
    btn.disabled = false;
  }
}

async function dropDB(name, tables) {
  const extra = tables > 0 ? `\n该库有 ${tables} 张表，数据会被一起删除。` : '';
  if (!confirm(`删除数据库 ${name}？${extra}\n\n此操作不可撤销。`)) return;
  try {
    const r = await api('/api/dbs', { action: 'drop', name });
    if (r.error) throw new Error(r.error);
    dbHint('已删除 ' + name, 'ok');
    await loadDBs();
  } catch (e) {
    dbHint('删除失败：' + e.message, 'err');
  }
}

function openDBModal() {
  const modal = document.getElementById('dbModal');
  modal.classList.remove('hidden');
  dbHint('');
  document.getElementById('dbNewName').value = '';
  if (dbData) renderDBList(dbData);
  loadDBs().then(() => document.getElementById('dbNewName').focus());
}

function closeDBModal() {
  document.getElementById('dbModal').classList.add('hidden');
}

document.getElementById('dbSelect').addEventListener('change', ev => switchDB(ev.target.value));
document.getElementById('btnDBs').addEventListener('click', openDBModal);
document.getElementById('dbBadge').addEventListener('click', openDBModal);
document.getElementById('btnDBClose').addEventListener('click', closeDBModal);
document.getElementById('btnDBDone').addEventListener('click', closeDBModal);
document.getElementById('btnDBCreate').addEventListener('click', createDB);
document.getElementById('dbNewName').addEventListener('keydown', e => {
  if (e.key === 'Enter') createDB();
});
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !document.getElementById('dbModal').classList.contains('hidden')) closeDBModal();
});

document.getElementById('cellClose').addEventListener('click', closeCellViewer);
document.getElementById('cellDone').addEventListener('click', closeCellViewer);
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !document.getElementById('cellModal').classList.contains('hidden')) closeCellViewer();
});

loadDBs().then(() => {
  // #dbs 深链：直接打开库管理（分享链接或者刷新后停在同一个面板）
  if (location.hash === '#dbs') openDBModal();
});
