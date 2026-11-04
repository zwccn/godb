// 控制台的前端没有测试框架，这里用 Chrome DevTools 协议做「真实点击」的回归检查。
//
// 目前检查的是一条交互约定：**点弹窗遮罩不关闭**（设置 / 数据库 / 历史 / 表设计器），
// 只有 × 、取消/关闭按钮、Esc 能关。以后要加别的 UI 断言，往下面 results 里接着写。
//
// 用法：先启动服务（默认 8080），然后
//   node tools/ui_check.js
// 需要本机有 Chrome（路径见 CHROME 常量），会自己起一个 headless 实例、跑完退出。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');

const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const PORT = 9333;
const URL = 'http://127.0.0.1:8080/?theme=sky';

function get(path) {
  return new Promise((res, rej) => {
    http.get({ host: '127.0.0.1', port: PORT, path }, r => {
      let b = '';
      r.on('data', d => (b += d));
      r.on('end', () => res(JSON.parse(b)));
    }).on('error', rej);
  });
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

/* ---------- 等条件，而不是等时间 ----------

   原来满屏 `await sleep(1200)`：点了执行就睡 1.2 秒，假装结果已经回来了。
   机器快时白等（整个回归大半时间在睡觉），机器慢时又不够 —— 加探测卡那轮，
   1200ms 不够用，只能一路加到 2500。所以这里换成「每 40ms 问一次」。

   waitFor 超时**不抛**：返回 null，让后面的 check() 照常记一条失败。
   一条卡住就把后面几十条断言全带走，是最难查的那种红。 */
async function waitFor(cdp, cond, opt = {}) {
  const timeout = opt.timeout || 5000;
  const every = opt.every || 40;
  const label = opt.label || '条件';
  const deadline = Date.now() + timeout;
  for (;;) {
    let v = null;
    try {
      v = typeof cond === 'function' ? await cond() : await cdp.eval(cond);
    } catch (e) {
      v = null; // 页面正在重载、元素还不存在时求值会抛，当作「还没好」
    }
    if (v) return v;
    if (Date.now() >= deadline) {
      console.log(`      （等不到「${label}」，已等 ${timeout}ms）`);
      return null;
    }
    await sleep(every);
  }
}

// waitIdle 等「这次执行真的结束」：busy 是 app.js 顶层的执行中标志，
// 从页面全局作用域能直接读到。快就立刻返回，慢（慢查询/大结果集）也不会假失败。
function waitIdle(cdp, opt = {}) {
  return waitFor(cdp, `typeof busy !== 'undefined' && busy === false && runAbort === null`,
    Object.assign({ label: '执行结束', timeout: 20000 }, opt));
}

// settle 让浏览器画两帧：改完 DOM 立刻量尺寸会读到旧布局（差一帧）。
// 用它替代「改完睡 300ms」。
async function settle(cdp) {
  try {
    await cdp.eval(`new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))`);
  } catch (e) { /* 页面正在导航时忽略 */ }
}

class CDP {
  constructor(ws) { this.ws = ws; this.id = 0; this.wait = new Map(); }
  static async open(url) {
    const ws = new WebSocket(url);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    const c = new CDP(ws);
    ws.onmessage = ev => {
      const m = JSON.parse(ev.data);
      const w = c.wait.get(m.id);
      if (w) { c.wait.delete(m.id); w(m); }
    };
    return c;
  }
  send(method, params = {}) {
    const id = ++this.id;
    return new Promise(res => {
      this.wait.set(id, res);
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }
  async eval(expr) {
    const r = await this.send('Runtime.evaluate', {
      expression: expr, awaitPromise: true, returnByValue: true,
    });
    if (r.result && r.result.exceptionDetails) {
      throw new Error(JSON.stringify(r.result.exceptionDetails).slice(0, 300));
    }
    return r.result.result.value;
  }
}

(async () => {
  const prof = 'C:/Users/Administrator/AppData/Local/Temp/godb-cdp';
  // 每次都从干净的 profile 起步：这是个固定路径，上一次运行写下的 localStorage
  // （皮肤、编辑器高度）会漏到这一次，表现为「拖编辑器加高」这条断言时好时坏
  // —— 起始高度已经是视口上限时再拖也不涨，看着像功能坏了，其实是测试没隔离。
  fs.rmSync(prof, { recursive: true, force: true });
  const chrome = spawn(CHROME, [
    '--headless=new', '--disable-gpu', '--hide-scrollbars',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${prof}`,
    '--window-size=1400,900', 'about:blank',
  ], { stdio: 'ignore' });

  try {
    // 等调试端口起来
    for (let i = 0; i < 40; i++) {
      try { await get('/json/version'); break; } catch (e) { await sleep(250); }
    }
    const targets = await get('/json/list');
    let page = targets.find(t => t.type === 'page');
    let cdp = await CDP.open(page.webSocketDebuggerUrl);
    await cdp.send('Page.enable');
    await cdp.send('Page.navigate', { url: URL });
    // 等控制台真的起来（编辑器接管 + 结果区挂上），而不是睡 2.5 秒
    await waitFor(cdp, `typeof cm !== 'undefined' && cm &&
      !!document.querySelector('.CodeMirror') && !!document.getElementById('results')`,
      { label: '控制台初始化', timeout: 15000 });

    const results = [];
    const check = (name, ok, detail) => {
      results.push({ name, ok, detail });
      console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  ' + detail : ''}`);
    };

    /* 静态检查：app.js 里不许有重名的顶层函数声明。
       JS 不报错，后声明的会安静地覆盖先声明的 —— 这正是「探测卡把纳秒当毫秒」
       那次事故的成因：基准页加了个 fmtDur(ms)，把原来那个 fmtDur(ns) 顶掉了，
       于是等锁 12 000 ns 显示成「12 秒」、服务端 115 µs 显示成「1:55」。
       数字被放大 1000 倍，而且读起来完全合理，肉眼和截图都看不出来。 */
    {
      const declLines = new Map();
      const dupes = [];
      const src = fs.readFileSync(__dirname + '/../web/assets/app.js', 'utf8').split('\n');
      src.forEach((ln, i) => {
        const m = /^function\s+([A-Za-z_$][\w$]*)\s*\(/.exec(ln);
        if (!m) return;
        if (declLines.has(m[1])) dupes.push(`${m[1]}（${declLines.get(m[1])} 与 ${i + 1} 行）`);
        else declLines.set(m[1], i + 1);
      });
      check('app.js 没有重名的顶层函数（后声明会静默覆盖先声明）',
        dupes.length === 0, dupes.join(' · '));
    }

    /* 空间地图有几条断言要一张「几百页」的表。库里随时可能被清空（设置页就摆着
       「清空当前库」），所以这里先确保 big_demo 在：没有就现造 5 万行 + 落盘，
       已有就一步不动。以前这条链靠「库里正好有演示数据」，别人清一次库测试就红。
       单引号用 String.fromCharCode(39) 拼 —— 这段代码本身在模板字符串里，
       直接写 \' 会被吃掉一层，不如绕开转义。v 必须是文本：页检查器的「文本」视图
       就是靠从字节里认出业务文本来断言的。 */
    const seedTotal = await cdp.eval(`(async () => {
      const q = sql => fetch('/api/query', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ sql }),
      }).then(r => r.json());
      const head = await fetch('/api/pages?table=big_demo&kind=data&limit=1').then(r => r.json());
      if (!head.error) return head.total;
      const Q = String.fromCharCode(39);
      await q('CREATE TABLE big_demo (id INTEGER PRIMARY KEY, v TEXT)');
      for (let b = 0; b < 10; b++) {
        const rows = [];
        for (let i = b * 5000 + 1; i <= (b + 1) * 5000; i++) {
          rows.push('(' + i + ',' + Q + 'user_' + i + Q + ')');
        }
        await q('INSERT INTO big_demo VALUES ' + rows.join(','));
      }
      await q('CREATE INDEX big_ix_v ON big_demo (v)');
      await q('CHECKPOINT');
      const h2 = await fetch('/api/pages?table=big_demo&kind=data&limit=1').then(r => r.json());
      return h2.error ? 0 : h2.total;
    })()`);
    // 现造的表要重新拉一次空间数据，否则树图里没它（页面加载时的 spaceData 还是旧的）
    await cdp.eval(`loadSpace()`);
    await sleep(800);

    // 0) 执行按钮在工具栏左侧，而且点了真能执行
    const pos = await cdp.eval(`(() => {
      const b = document.getElementById('btnRun').getBoundingClientRect();
      const bar = document.querySelector('.editor-bar').getBoundingClientRect();
      return { left: b.left, mid: bar.left + bar.width / 2 };
    })()`);
    check('执行按钮在工具栏左半边', pos.left < pos.mid,
      `按钮 x=${Math.round(pos.left)}，中线 x=${Math.round(pos.mid)}`);

    await cdp.eval(`cm.setValue('SELECT 4242 AS answer')`);
    await cdp.eval(`document.getElementById('btnRun').click()`);
    await waitIdle(cdp);
    check('点「执行」能跑出结果',
      await cdp.eval(`document.getElementById('results').textContent.includes('4242')`));

    // 0.1) 执行按钮：配色跟帮助弹窗的「知道了」一致（不填主色），只靠一枚 9px 播放三角
    //      标记主操作。做过渐变+发光、主色填充、快捷键药丸三版，都被判「太重」退回。
    const runBtn = await cdp.eval(`(() => {
      const b = document.getElementById('btnRun');
      const cs = getComputedStyle(b);
      const peer = getComputedStyle(document.getElementById('btnHelpDone'));
      const ico = b.querySelector('.run-ico svg');
      const probe = document.createElement('span');
      probe.style.color = 'var(--accent)';
      document.body.appendChild(probe);
      const accent = getComputedStyle(probe).color;
      probe.remove();
      return {
        icon: !!ico,
        icoW: ico ? Math.round(ico.getBoundingClientRect().width) : 0,
        icoColor: ico ? getComputedStyle(ico).color : '',
        accent: accent,
        kbds: b.querySelectorAll('kbd').length,
        grad: cs.backgroundImage.indexOf('gradient') >= 0,
        shadow: cs.boxShadow,
        sameAsPeer: cs.backgroundColor === peer.backgroundColor &&
          cs.color === peer.color && cs.borderRadius === peer.borderRadius,
      };
    })()`);
    check('执行按钮：与「知道了」同色 + 一枚 accent 播放三角',
      runBtn.icon && runBtn.icoW >= 8 && runBtn.icoW <= 12 &&
        runBtn.icoColor === runBtn.accent && runBtn.kbds === 0 && !runBtn.grad &&
        runBtn.shadow === 'none' && runBtn.sameAsPeer,
      `图标 ${runBtn.icoW}px / kbd ${runBtn.kbds} 个 / 阴影 ${runBtn.shadow} / 与知道了同色 ${runBtn.sameAsPeer}`);

    // 执行中：文字变「停止」、图标从播放三角换成方块，而且**不能禁用**
    //（禁用 + 转圈的意思是「只能等」，但这里其实能中断）
    await cdp.eval(`setRunning(true)`);
    const running = await cdp.eval(`(() => {
      const b = document.getElementById('btnRun');
      const ico = b.querySelector('.run-ico svg');
      const stop = b.querySelector('.run-stop');
      return { label: document.getElementById('btnRunText').textContent,
               stopW: stop ? Math.round(stop.getBoundingClientRect().width) : 0,
               icoW: ico ? Math.round(ico.getBoundingClientRect().width) : 0,
               disabled: b.disabled };
    })()`);
    await cdp.eval(`setRunning(false)`);
    const back = await cdp.eval(`document.getElementById('btnRunText').textContent`);
    check('执行中：文字变「停止」、图标换成方块、按钮仍可点',
      running.label === '停止' && running.stopW > 0 && running.icoW === 0 &&
        !running.disabled && back === '执行',
      `${running.label} / 方块 ${running.stopW}px / 三角 ${running.icoW}px / 禁用 ${running.disabled} → ${back}`);

    /* 秒表：执行期间按钮旁边显示并持续走字，超过 3 秒转警示色，跑完收回。
       慢查询时人靠它决定「再等等」还是「点停止」——原来只有「停止」两个字，
       跑 0.8 秒和跑 40 秒看起来一模一样。 */
    await cdp.eval(`setRunning(true)`);
    // 等到「已经走了至少 0.2 秒」：等 0.0s 也算条件成立的话，量到的就是起点那一帧
    await waitFor(cdp, `(() => { const b = document.getElementById('runTimer');
      const t = b.textContent;
      return /^\\d+\\.\\ds$/.test(t) && parseFloat(t) >= 0.2 &&
        getComputedStyle(b).display !== 'none'; })()`,
      { label: '秒表走到 0.2 秒' });
    const tm1 = await cdp.eval(`(() => {
      const b = document.getElementById('runTimer');
      return { text: b.textContent, shown: getComputedStyle(b).display !== 'none',
               w: Math.round(b.getBoundingClientRect().width) };
    })()`);
    // 等它真的转黄（>3 秒），而不是睡死 3.1 秒 —— 以后阈值调小这条也不会白等
    await waitFor(cdp, `document.getElementById('runTimer').classList.contains('slow')`,
      { label: '秒表转警示色', timeout: 6000 });
    const tm2 = await cdp.eval(`(() => {
      const b = document.getElementById('runTimer');
      return { text: b.textContent, slow: b.classList.contains('slow'),
               color: getComputedStyle(b).color };
    })()`);
    await cdp.eval(`setRunning(false)`);
    const tm3 = await waitFor(cdp, `document.getElementById('runTimer').classList.contains('hidden')`,
      { label: '秒表收回', timeout: 2000 });
    check('执行中按钮旁有秒表，超过 3 秒转警示色，结束后收回',
      tm1.shown && /^\d+\.\ds$/.test(tm1.text) && parseFloat(tm1.text) >= 0.2 && tm1.w > 20
      && parseFloat(tm2.text) >= 3 && tm2.slow && tm3,
      `${tm1.text} → ${tm2.text}（${tm2.color}）· 收回 ${tm3}`);

    /* 空编辑器的占位符：淡色斜体一句、行号也收起来 —— 看着像个空输入框，
       而不是「第一行写着一句 -- 注释」。 */
    await cdp.eval(`cm.setValue('')`);
    await sleep(300);
    const phUi = await cdp.eval(`(() => {
      const p = document.querySelector('.CodeMirror-placeholder');
      const g = document.querySelector('.CodeMirror-gutter-wrapper');
      const ed = document.querySelector('.CodeMirror');
      return {
        text: p ? p.textContent : '',
        italic: p ? getComputedStyle(p).fontStyle === 'italic' : false,
        dim: p ? getComputedStyle(p).color !== getComputedStyle(ed).color : false,
        gutterW: g ? Math.round(g.getBoundingClientRect().width) : 0,
      };
    })()`);
    check('空编辑器只有一句淡色斜体占位符（没有行号、不像一句注释）',
      phUi.text === '在此输入 SQL' && phUi.italic && phUi.dim && phUi.gutterW === 0,
      `「${phUi.text}」· 斜体 ${phUi.italic} · 比正文淡 ${phUi.dim} · 行号宽 ${phUi.gutterW}`);

    /* F5 = 执行。用**真按键**发（合成事件不会触发浏览器默认动作，测不到「拦住刷新」），
       顺带用一个 window 上的标记确认页面确实没被刷掉。 */
    await cdp.eval(`cm.setValue('SELECT 7 AS f5;'); window.__f5mark = 'alive'; lastSubmittedSQL = null`);
    await cdp.send('Input.dispatchKeyEvent', { type: 'rawKeyDown', key: 'F5', code: 'F5', windowsVirtualKeyCode: 116, nativeVirtualKeyCode: 116 });
    await cdp.send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'F5', code: 'F5', windowsVirtualKeyCode: 116, nativeVirtualKeyCode: 116 });
    await waitFor(cdp, `lastSubmittedSQL === 'SELECT 7 AS f5;'`, { label: 'F5 提交语句' });
    await waitIdle(cdp);
    const f5Ui = await cdp.eval(`({ sql: lastSubmittedSQL, alive: window.__f5mark })`);
    check('F5 直接执行，而且页面没有被刷新',
      f5Ui.sql === 'SELECT 7 AS f5;' && f5Ui.alive === 'alive',
      `提交 ${JSON.stringify(f5Ui.sql)} · 页面存活 ${f5Ui.alive}`);

    await cdp.eval(`document.getElementById('helpModal').classList.remove('hidden'); lastSubmittedSQL = null`);
    await cdp.eval(`document.dispatchEvent(new KeyboardEvent('keydown', { key: 'F5', bubbles: true, cancelable: true }))`);
    await sleep(600);
    const f5Guard = await cdp.eval(`lastSubmittedSQL`);
    await cdp.eval(`document.getElementById('helpModal').classList.add('hidden')`);
    check('弹窗打开时 F5 不执行（免得在设置里按一下就跑了条 SQL）',
      f5Guard === null, `提交 ${JSON.stringify(f5Guard)}`);

    const ctrlF5 = await cdp.eval(`(() => {
      const ev = new KeyboardEvent('keydown', { key: 'F5', ctrlKey: true, bubbles: true, cancelable: true });
      document.dispatchEvent(ev);
      return ev.defaultPrevented;
    })()`);
    check('Ctrl+F5 不被拦，留给浏览器的硬刷新', ctrlF5 === false, `被拦 ${ctrlF5}`);

    // 0.1b) 美化 SQL：子句换行 + 关键字大写，字符串里的内容一个字都不能动
    await cdp.eval(`cm.setValue("select id,name from t where a='it''s' and b=1 order by id")`);
    await cdp.eval(`formatSQL()`);
    const fmted = await cdp.eval(`cm.getValue()`);
    check('美化：子句换行 + 关键字大写 + 字符串原样',
      /^SELECT id,/.test(fmted) && /\nFROM t\n/.test(fmted) &&
        /'it''s'/.test(fmted) && /ORDER BY id/.test(fmted),
      JSON.stringify(fmted).slice(0, 140));

    // 括号没配对就一个字都不改（宁可不动，也不要吐出半截 SQL 让人再改回去）
    await cdp.eval(`cm.setValue("select * from t where (a = 1")`);
    await cdp.eval(`formatSQL()`);
    const broken = await cdp.eval(`cm.getValue()`);
    check('美化：括号不配对时保持原样', broken === 'select * from t where (a = 1', JSON.stringify(broken));

    const fmtKey = await cdp.eval(`!!((cm.getOption('extraKeys') || {})['Shift-Ctrl-F'])`);
    check('美化：Ctrl+Shift+F 已绑定', fmtKey === true, String(fmtKey));

    // 0.1c) 结果区：一条 tab 栏（信息 / 结果 N）+ 同一时刻只有一条滚动条。
    //      以前 .results 自己滚、卡片里的 .grid-wrap 又按 52vh 滚，必然出现两条。
    // 执行一条 SQL 并等它真的跑完（不再猜「1.1 秒大概够」）
    const runSqlX = async (sql) => {
      await cdp.eval(`cm.setValue(${JSON.stringify(sql)}); document.getElementById('btnRun').click()`);
      await waitIdle(cdp);
    };
    const tabState = () => cdp.eval(`(() => {
      const tabs = [...document.querySelectorAll('.result-tab')];
      const res = document.getElementById('results');
      const inner = [...document.querySelectorAll('#results *')].filter(e => {
        const cs = getComputedStyle(e);
        return (cs.overflowY === 'auto' || cs.overflowY === 'scroll') && e.scrollHeight > e.clientHeight + 1;
      });
      const pane = document.getElementById('resultPane').firstElementChild || {};
      return {
        labels: tabs.map(t => (t.querySelector('.tab-label') || t).textContent.trim()),
        active: (document.querySelector('.result-tab.on') || {}).textContent || '',
        outerScrolls: res.scrollHeight > res.clientHeight + 1,
        innerScrollers: inner.length,
        pane: pane.className || '',
        text: (document.getElementById('resultPane').textContent || '').replace(/\\s+/g, ' ').slice(0, 200),
      };
    })()`);

    await runSqlX('SELECT * FROM employees');
    const tSingle = await tabState();
    check('单条结果：tab 是「信息 / 结果 1」且默认停在结果 1',
      tSingle.labels.join('|') === '信息|结果 1' && tSingle.active === '结果 1', tSingle.labels.join('|'));
    check('结果区只有一条滚动条（外层不滚、表体自己滚）',
      !tSingle.outerScrolls && tSingle.innerScrollers <= 1,
      `外层滚动=${tSingle.outerScrolls} / 内层滚动容器=${tSingle.innerScrollers}`);

    await runSqlX('SELECT 1 AS a; SELECT 2 AS b');
    const tMulti = await tabState();
    check('多语句：有结果集的语句各占一个 tab，不再纵向堆叠',
      tMulti.labels.join('|') === '信息|结果 1|结果 2', tMulti.labels.join('|'));
    await cdp.eval(`[...document.querySelectorAll('.result-tab')].find(t => t.textContent.trim() === '信息').click()`);
    await waitFor(cdp, `(() => { const a = document.querySelector('.result-tab.on');
      return !!a && a.textContent.trim() === '信息'; })()`, { label: '切到「信息」tab' });
    const tInfo = await tabState();
    check('「信息」tab = 往返时间 + 逐条语句明细',
      tInfo.pane.indexOf('info-body') === 0 && /浏览器往返/.test(tInfo.text) && /SELECT 1 AS a/.test(tInfo.text),
      tInfo.text.slice(0, 90));

    await runSqlX('SELECT * FROM no_such_table');
    const tErr = await tabState();
    check('报错时默认落到「信息」，且只有信息这一个 tab',
      tErr.labels.join('|') === '信息' && tErr.active === '信息' &&
        /does not exist|不存在/.test(tErr.text), tErr.text.slice(0, 70));

    // 失败文案只写一遍「失败」：标题的兜底值本来就叫「执行失败」，
    // 原来拼成「失败 · 执行失败」，一句话说两遍。
    const failText = await cdp.eval(`((document.querySelector('.info-status.err') || {}).textContent || '').trim()`);
    check('失败状态行不重复（不是「失败 · 执行失败」）', failText === '失败', JSON.stringify(failText));

    // 0.1d) 数值列对齐：按**列声明的类型**判，不看值的 JS 类型。
    //      numeric 走 JSON 是字符串（保精度），只看 typeof 会让 id 右对齐、salary 左对齐。
    await runSqlX('SELECT id, salary FROM employees ORDER BY id LIMIT 3');
    await waitFor(cdp, `document.querySelectorAll('table.grid tbody tr td').length >= 2`, { label: '结果表格' });
    const numAlign = await cdp.eval(`(() => {
      const tds = [...document.querySelectorAll('table.grid tbody tr:first-child td')];
      return tds.map(td => ({ text: td.textContent.trim(), cls: td.className, align: getComputedStyle(td).textAlign }));
    })()`);
    check('数值列按类型右对齐（numeric 是字符串，也照样算数值）',
      numAlign.length === 2 && numAlign[0].align === 'right' && numAlign[1].align === 'right'
        && /num/.test(numAlign[0].cls) && /num/.test(numAlign[1].cls),
      numAlign.map(c => `${c.text}→${c.align}`).join(', '));

    // 0.1e) 0 行的 SELECT 也要有结果页：表头是服务端真给了的，空集时人第一个想知道
    //      的就是「列名/类型对不对」。原来只有 rows.length > 0 才建 tab，
    //      这种查询只剩一个「信息」，看着像没反应。
    //      （服务端 0 行时**不返回 rows 字段**，不是空数组 —— 入口补成 [] 才能画表头）
    await runSqlX('SELECT id, name FROM employees WHERE 1 = 0');
    const zeroTab = await cdp.eval(`(() => ({
      labels: [...document.querySelectorAll('.result-tab')].map(t => (t.querySelector('.tab-label') || t).textContent.trim()),
      ths: [...document.querySelectorAll('table.grid thead th')].map(t => t.textContent.trim()),
      empty: (document.querySelector('.grid-empty-cell') || {}).textContent || '',
      foot: (document.querySelector('.grid-range') || {}).textContent || '',
      err: !!(document.querySelector('.error-body')),
    }))()`);
    check('0 行的 SELECT 也有结果页（表头 + 一句说明 + 「共 0 行」）',
      zeroTab.labels.join('|') === '信息|结果 1' && zeroTab.ths.length === 2
        && /id/.test(zeroTab.ths[0]) && zeroTab.empty === '没有匹配的行'
        && zeroTab.foot === '共 0 行' && !zeroTab.err,
      `${zeroTab.labels.join('|')} / 表头 ${zeroTab.ths.length} / 「${zeroTab.empty}」/ ${zeroTab.foot}`);

    // 0.1f) 执行中：结果区顶边一条走动条 + aria-busy。
    //      直接调 setRunning 而不是发一条慢查询 —— 慢查询在这个数据规模下不可靠地
    //      慢（同一句 57ms～568ms 都见过），断言会变成随机红绿。这里要钉的是
    //      「状态切换有没有真的动到 DOM」。
    const runFlag = await cdp.eval(`(() => {
      const p = document.getElementById('results');
      setRunning(true);
      const on = { cls: p.classList.contains('is-running'), busy: p.getAttribute('aria-busy'),
        anim: getComputedStyle(p, '::after').animationName };
      setRunning(false);
      const off = { cls: p.classList.contains('is-running'), busy: p.getAttribute('aria-busy') };
      return { on, off };
    })()`);
    check('执行中：结果区有走动条且结束即撤（旧结果不压暗）',
      runFlag.on.cls && runFlag.on.busy === 'true' && runFlag.on.anim === 'res-running'
        && !runFlag.off.cls && runFlag.off.busy === 'false',
      `开=${runFlag.on.cls}/${runFlag.on.anim} 关=${runFlag.off.cls}`);
    await waitIdle(cdp);

    // 0.1g) 图标按钮 24×24：实测原来是 20×24 / 22×22，鼠标点得准才行
    const iconSizes = await cdp.eval(`['btnNewTable','btnTableSearch','btnHelp','btnRefresh']
      .map(id => { const e = document.getElementById(id); if (!e) return null;
        const r = e.getBoundingClientRect(); return { id, w: Math.round(r.width), h: Math.round(r.height) }; })
      .filter(Boolean)`);
    check('图标按钮点击目标 ≥ 24×24',
      iconSizes.length >= 3 && iconSizes.every(s => s.w >= 24 && s.h >= 24),
      iconSizes.map(s => `${s.id} ${s.w}×${s.h}`).join('  '));

    // 0.2)「执行计划」按钮：只解释查询，别把 BEGIN/UPDATE 也套上 EXPLAIN
    //      （以前是 split(';') 全量加前缀，事务演示一按就报 only supports SELECT）
    const planText = () => cdp.eval(`(document.getElementById('results').textContent || '').replace(/\\s+/g, ' ')`);
    await cdp.eval(`cm.setValue("BEGIN; UPDATE employees SET salary = salary; SELECT * FROM employees LIMIT 1;")`);
    await cdp.eval(`document.getElementById('btnExplain').click()`);
    await waitIdle(cdp);
    const mixed = await planText();
    check('执行计划：多语句里只解释 SELECT（不再报错）',
      /Scan|Project/.test(mixed) && !/only supports/i.test(mixed), mixed.slice(0, 120));

    await cdp.eval(`cm.setValue("EXPLAIN SELECT 1;")`);
    await cdp.eval(`document.getElementById('btnExplain').click()`);
    await waitIdle(cdp);
    const doubleEx = await planText();
    check('执行计划：已经是 EXPLAIN 的语句不再套一层',
      /Result|Project/.test(doubleEx) && !/only supports/i.test(doubleEx), doubleEx.slice(0, 120));

    // 字符串里的分号不能被当成分隔符
    await cdp.eval(`cm.setValue("SELECT 'a;b' AS v;")`);
    await cdp.eval(`document.getElementById('btnExplain').click()`);
    await waitIdle(cdp);
    const semi = await planText();
    check('执行计划：字符串里的分号不会被切开',
      /Result|Project/.test(semi) && !/only supports|执行失败/i.test(semi), semi.slice(0, 120));

    // 一条能解释的都没有：直接说清楚，别发个请求换回看不懂的报错
    await cdp.eval(`cm.setValue("UPDATE employees SET salary = salary WHERE id = 1;")`);
    await cdp.eval(`document.getElementById('btnExplain').click()`);
    await waitIdle(cdp);
    const onlyWrite = await planText();
    check('执行计划：全是非查询语句时给出明确提示',
      /只能解释查询语句/.test(onlyWrite) && /UPDATE/.test(onlyWrite), onlyWrite.slice(0, 100));

    // 0.3) 新能力：Top-N、interval、精确 numeric、快聚合、覆盖索引
    const runSql = async (sql) => {
      await cdp.eval(`cm.setValue(${JSON.stringify(sql)})`);
      await cdp.eval(`document.getElementById('btnRun').click()`);
      await waitIdle(cdp);
      return (await cdp.eval(`document.getElementById('results').textContent`) || '').replace(/\s+/g, ' ');
    };
    const t1 = await runSql(`SELECT 0.1::numeric + 0.2::numeric AS x`);
    check('精确 numeric：0.1+0.2=0.3', t1.includes('0.3') && !t1.includes('0.30000000000000004'), t1.slice(0, 80));
    const t2 = await runSql(`SELECT now() - INTERVAL '1 day' AS d`);
    check('interval 时间加减', /执行失败|error/i.test(t2) === false && /20\d\d-/.test(t2), t2.slice(0, 80));
    const t3 = await runSql(`SELECT lpad('1', 6, '0') AS c`);
    check('lpad', t3.includes('000001'), t3.slice(0, 80));
    const t3b = await runSql(`SELECT unnest(array[1,2]) AS n`);
    check('unnest 展开多行', t3b.includes('1') && t3b.includes('2'), t3b.slice(0, 80));
    const t4 = await runSql(`EXPLAIN SELECT count(*) FROM employees`);
    check('快聚合 count(*)', /Fast Aggregate/.test(t4), t4.slice(0, 100));
    const t5 = await runSql(`CREATE INDEX IF NOT EXISTS ix_ui_dept ON employees (dept_id)`);
    check('CREATE INDEX IF NOT EXISTS', !/执行失败|error/i.test(t5), t5.slice(0, 80));
    const t5b = await runSql(`EXPLAIN SELECT dept_id FROM employees WHERE dept_id > 0`);
    check('覆盖索引或索引扫描', /Index/.test(t5b), t5b.slice(0, 100));

    // 0.4) 顶栏状态：空闲不占位（药丸隐藏），事务中露出来且圆点提示跟着变
    await runSql(`BEGIN; UPDATE employees SET salary = salary WHERE id = 1;`);
    const txOn = await cdp.eval(`(() => {
      const p = document.getElementById('txPill');
      const d = document.getElementById('monDot');
      return { pillShown: !p.classList.contains('hidden'), tip: d ? d.title : '', text: p.textContent };
    })()`);
    check('进事务：药丸露出 + 圆点提示含「事务中」',
      txOn.pillShown && /事务中/.test(txOn.tip), txOn.text + ' / ' + txOn.tip);
    await runSql(`ROLLBACK;`);
    const txOff = await cdp.eval(`(() => {
      const p = document.getElementById('txPill');
      const d = document.getElementById('monDot');
      return { pillHidden: p.classList.contains('hidden'), tip: d ? d.title : '' };
    })()`);
    check('回滚后：药丸隐藏 + 圆点提示回到「空闲」',
      txOff.pillHidden && /空闲/.test(txOff.tip), txOff.tip);

    // 0.4c) 监控面板：慢SQL/慢事务是紧凑表格，点行出详情（含扫描行数）
    // 先跑一条语句：面板读的是「当前库」的环形缓冲，刚切过库 / 刚清过表时它是空的，
    // 表格里只有一行「还没有执行记录」，下面的断言就会随机变红。
    await cdp.eval(`cm.setValue("SELECT 1 AS ping")`);
    await cdp.eval(`document.getElementById('btnRun').click()`);
    await waitIdle(cdp);
    await cdp.eval(`document.getElementById('btnMonitor').click()`);
    // 面板会自己拉一次 /api/monitor 再渲染，等慢 SQL 表格真的出现
    await waitFor(cdp, `!!document.querySelector('#monSlowSQL tr')`,
      { label: '监控-慢SQL 表格' });
    const monUi = await cdp.eval(`(() => {
      const tb = document.querySelector('#monSlowSQL').closest('table');
      const cell = document.querySelector('#monSlowSQL td.c-sql');
      const cs = cell ? getComputedStyle(cell) : null;
      return {
        isTable: !!tb,
        rows: document.querySelectorAll('#monSlowSQL tr').length,
        tableLayout: tb ? getComputedStyle(tb).tableLayout : '',
        ellipsis: cs ? cs.textOverflow : '',
        nowrap: cs ? cs.whiteSpace : '',
      };
    })()`);
    check('慢SQL 是紧凑表格 + SQL 列省略号截断',
      monUi.isTable && monUi.rows > 0 && monUi.tableLayout === 'fixed' &&
      monUi.ellipsis === 'ellipsis' && monUi.nowrap === 'nowrap',
      `${monUi.rows} 行 / ${monUi.tableLayout} / ${monUi.ellipsis}`);

    // 存储卡里的「索引」行：占用要能看出来，重建次数必须解释得清
    const ixRow = await cdp.eval(`(() => {
      const rows = [...document.querySelectorAll('#monIO .io-row')];
      const hit = rows.find(r => ((r.querySelector('.io-k') || {}).textContent || '') === '索引');
      if (!hit) {
        return { found: false, keys: rows.map(r => (r.querySelector('.io-k') || {}).textContent) };
      }
      return {
        found: true,
        main: (hit.querySelector('.io-v') || {}).textContent || '',
        sub: (hit.querySelector('.io-sub') || {}).textContent || '',
        tip: hit.title || '',
      };
    })()`);
    check('存储卡有「索引」行（占用 + 页数 + 重建次数，且带解释）',
      ixRow.found && /页|索引/.test(ixRow.sub) && /WAL/.test(ixRow.tip),
      ixRow.found ? `${ixRow.main} · ${ixRow.sub}` : `缺行：${JSON.stringify(ixRow.keys)}`);

    // 页密度：百分比 + 页数，解释里必须说清「洞不算空闲」
    const fillRow = await cdp.eval(`(() => {
      const rows = [...document.querySelectorAll('#monIO .io-row')];
      const hit = rows.find(r => ((r.querySelector('.io-k') || {}).textContent || '') === '页密度');
      if (!hit) {
        return { found: false, keys: rows.map(r => (r.querySelector('.io-k') || {}).textContent) };
      }
      return {
        found: true,
        main: (hit.querySelector('.io-v') || {}).textContent || '',
        sub: (hit.querySelector('.io-sub') || {}).textContent || '',
        tip: hit.title || '',
      };
    })()`);
    check('存储卡有「页密度」行（百分比或「—」+ 说明洞不算空闲）',
      fillRow.found && (/%|—/.test(fillRow.main)) && /VACUUM/.test(fillRow.tip),
      fillRow.found ? `${fillRow.main} · ${fillRow.sub}` : `缺行：${JSON.stringify(fillRow.keys)}`);

    // 页面级 Tab：概览 / 诊断 / 存储 / 空间地图。诊断卡里的小 Tab 只留三类诊断。
    const navUi = await cdp.eval(`(() => {
      const nav = document.getElementById('monNav');
      const tabs = nav ? [...nav.querySelectorAll('.mon-nav-tab')] : [];
      return {
        n: tabs.length,
        labels: tabs.map(b => b.textContent.trim()),
        on: (tabs.find(b => b.classList.contains('on')) || {}).dataset &&
            tabs.find(b => b.classList.contains('on')).dataset.page,
      };
    })()`);
    // 顺序是「排查时先看哪个」：诊断紧跟概览（健康灯一亮就在这儿找原因），
    // 存储排在它后面（确认是磁盘问题才去），空间地图最后。
    /* 概览第一块是「结论」：一句话 + 三个数字 + 有才出现的建议。
       目的是让第一次打开的人不用先读懂水位/P95 那一排，就知道「有没有事」。 */
    const vdOk = await cdp.eval(`(() => {
      const b = document.getElementById('monVerdict');
      const page = document.querySelector('.mon-page[data-page="overview"]');
      const layers = [...b.querySelectorAll('.mon-layer')];
      return {
        first: page.firstElementChild === b,
        title: (b.querySelector('.vd-title') || {}).textContent || '',
        layers: layers.length,
        caps: layers.map(l => (l.querySelector('.lv-cap') || {}).textContent || ''),
        counts: (b.querySelector('.vd-counts') || {}).textContent || '',
        advice: b.querySelectorAll('.vd-adv').length,
        level: b.dataset.level,
        water: !!document.getElementById('monWater'),
      };
    })()`);
    // 建议行不是「正常就一定没有」：跑过测试之后慢查询记录里总会有几条，
    // 所以这里只断言「最多两条」和结构，具体文案在下面那条里造数据验。
    check('概览第一块是结论卡：一句话 + 三层数字都在同一张卡里（建议最多两条）',
      vdOk.first && /运行正常|有点忙|有点撑不住/.test(vdOk.title) && vdOk.layers === 3
      && vdOk.advice <= 2 && vdOk.level === 'green' && !vdOk.water,
      `${vdOk.title} · ${vdOk.layers} 列 · 建议 ${vdOk.advice} 条 · ${vdOk.level}`);
    /* 这三个数以前在结论卡、水位条、三层卡各来一遍，同一个 0 一屏三份，
       「113.1 ms」和「113 ms」还是同一个字段的两种写法。现在只剩一份，这条钉住它。 */
    check('水位条已并入三列：容量跟各自的层走，累计次数回到结论卡',
      vdOk.caps.filter(Boolean).length === 2 && vdOk.caps.includes('/256')
      && vdOk.caps.includes('/32') && /Kill \d+/.test(vdOk.counts),
      `${vdOk.caps.join(' ')} · ${vdOk.counts.slice(0, 36)}`);

    // 造一个「有排队 + 有慢查询」的状态：建议行必须出现，且按钮真的能跳到对应 Tab
    const vdBusy = await cdp.eval(`(() => {
      renderVerdict({ water: { exec: 6, execMax: 32, wait: 2, queue: 3, level: 'yellow' },
        layers: { running: { count: 6, maxNs: 2600000000 } }, slowSql: [{ n: 4 }, { n: 5 }] });
      const b = document.getElementById('monVerdict');
      const rows = [...b.querySelectorAll('.vd-adv')].map(r => r.textContent);
      b.querySelectorAll('.vd-adv button')[0].click();
      return { level: b.dataset.level, rows, page: monPageTab, tab: monTab };
    })()`);
    check('忙的时候给出建议动作，按钮直接跳到诊断的对应 Tab',
      vdBusy.level === 'yellow' && vdBusy.rows.length === 2
      && /排队/.test(vdBusy.rows[0]) && /慢查询/.test(vdBusy.rows[1])
      && vdBusy.page === 'diag' && vdBusy.tab === 'lock',
      `${vdBusy.level} · ${vdBusy.rows.join(' | ')} → ${vdBusy.page}/${vdBusy.tab}`);
    await cdp.eval(`setMonPage('overview')`);

    check('监控顶部有页面级 Tab（概览 / 诊断 / 存储 / 空间地图 / 基准），默认概览',
      navUi.n === 5 && navUi.labels.join('/') === '概览/诊断/存储/空间地图/基准' && navUi.on === 'overview',
      navUi.labels.join(' / ') + ' · 当前 ' + navUi.on);

    // 概览页底部留一行存储摘要：fsync 是最高频的排障入口，不该逼人先切页
    const sumUi = await cdp.eval(`(() => {
      const box = document.getElementById('monIOSum');
      const items = box ? [...box.querySelectorAll('.sum-item')] : [];
      const card = box ? box.closest('.mon-card').getBoundingClientRect() : null;
      const panel = document.getElementById('monPanel').getBoundingClientRect();
      return {
        n: items.length,
        keys: items.map(e => (e.querySelector('.sum-k') || {}).textContent),
        // 摘要必须在概览首屏内：要滚一下才能看见就白放了
        inView: card ? card.bottom <= panel.bottom + 1 : false,
        h: card ? Math.round(card.height) : 0,
      };
    })()`);
    check('概览页有存储摘要（fsync / 缓冲池 / 页密度），且一屏放得下',
      sumUi.n === 3 && sumUi.keys.join('/') === 'fsync/缓冲池/页密度' && sumUi.inView,
      `${sumUi.keys.join(' / ')} · 卡片 ${sumUi.h}px · 首屏内 ${sumUi.inView}`);


    // 0.4c2) 监控曲线：纵横轴 + 零延迟悬浮读数。
    // 这两块以前缺得最明显 —— 悬停靠原生 title（系统提示，停住 0.7~1 秒才弹，
    // 而且只说一句固定的话），纵横轴一个都没有，鼠标放上去什么也问不出来。
    await waitFor(cdp, `document.querySelectorAll('#monPanel .mon-page[data-page="overview"] .spark-plot').length >= 3`,
      { label: '监控曲线' });
    const axUi = await cdp.eval(`(() => {
      return [...document.querySelectorAll('#monPanel .mon-page[data-page="overview"] .spark-plot')].map(p => ({
        ys: [...p.querySelectorAll('.spark-y')].map(s => s.textContent.trim()),
        xs: [...p.parentElement.querySelectorAll('.spark-x span')].map(s => s.textContent.trim()),
        // 刻度必须是 HTML：图是 preserveAspectRatio="none" 拉伸铺满卡片的，
        // 画在 SVG 里的 <text> 会跟着被横向拉扁，字读不出来
        textInSvg: p.querySelectorAll('svg text').length,
      }));
    })()`);
    check('监控曲线：三张图都有纵轴刻度（≥2 档）+ 横轴三个时刻',
      axUi.length === 3 && axUi.every(a => a.ys.length >= 2 && a.xs.length === 3
        && /^\d\d:\d\d:\d\d$/.test(a.xs[0]) && /^\d\d:\d\d:\d\d$/.test(a.xs[2])),
      axUi.map(a => a.ys.join('/') + ' | ' + a.xs.join(' ')).join(' ‖ '));
    // 中间那格允许是空的：点少的时候中点和起点落在同一秒，会留空占位而不是重复两次
    check('横轴时刻按「起点 → 中点 → 终点」递增',
      axUi.every(a => a.xs[0] <= a.xs[2]
        && (!a.xs[1] || (a.xs[0] <= a.xs[1] && a.xs[1] <= a.xs[2]))),
      axUi.map(a => a.xs.join('→')).join(' ‖ '));
    check('纵轴刻度不重复（区间窄、取整撞车时丢掉中间那一档）',
      axUi.every(a => new Set(a.ys).size === a.ys.length),
      axUi.map(a => a.ys.join('/')).join(' ‖ '));
    check('刻度文字是 HTML 覆盖层，SVG 里一个 text 都没有（否则会被拉伸变形）',
      axUi.every(a => a.textInSvg === 0), axUi.map(a => a.textInSvg).join(','));

    /* 吞吐卡里叠着两张曲线（QPS/TPS 与 连接数），图例必须贴着各自那张图。
       以前图例在图**下面**：上一张的「QPS / TPS（虚线）」就夹在两张图中间，
       离下一张的纵轴刻度只有十来像素，读的人分不清那行小字是谁的标签。
       现在图例移到图的上方、与绘图区左对齐，两张图之间再加一条分隔线。
       这三条一起钉住排版 —— 光看截图看不出来，只能量。 */
    const stackUi = await cdp.eval(`(() => {
      return [...document.querySelectorAll('#monPanel .mon-page[data-page="overview"] .mon-spark')].map(s => {
        const tip = s.querySelector('.spark-tip');
        const plot = s.querySelector('.spark-plot');
        const tr = tip.getBoundingClientRect();
        const pr = plot.getBoundingClientRect();
        const sr = s.getBoundingClientRect();
        return {
          tipTop: Math.round(tr.top), tipLeft: Math.round(tr.left),
          plotTop: Math.round(pr.top), plotLeft: Math.round(pr.left),
          boxTop: Math.round(sr.top),
          borderTop: parseFloat(getComputedStyle(s).borderTopWidth) || 0,
          text: tip.textContent.replace(/\\s+/g, ''),
        };
      });
    })()`);
    check('两张曲线各带图例，且图例在图的上方（不再夹在两张图之间）',
      stackUi.length === 2 && stackUi.every(s => s.tipTop < s.plotTop),
      stackUi.map(s => `${s.text} ${s.tipTop}<${s.plotTop}`).join(' ‖ '));
    check('图例与绘图区左对齐（不挤进纵轴刻度那一列）',
      stackUi.length === 2 && stackUi.every(s => Math.abs(s.tipLeft - s.plotLeft) <= 2),
      stackUi.map(s => `${s.tipLeft}/${s.plotLeft}`).join(' '));
    check('两张图之间有分隔线，图例离上一张图 ≥24px',
      stackUi.length === 2 && stackUi[1].borderTop > 0 &&
        stackUi[1].tipTop - (stackUi[0].plotTop + 64) >= 24,
      stackUi.length === 2
        ? `border=${stackUi[1].borderTop}px gap=${stackUi[1].tipTop - stackUi[0].plotTop - 64}px`
        : `${stackUi.length} 张图`);

    // 悬浮：鼠标落在绘图区 70% 处。headless 下第一次 mouseMoved 只算「进入页面」，
    // 同一个点要再发一次才落到元素上（所以上面那几条不要照抄这个发两次的写法）。
    await cdp.eval(`document.querySelector('#monPanel .proc-spark .spark-plot').scrollIntoView({ block: 'center' })`);
    await sleep(150);
    const sparkBox = await cdp.eval(`(() => {
      const b = document.querySelector('#monPanel .proc-spark .spark-plot').getBoundingClientRect();
      return { x: b.left, y: b.top, w: b.width, h: b.height };
    })()`);
    const snoop = { x: Math.round(sparkBox.x + sparkBox.w * 0.7), y: Math.round(sparkBox.y + sparkBox.h * 0.5) };
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: snoop.x, y: snoop.y, button: 'none' });
    await sleep(80);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: snoop.x, y: snoop.y, button: 'none' });
    await sleep(140);
    const hov = await cdp.eval(`(() => {
      const p = document.querySelector('#monPanel .proc-spark .spark-plot');
      const c = p.querySelector('.spark-hover');
      const cur = p.querySelector('.spark-cursor');
      const cb = c.getBoundingClientRect();
      const pb = p.getBoundingClientRect();
      return {
        text: c.textContent,
        op: getComputedStyle(c).opacity,
        curOp: getComputedStyle(cur).opacity,
        dots: [...p.querySelectorAll('.spark-dot')].map(d => getComputedStyle(d).opacity),
        flip: c.style.transform,
        inside: cb.top >= pb.top - 1 && cb.bottom <= pb.bottom + 1
          && cb.left >= pb.left - 1 && cb.right <= pb.right + 1,
      };
    })()`);
    check('曲线悬浮：竖线 + 每条线的跟随点 + 读数卡片同时出现',
      hov.op === '1' && hov.curOp === '1' && hov.dots.length === 2 && hov.dots.every(o => o === '1'),
      `卡片 ${hov.op} / 竖线 ${hov.curOp} / 点 ${hov.dots.join(',')}`);
    check('读数卡片：那一刻的时刻 + 两条线的数值，整块落在绘图区内',
      /^\d\d:\d\d:\d\d/.test(hov.text) && /CPU/.test(hov.text) && /内存/.test(hov.text) && hov.inside,
      `${hov.text.slice(0, 40)} · 区内 ${hov.inside}`);
    check('指针靠右时卡片翻到竖线左边（不会顶出绘图区）',
      hov.flip.includes('-100%'), hov.flip);

    // 监控每秒刷新、每秒重建整块卡片。重建后必须把读数原样放回去，
    // 否则鼠标一动不动，卡片也会每秒闪一下（甚至直接消失）。
    await sleep(1600);
    const hov2 = await cdp.eval(`(() => {
      const p = document.querySelector('#monPanel .proc-spark .spark-plot');
      const c = p.querySelector('.spark-hover');
      return { op: getComputedStyle(c).opacity, text: c.textContent };
    })()`);
    check('曲线每秒重绘后读数卡片仍在（鼠标没动也不该闪掉）',
      hov2.op === '1' && /^\d\d:\d\d:\d\d/.test(hov2.text), `${hov2.op} · ${hov2.text.slice(0, 30)}`);

    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: 5, y: 5, button: 'none' });
    await sleep(140);
    const sparkGone = await cdp.eval(
      `getComputedStyle(document.querySelector('#monPanel .proc-spark .spark-hover')).opacity`);
    check('指针离开曲线后读数卡片收起', String(sparkGone) === '0', String(sparkGone));

    // 存储页：四块占比条 + 明细表；点一行才去逐页扫「洞」
    await cdp.eval(`document.querySelector('.mon-nav-tab[data-page="storage"]').click()`);
    await waitFor(cdp, `document.querySelectorAll('#spaceBody tr').length > 0`,
      { label: '存储页表格' });
    await settle(cdp);
    const spUi = await cdp.eval(`(() => {
      const rows = document.querySelectorAll('#spaceBody tr');
      const heads = [...document.querySelectorAll('#spaceTable thead th, .space-table thead th')]
        .map(e => e.textContent.trim());
      const cells = rows.length ? [...rows[0].children].map(e => e.textContent.trim()) : [];
      // 每一行的末列都要是相对时间：只看第一行会漏（第一行常常是「从未」）
      const vacAll = [...rows].map(tr => tr.lastElementChild.textContent.trim());
      return {
        segs: document.querySelectorAll('#spaceBar .space-seg').length,
        legend: document.querySelectorAll('#spaceLegend .space-item').length,
        rows: rows.length,
        heads, cells, vacAll,
        diagHidden: document.querySelector('.mon-page[data-page="diag"]').classList.contains('hidden'),
      };
    })()`);
    // 五块：索引 / 数据 / 大字段 / 向量 / WAL。向量是后加的 —— 它不进堆页，
    // 混进「数据」里会让「数据 0 B 却有 2.4MB 向量」这种表彻底看不见。
    check('存储页：空间五块占比 + 五段图例 + 明细行（诊断页已隐藏）',
      spUi.segs === 5 && spUi.legend === 5 && spUi.rows > 0 && spUi.diagHidden,
      `${spUi.segs} 段 / 图例 ${spUi.legend} / ${spUi.rows} 行 / 诊断页隐藏 ${spUi.diagHidden}`);
    // 「上次整理」是这轮的驱动信号：密度 + 多久没整理，才知道该不该动手
    // 末列的数是「多久以前」，三位数以上一定是单位/时间戳搞错了
    // （踩过：把时间戳当成时长算，刚 VACUUM 的表显示「498171 小时前」）
    const vacText = spUi.cells[spUi.cells.length - 1] || '';
    // 列顺序和占比条一致：索引在前、数据在后（两种视图对不上就得来回找）
    check('空间表前两列是「索引」「数据」（和占比条同序）',
      spUi.heads[1] === '索引' && spUi.heads[2] === '数据',
      `第 2/3 列：${spUi.heads[1]} / ${spUi.heads[2]}`);
    // 向量列必须单列一列：它不进堆页，所以「数据」那列永远是 0 B，
    // 不单独显示的话，一张 2.4MB 的向量表在存储页上就是一张空表。
    check('空间表有「向量」列，且排在「大字段」和「行数」之间',
      spUi.heads[3] === '大字段' && spUi.heads[4] === '向量' && spUi.heads[5] === '行数',
      `第 4/5/6 列：${spUi.heads[3]} / ${spUi.heads[4]} / ${spUi.heads[5]}`);
    const vecStat = await cdp.eval(`(async () => {
      const q = (sql) => fetch('/api/query', { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ sql }) }).then(r => r.json());
      await q('DROP TABLE IF EXISTS t_uivec');
      await q('CREATE TABLE t_uivec (id int, v vector(8))');
      await q("INSERT INTO t_uivec VALUES (1,'[1,2,3,4,5,6,7,8]'),(2,'[8,7,6,5,4,3,2,1]')");
      const sp = await fetch('/api/space').then(r => r.json());
      const t = (sp.tables || []).find(x => x.name === 't_uivec');
      await q('DROP TABLE t_uivec');
      return t ? { bytes: t.bytes || 0, vecBytes: t.vecBytes || 0, vecCols: t.vecCols || 0,
        totalVec: (sp.space && sp.space.vecBytes) || 0 } : null;
    })()`);
    check('向量字节进空间统计（.dat 为 0 也要报出 .vec 的实际占用）',
      vecStat && vecStat.bytes === 0 && vecStat.vecBytes > 0 && vecStat.vecCols === 1
        && vecStat.totalVec >= vecStat.vecBytes,
      vecStat ? `数据 ${vecStat.bytes} / 向量 ${vecStat.vecBytes} / ${vecStat.vecCols} 列 / 全库 ${vecStat.totalVec}`
        : '没查到 t_uivec');

    check('空间表有「上次整理」列（表头与单元格对齐，值是「从未 / N 分钟前」这类相对时间）',
      spUi.heads.length === 9 && spUi.heads[8] === '上次整理' &&
      spUi.cells.length === 9 &&
      /^(从未|刚刚|\d+ (秒|分钟|小时|天|个月)前)$/.test(vacText) &&
      spUi.vacAll.every(v => /^(从未|刚刚|\d+ (秒|分钟|小时|天|个月)前)$/.test(v)),
      `${spUi.heads.length} 列 · 末列「${vacText}」· 全部 ${JSON.stringify(spUi.vacAll)}`);

    await cdp.eval(`document.querySelector('#spaceBody tr').click()`);
    // 注意：点一下只让面板「先打开」，页级构成是**随后**逐页扫出来的，
    // 中间会显示「正在逐页统计…」。只等「面板可见」会在扫描没跑完时就去量，
    // 拿到 0 段 0 图例（实测踩过 —— 面板先开、内容后填）。
    await waitFor(cdp, `(() => {
      const d = document.getElementById('spaceDetail');
      if (!d || d.classList.contains('hidden')) return false;
      return d.querySelectorAll('.space-dbar .space-dseg').length === 3 &&
        /VACUUM/.test(d.textContent);
    })()`, { label: '页级构成扫完（3 段 + 回收建议）' });
    await settle(cdp);
    const spDet = await cdp.eval(`(() => {
      const d = document.getElementById('spaceDetail');
      return {
        open: !d.classList.contains('hidden'),
        segs: d.querySelectorAll('.space-dbar .space-dseg').length,
        legend: d.querySelectorAll('.space-legend .space-item').length,
        text: d.textContent,
      };
    })()`);
    check('点表名展开页级构成（已用 / 空闲 / 洞 + 怎么回收）',
      spDet.open && spDet.segs === 3 && spDet.legend === 3 && /VACUUM/.test(spDet.text),
      `${spDet.segs} 段 / 图例 ${spDet.legend} / ${spDet.text.slice(0, 50)}`);

    // 空间地图：库视图 = 矩形树图（面积 = 字节），双击进表视图 = 页网格。
    // 这一页是「大视野」：画布吃满剩余高度，一张 785 页的表要能一屏铺完。
    await cdp.eval(`document.querySelector('.mon-nav-tab[data-page="map"]').click()`);
    await waitFor(cdp, `document.querySelectorAll('#mapTree .tv-cell').length > 0`,
      { label: '空间地图树图' });
    await settle(cdp);
    const tvUi = await cdp.eval(`(() => {
      const cells = [...document.querySelectorAll('#mapTree .tv-cell')];
      const box = document.getElementById('mapTree').getBoundingClientRect();
      const area = cells.reduce((a, c) => {
        const r = c.getBoundingClientRect();
        return a + r.width * r.height;
      }, 0);
      const rail = document.getElementById('mapRail').getBoundingClientRect();
      const bodyMode = document.getElementById('monBody').classList.contains('map-mode');
      return {
        n: cells.length,
        cover: box.width * box.height ? area / (box.width * box.height) : 0,
        named: cells.filter(c => c.querySelector('.tv-name')).length,
        tints: [...new Set(cells.map(c => c.dataset.tint).filter(Boolean))],
        canvas: [Math.round(box.width), Math.round(box.height)],
        railW: Math.round(rail.width),
        railLeft: Math.round(rail.left),
        canvasRight: Math.round(box.right),
        legend: document.querySelectorAll('#pmLegend .space-item').length,
        note: (document.getElementById('mapNote') || {}).textContent || '',
        bodyMode,
      };
    })()`);
    check('空间地图：树图铺满画布（覆盖 > 75%）、块有名字和分类底色',
      tvUi.n > 0 && tvUi.cover > 0.75 && tvUi.named > 0 && tvUi.tints.length > 0,
      `${tvUi.n} 块 / 覆盖 ${(tvUi.cover * 100).toFixed(0)}% / 有名字 ${tvUi.named} / 底色 ${tvUi.tints.join(',')}`);
    check('空间地图：画布占满剩余高度、右栏在画布右侧、图例四类且写了「面积 = 字节」',
      tvUi.canvas[1] > 400 && tvUi.railW >= 360 && tvUi.railLeft >= tvUi.canvasRight - 1 &&
        tvUi.legend === 4 && /面积 = 字节/.test(tvUi.note) && tvUi.bodyMode,
      `画布 ${tvUi.canvas.join('×')} · 右栏 ${tvUi.railW}px · 图例 ${tvUi.legend} · ${tvUi.note}`);

    // 右栏左侧的竖向拖宽条：和编辑器和结果区之间那条是同一套做法（转了 90°）。
    // 原来悬停是把整条 7px 涂成淡蓝，看着像「一条色带」，不像「一处能抓的地方」。
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: 5, y: 5, button: 'none' });
    await sleep(250);
    const vGripIdle = await cdp.eval(`(() => {
      const g = document.getElementById('mapGrip');
      const a = getComputedStyle(g, '::after');
      return { op: getComputedStyle(g, '::before').opacity, w: a.width, h: a.height };
    })()`);
    check('右栏拖宽条常态：渐变柱收起，只剩一小段竖向短条',
      vGripIdle.op === '0' && vGripIdle.w === '3px' && vGripIdle.h === '36px', JSON.stringify(vGripIdle));

    const vPt = await cdp.eval(`(() => {
      const b = document.getElementById('mapGrip').getBoundingClientRect();
      return { x: Math.round(b.left + b.width / 2), y: Math.round(b.top + b.height / 2) };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: vPt.x, y: vPt.y, button: 'none' });
    await sleep(80);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: vPt.x, y: vPt.y, button: 'none' });
    await sleep(260);
    const vGripHover = await cdp.eval(`(() => {
      const g = document.getElementById('mapGrip');
      const a = getComputedStyle(g, '::after');
      const b = getComputedStyle(g, '::before');
      return {
        hovered: g.matches(':hover'),
        lineOp: b.opacity,
        lineFade: /rgba\\(0, 0, 0, 0\\)/.test(b.backgroundImage),
        // 方向别查角度：Chrome 把 180deg 规范成默认方向，计算值里根本不带角度。
        // 查抓手纹理的尺寸更实在 —— 竖版是「1.5×13 的竖条」，横版是「13×1.5 的横条」。
        vBars: /1\\.5px 13px/.test(a.backgroundSize),
        w: a.width, h: a.height, radius: a.borderRadius,
        // 拖宽条本身只有 7px：胶囊 13px 必须拒绝 flex 收缩，否则会被压回 5px
        gripW: Math.round(g.getBoundingClientRect().width),
      };
    })()`);
    check('右栏拖宽条悬停：竖向渐变柱 + 胶囊手柄，胶囊不被 7px 宽压扁',
      vGripHover.hovered && vGripHover.lineOp === '0.9' && vGripHover.lineFade && vGripHover.vBars
        && vGripHover.w === '13px' && vGripHover.h === '44px' && vGripHover.radius === '7px'
        && vGripHover.gripW === 7,
      JSON.stringify(vGripHover));
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: 5, y: 5, button: 'none' });
    await sleep(200);

    // 「按文件」把每棵索引拆成独立块：块数一定变多
    const beforeFile = tvUi.n;
    await cdp.eval(`document.querySelector('#mapGroup button[data-group="file"]').click()`);
    await waitFor(cdp, `(document.querySelector('#mapGroup button.on') || {}).dataset.group === 'file'`,
      { label: '按文件分组生效' });
    await settle(cdp);
    const fileUi = await cdp.eval(`(() => ({
      n: document.querySelectorAll('#mapTree .tv-cell').length,
      on: (document.querySelector('#mapGroup button.on') || {}).dataset.group,
    }))()`);
    check('空间地图「按文件」把索引和数据拆成独立块',
      fileUi.on === 'file' && fileUi.n > beforeFile,
      `按表 ${beforeFile} 块 → 按文件 ${fileUi.n} 块`);
    await cdp.eval(`document.querySelector('#mapGroup button[data-group="table"]').click()`);
    await sleep(500);

    // 缩放：只放大「当前这块区域」—— 块数和面积比例都不变，变的是格子大小。
    // 这条是为了「数据多的时候某些块太小」：放大后小格子够大，名字会回来。
    const z0 = await cdp.eval(`(() => {
      const c = [...document.querySelectorAll('#mapTree .tv-cell')];
      return { n: c.length, big: c.length ? Math.round(Math.max(...c.map(x => x.getBoundingClientRect().width))) : 0 };
    })()`);
    await cdp.eval(`document.getElementById('mapZoomIn').click()`);
    await cdp.eval(`document.getElementById('mapZoomIn').click()`);
    await cdp.eval(`document.getElementById('mapZoomIn').click()`);
    await sleep(600);
    const z1 = await cdp.eval(`(() => {
      const c = [...document.querySelectorAll('#mapTree .tv-cell')];
      const box = document.getElementById('mapTree');
      return {
        label: document.getElementById('mapZoomVal').textContent,
        n: c.length,
        big: c.length ? Math.round(Math.max(...c.map(x => x.getBoundingClientRect().width))) : 0,
        canPan: box.classList.contains('can-pan'),
      };
    })()`);
    check('缩放：块数不变、格子变大、进入可拖动状态',
      z1.n === z0.n && z1.big >= z0.big * 2 && /×/.test(z1.label) && z1.canPan,
      `${z0.big}px → ${z1.big}px（${z1.label}）· ${z1.n} 块`);

    // 拖动平移：拖到边界之外要被夹住，不能把图拖出画面
    const dragBase = await cdp.eval(`(() => {
      const r = document.getElementById('mapTree').getBoundingClientRect();
      return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2),
        left: Math.round(document.querySelector('#mapTree .tv-cell').getBoundingClientRect().left) };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: dragBase.x, y: dragBase.y, button: 'left', clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: dragBase.x + 2000, y: dragBase.y + 2000, button: 'left', buttons: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: dragBase.x + 2000, y: dragBase.y + 2000, button: 'left' });
    await sleep(500);
    const dragUi = await cdp.eval(`(() => {
      const box = document.getElementById('mapTree').getBoundingClientRect();
      const c = document.querySelector('#mapTree .tv-cell').getBoundingClientRect();
      return { dx: Math.round(c.left - box.left), dy: Math.round(c.top - box.top) };
    })()`);
    check('放大后能拖动，拖到边界就夹住（不会把图拖出画面）',
      dragUi.dx === 0 && dragUi.dy === 0,
      `拖动后首块偏移 ${dragUi.dx},${dragUi.dy}（应为 0,0）`);

    await cdp.eval(`document.getElementById('mapZoomReset').click()`);
    await sleep(600);
    const zReset = await cdp.eval(`(() => ({
      label: document.getElementById('mapZoomVal').textContent,
      left: Math.round(document.querySelector('#mapTree .tv-cell').getBoundingClientRect().left -
        document.getElementById('mapTree').getBoundingClientRect().left),
      canPan: document.getElementById('mapTree').classList.contains('can-pan'),
    }))()`);
    check('「1:1」复位：回到 1.0× 且位置归零',
      /^1\.0×$/.test(zReset.label) && zReset.left === 0 && !zReset.canPan,
      `${zReset.label} · 偏移 ${zReset.left}`);

    // 平滑：滚一格不是瞬间跳到位，而是逐帧滑过去（每帧记一次 mapZoom，看有没有中间值）
    await cdp.eval(`window.__zv=[];(function s(){window.__zv.push(mapZoom);if(window.__zv.length<45)requestAnimationFrame(s);})()`);
    await cdp.send('Input.dispatchMouseEvent', {
      type: 'mouseWheel', x: dragBase.x, y: dragBase.y, deltaX: 0, deltaY: -120,
    });
    await sleep(800);
    const zSeries = await cdp.eval(`window.__zv`);
    const zFirst = zSeries[0];
    const zLast = zSeries[zSeries.length - 1];
    const zMid = zSeries.filter(v => v > zFirst + 1e-3 && v < zLast - 1e-3).length;
    check('滚轮缩放是「滑过去」的（逐帧有中间值，不是一步跳到位）',
      zLast > zFirst + 0.05 && zMid >= 3,
      `${zFirst.toFixed(3)} → ${zLast.toFixed(3)}，中间帧 ${zMid} 个`);

    // 惯性：快速拖一把松手，画面还会继续滑一段然后停住
    await cdp.eval(`setMapZoom(4, null)`);
    await sleep(700);
    const ipos = `(() => { const c = document.querySelector('#mapTree .tv-cell').getBoundingClientRect();
      return { left: Math.round(c.left), top: Math.round(c.top) }; })()`;
    const glide0 = await cdp.eval(ipos);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: dragBase.x, y: dragBase.y, button: 'left', clickCount: 1 });
    for (let i = 1; i <= 4; i++) {
      await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: dragBase.x + 45 * i, y: dragBase.y, button: 'left', buttons: 1 });
      await sleep(16);
    }
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: dragBase.x + 180, y: dragBase.y, button: 'left' });
    const glideRel = await cdp.eval(ipos);
    await sleep(300);
    const glideMid = await cdp.eval(ipos);
    await sleep(1500);                 // 衰减尾巴要走一会儿，别在滑行中途取样
    const glideEnd = await cdp.eval(ipos);
    await sleep(500);
    const glideRest = await cdp.eval(ipos);
    check('松手后有惯性滑行，随后自己停住',
      glideRel.left > glide0.left && glideMid.left > glideRel.left + 10 &&
        glideEnd.left >= glideMid.left && glideRest.left === glideEnd.left,
      `拖到 ${glideRel.left} → 滑到 ${glideMid.left} → 停在 ${glideEnd.left}（再看 ${glideRest.left}）`);

    // 双击一块进表视图：页网格。
    // 别拿「第一块」—— 树图里还有 WAL 和「其它」两种块，它们不挂双击，
    // 基准跑完 WAL 涨到几十 MB 就会排在最前面，双击它什么也不会发生。
    await cdp.eval(`(() => {
      // 取面积最大的「可下钻」块：跳过 WAL 块和「其它」块（它们不挂双击）。
      // 用面积排序而不是取第一个，是为了跟原来的语义一致 —— 双击最显眼的那块。
      const cells = [...document.querySelectorAll('#mapTree .tv-cell')].filter(x =>
        !x.classList.contains('other')
        && !/^WAL$/.test(((x.querySelector('.tv-name') || {}).textContent || '').trim()));
      const area = x => { const r = x.getBoundingClientRect(); return r.width * r.height; };
      cells.sort((a, b) => area(b) - area(a));
      cells[0].dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    })()`);
    await waitFor(cdp, `document.querySelectorAll('#pmGrid .pm-c').length > 0`,
      { label: '表视图页网格' });
    await settle(cdp);
    const gridUi = await cdp.eval(`(() => {
      const cells = [...document.querySelectorAll('#pmGrid .pm-c')];
      const g = document.getElementById('pmGrid').getBoundingClientRect();
      return {
        n: cells.length,
        cell: cells.length ? Math.round(cells[0].getBoundingClientRect().width) : 0,
        h: Math.round(g.height),
        w: Math.round(g.width),
        crumbs: (document.getElementById('mapCrumbs') || {}).textContent || '',
        files: [...document.querySelectorAll('#mapFiles .map-file')].map(b => b.textContent),
        note: (document.getElementById('mapNote') || {}).textContent || '',
        treeHidden: document.getElementById('mapTree').classList.contains('hidden'),
      };
    })()`);
    check('双击树图块进表视图：页网格吃满画布（高 > 300），树图收起',
      gridUi.n > 0 && gridUi.h > 300 && gridUi.treeHidden && /数据页/.test(gridUi.crumbs) && gridUi.files.length >= 1,
      `${gridUi.n} 格 / 格 ${gridUi.cell}px / 画布 ${gridUi.w}×${gridUi.h} / ${gridUi.crumbs}`);

    // 页网格也能放大：格子变大、开始滚动（页特别多时格子会掉到 5px 下限）
    const gz0 = await cdp.eval(`(() => {
      const g = document.getElementById('pmGrid');
      const c = document.querySelector('#pmGrid .pm-c').getBoundingClientRect();
      return { cell: Math.round(c.width), zoom: document.getElementById('mapZoomVal').textContent,
        sw: g.scrollWidth, cw: g.clientWidth, sh: g.scrollHeight, ch: g.clientHeight };
    })()`);
    await cdp.eval(`document.getElementById('mapZoomIn').click()`);
    await cdp.eval(`document.getElementById('mapZoomIn').click()`);
    await sleep(600);
    const gz1 = await cdp.eval(`(() => {
      const g = document.getElementById('pmGrid');
      const c = document.querySelector('#pmGrid .pm-c').getBoundingClientRect();
      return { cell: Math.round(c.width), scrollable: g.scrollHeight > g.clientHeight + 2 ||
        g.scrollWidth > g.clientWidth + 2 };
    })()`);
    check('进表后缩放归零；页网格放大时格子变大并开始滚动',
      /^1\.0×$/.test(gz0.zoom) && gz1.cell > gz0.cell && gz1.scrollable
      && gz0.sw <= gz0.cw + 1 && gz0.sh <= gz0.ch + 1,
      `进表 ${gz0.zoom} · 格子 ${gz0.cell} → ${gz1.cell}px · 可滚 ${gz1.scrollable}`
      + ` · 1× 内容 ${gz0.sw}×${gz0.sh} / 视口 ${gz0.cw}×${gz0.ch}`);
    await cdp.eval(`document.getElementById('mapZoomReset').click()`);
    await sleep(400);

    // 页网格里的手势：**普通滚轮就是缩放**（用户报过「第二层级的小方格无法缩放」——
    // 当时把网格的滚轮留给了滚动，而网格在 1× 时正好铺满、根本没得滚，看着就像没反应）。
    const gbox = await cdp.eval(`(() => {
      const r = document.getElementById('pmGrid').getBoundingClientRect();
      return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseWheel', x: gbox.x, y: gbox.y, deltaX: 0, deltaY: -120 });
    await sleep(300);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseWheel', x: gbox.x, y: gbox.y, deltaX: 0, deltaY: -120 });
    await sleep(600);
    const gImg = await cdp.eval(`(() => {
      const g = document.getElementById('pmGrid');
      const c = document.querySelector('#pmGrid .pm-c').getBoundingClientRect();
      return { zoom: document.getElementById('mapZoomVal').textContent, cell: Math.round(c.width),
        st: Math.round(g.scrollTop), canPan: g.classList.contains('can-pan'),
        sw: g.scrollWidth, cw: g.clientWidth };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: gbox.x, y: gbox.y, button: 'left', clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: gbox.x, y: gbox.y - 120, button: 'left', buttons: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: gbox.x, y: gbox.y - 120, button: 'left' });
    await sleep(500);
    const gPan = await cdp.eval(`(() => {
      const g = document.getElementById('pmGrid');
      return { st: Math.round(g.scrollTop),
        rail: (document.querySelector('#pmSide .pm-head') || { textContent: '' }).textContent };
    })()`);
    check('页网格：滚轮直接放大、按住拖动平移（拖动不会误开某一页）',
      /×/.test(gImg.zoom) && gImg.cell > 0 && gImg.canPan && gPan.st > gImg.st && gPan.rail === '',
      `${gImg.zoom} · 格子 ${gImg.cell}px · 滚动 ${gImg.st} → ${gPan.st} · 右栏「${gPan.rail || '空'}」`);

    /* 横向也要能拖。用户报「拖动只能上下、不能左右」—— 根因是网格原来用 flex-wrap：
       换行时内容宽度恒等于容器宽度，横向根本没有可滚区域，放大只是行数变多。
       改成固定列数的 grid 后整张图等比撑开，两边都得能动。 */
    await cdp.eval(`document.getElementById('pmGrid').scrollLeft = 0`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: gbox.x, y: gbox.y, button: 'left', clickCount: 1 });
    for (let i = 1; i <= 3; i++) {
      await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: gbox.x - 50 * i, y: gbox.y, button: 'left', buttons: 1 });
      await sleep(16);
    }
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: gbox.x - 150, y: gbox.y, button: 'left' });
    await sleep(1400);                 // 等惯性尾巴走完，否则读到的是滑行中间值
    const gPanX = await cdp.eval(`(() => {
      const g = document.getElementById('pmGrid');
      return { sl: Math.round(g.scrollLeft), sw: g.scrollWidth, cw: g.clientWidth,
        cols: getComputedStyle(g).gridTemplateColumns.split(' ').length };
    })()`);
    check('页网格放大后横向也能拖（列数固定、内容等比撑开）',
      gImg.sw > gImg.cw + 2 && gPanX.sl > 0 && gPanX.sw > gPanX.cw + 2,
      `内容 ${gImg.sw}px / 视口 ${gImg.cw}px · ${gPanX.cols} 列 · scrollLeft 0 → ${gPanX.sl}`);
    await cdp.eval(`document.getElementById('mapZoomReset').click()`);
    await sleep(400);

    /* 滚轮输入整形：一次手势的尾巴常带几个反向的小 delta，逐个取指数就会「放大着放大着
       又缩一下」。这里直接喂事件对象：同向放大 → 反向小尾巴 → 真回头，看后两个判得对不对。 */
    const zsh = await cdp.eval(`(() => {
      const r = document.getElementById('pmGrid').getBoundingClientRect();
      const ev = dy => ({ deltaY: dy, deltaMode: 0, clientX: r.left + 40, clientY: r.top + 40 });
      const up = wheelZoom(ev(-120));      // 用户往上滚：放大
      const tail = wheelZoom(ev(6));       // 尾巴：反向、幅度小、紧跟上一拍
      const back = wheelZoom(ev(120));     // 用户真往回滚：该缩
      return { up, tail, back };
    })()`);
    check('滚轮缩放手势整形：反向小尾巴被丢掉，真回头才缩',
      zsh.up === true && zsh.tail === false && zsh.back === true,
      `放大 ${zsh.up} · 尾巴 ${zsh.tail}（应为 false）· 回头 ${zsh.back}`);
    await cdp.eval(`document.getElementById('mapZoomReset').click()`);
    await sleep(400);

    /* 放大过程中「只进不退」，而且不能重建格子：
       - 回退的根因是滚动条一出现就压缩 clientWidth，基准格子跟着变小 → 内容收敛 →
         滚动条消失 → 又变大，卡在临界点来回横跳。现在基准只乘倍数，不再按当帧宽度重算。
       - 重建的根因是每帧 innerHTML=''，清空那一帧 scrollWidth 塌成 0，scrollLeft 被夹回去。 */
    await cdp.eval(`(() => {
      const g = document.getElementById('pmGrid');
      g.scrollTop = 0; g.scrollLeft = 0;
      window.__z0 = g.children[0];
      window.__zt = [];
      (function s(n) {
        const c = document.querySelector('#pmGrid .pm-c');
        window.__zt.push({
          z: Math.round(mapZoom * 1000) / 1000,
          cell: c ? Math.round(c.getBoundingClientRect().width) : 0,
          same: c === window.__z0,
        });
        if (n < 42) requestAnimationFrame(() => s(n + 1));
      })(0);
    })()`);
    for (let i = 0; i < 3; i++) {
      await cdp.send('Input.dispatchMouseEvent', { type: 'mouseWheel', x: gbox.x, y: gbox.y, deltaX: 0, deltaY: -120 });
      await sleep(140);
    }
    await sleep(700);
    const zt = await cdp.eval(`window.__zt`);
    let zBack = 0, zShrunk = 0, zRebuilt = 0;
    for (let i = 1; i < zt.length; i++) {
      if (zt[i].z < zt[i - 1].z - 1e-6) zBack++;
      if (zt[i].cell < zt[i - 1].cell) zShrunk++;
      if (!zt[i].same) zRebuilt++;
    }
    check('放大过程中倍数/格子只增不减，且格子 DOM 全程没被重建',
      zt[zt.length - 1].z > zt[0].z + 0.2 && zBack === 0 && zShrunk === 0 && zRebuilt === 0,
      `${zt[0].z}× → ${zt[zt.length - 1].z}× · 格子 ${zt[0].cell} → ${zt[zt.length - 1].cell}px`
      + ` · 回退 ${zBack} 帧 / 变小 ${zShrunk} 帧 / 重建 ${zRebuilt} 帧`);
    await cdp.eval(`document.getElementById('mapZoomReset').click()`);
    await sleep(400);

    // 页检查器：点一格看结构 / 文本 / 十六进制（直接读文件，不进缓冲池）
    await cdp.eval(`document.querySelector('#pmGrid .pm-c').click()`);
    await waitFor(cdp, `(() => { const h = document.querySelector('#pmSide .pm-head');
      return !!h && /第 \\d+ 页/.test(h.textContent); })()`, { label: '页检查器结构面板' });
    const pmStruct = await cdp.eval(`(() => {
      const side = document.getElementById('pmSide');
      const rows = [...side.querySelectorAll('.pm-row')].map(r => r.textContent);
      return {
        head: (side.querySelector('.pm-head') || {}).textContent || '',
        rows: rows.length,
        sample: rows.slice(0, 2).join(' / '),
        tabs: [...side.querySelectorAll('.pm-tab')].map(b => b.textContent).join(','),
      };
    })()`);
    check('点一格能看到页结构与元组（含 RID）',
      pmStruct.rows > 0 && /结构,文本,十六进制/.test(pmStruct.tabs) && /第 \d+ 页/.test(pmStruct.head),
      `${pmStruct.head} · ${pmStruct.rows} 条 · ${pmStruct.sample.slice(0, 40)}`);

    // 关联链：点一条行（不论是从数据页还是索引叶子点），应该出现「RID → 堆页 N 槽 M」
    await cdp.eval(`document.querySelector('#pmSide .pm-row').click()`);
    await waitFor(cdp, `!!document.querySelector('#pmSide .pm-link')`,
      { label: 'RID 关联块' });
    const pmLink = await cdp.eval(`(() => {
      const box = document.querySelector('#pmSide .pm-link');
      return { text: box ? box.textContent : '', onRow: document.querySelectorAll('#pmSide .pm-row.on').length };
    })()`);
    check('点一行能定位到「堆页 N 槽 M」（关联链的第一跳）',
      /RID \d+ → 堆页 \d+ 槽 \d+/.test(pmLink.text),
      pmLink.text.slice(0, 60) || '没有关联块');

    /* 索引树能一路下钻：头页（root/高度）→ 根 → 内部节点 → 叶子。
       B+Tree 在磁盘上是一棵真的树，但页网格里它只是 212 个同色的方格 ——
       不能从根顺着指针走一遍，就永远只是在看一堆页。路径条同时解决「我在第几层」。 */
    // 选索引最大的那张表：索引越大越可能已经长出内部节点（高度 ≥ 2），
    // 才能覆盖「根 → 内部 → 叶子」三级；小表的根直接就是叶子，只能测两级。
    // （原来写的是「第一张有索引的表」，库里多删两张表之后它就换人了，断言跟着挂。）
    const ixNav = await cdp.eval(`(() => {
      const ts = (spaceData.tables || []).filter(x => (x.indexes || []).length)
        .sort((a, b) => (b.indexBytes || 0) - (a.indexBytes || 0));
      return ts.length ? { table: ts[0].name, ix: ts[0].indexes[0].name } : null;
    })()`);
    if (ixNav) {
      await cdp.eval(`enterTable(${JSON.stringify(ixNav.table)}, 'index', ${JSON.stringify(ixNav.ix)})`);
      await waitFor(cdp, `!!document.querySelector('#pmGrid .pm-c[data-no="0"]')`,
        { label: '索引页网格' });
      // 右栏可能还停在上一条断言切的「十六进制」页，先回结构页 —— 结构页才有 RID
      await cdp.eval(`pmTab = 'struct'`);
      await cdp.eval(`document.querySelector('#pmGrid .pm-c[data-no="0"]').click()`);
      await waitFor(cdp, `!!document.querySelector('#pmSide .pm-path')`,
        { label: '索引头页路径条' });
      const idxHead = await cdp.eval(`(() => ({
        path: (document.querySelector('#pmSide .pm-path') || {}).textContent || '',
        rootLink: document.querySelectorAll('#pmSide .pm-row.linkable').length,
      }))()`);
      await cdp.eval(`document.querySelector('#pmSide .pm-row.linkable').click()`);
      // 等路径条从「头页 0」往下走一层（比「睡 1 秒」准：这一跳要读页）
      await waitFor(cdp, `(() => { const p = document.querySelector('#pmSide .pm-path');
        return !!p && p.textContent.trim() !== '头页 0'; })()`, { label: '路径条下钻一层' });
      const idxRoot = await cdp.eval(`(() => ({
        path: (document.querySelector('#pmSide .pm-path') || {}).textContent || '',
        inner: /内部节点/.test(document.getElementById('pmSide').textContent),
        links: document.querySelectorAll('#pmSide .pm-row.linkable').length,
      }))()`);
      // 根是内部节点才再下一层；小表的根本身就是叶子
      let idxLeaf = { path: idxRoot.path };
      if (idxRoot.inner) {
        // 记下当前路径，要求它真的变了（叶子这一层是「内部 N›叶子 M」）
        const beforeLeaf = (idxRoot.path || '').trim();
        await cdp.eval(`document.querySelector('#pmSide .pm-row.linkable').click()`);
        await waitFor(cdp, `(() => { const p = document.querySelector('#pmSide .pm-path');
          if (!p) return false;
          const t = p.textContent.trim();
          return t !== '头页 0' && t !== ${JSON.stringify(beforeLeaf)}; })()`,
          { label: '路径条再下一层' });
        idxLeaf = await cdp.eval(`(() => ({
          path: (document.querySelector('#pmSide .pm-path') || {}).textContent || '',
          leaf: /叶子/.test(document.getElementById('pmSide').textContent),
          rid: document.getElementById('pmSide').textContent.includes('RID '),
        }))()`);
      }
      const deepOk = idxRoot.inner
        ? (/^头页 0›内部 \d+›叶子 \d+$/.test(idxLeaf.path) && idxLeaf.leaf && idxLeaf.rid)
        : /^头页 0›叶子 \d+$/.test(idxRoot.path);
      check('索引树能从根一路下钻到叶子，路径条标出层级',
        idxHead.path === '头页 0' && idxHead.rootLink === 1
        && /^头页 0›(内部|叶子) \d+$/.test(idxRoot.path) && deepOk,
        `${idxHead.path} → ${idxRoot.path}` + (idxRoot.inner ? ` → ${idxLeaf.path}` : '（根即叶子）'));
      await cdp.eval(`document.querySelector('#pmSide .pm-path-i').click()`);
      await waitFor(cdp, `(() => { const p = document.querySelector('#pmSide .pm-path');
        return !!p && p.textContent.trim() === '头页 0'; })()`, { label: '退回头页' });
      const idxBack = await cdp.eval(
        `(document.querySelector('#pmSide .pm-path') || {}).textContent || ''`);
      check('点路径条能退回上一层（不用重新点网格）', idxBack === '头页 0', idxBack || '路径条没了');
    }

    // 整条链：索引叶子 → RID → 堆页（换文件、高亮、关联块三件事必须一起发生）
    const hasIx = await cdp.eval(`(() => {
      const t = (spaceData.tables || []).find(x => (x.indexes || []).length);
      return t ? { table: t.name, ix: t.indexes[0].name } : null;
    })()`);
    if (hasIx) {
      await cdp.eval(`enterTable(${JSON.stringify(hasIx.table)}, 'index', ${JSON.stringify(hasIx.ix)})`);
      await waitFor(cdp, `document.querySelectorAll('#pmGrid .pm-c').length > 0`,
        { label: '索引页网格（整链）' });
      const idxPage = await cdp.eval(`(() => {
        const c = [...document.querySelectorAll('#pmGrid .pm-c')].find(x => x.classList.contains('idx'));
        if (!c) return null;
        c.click();
        return c.dataset.no;
      })()`);
      await waitFor(cdp, `!!document.querySelector('#pmSide .pm-row.linkable')`,
        { label: '索引页里的可点行' });
      await cdp.eval(`document.querySelector('#pmSide .pm-row.linkable').click()`);
      // 这一跳要换文件 + 读堆页，等「关联块里出现堆页」再收尾
      await waitFor(cdp, `(() => { const t = document.querySelector('#pmSide .pm-link');
        return !!t && /堆页/.test(t.textContent); })()`, { label: '跳到堆页' });
      const chain = await cdp.eval(`(() => {
        const txt = s => { const e = document.querySelector(s); return e ? e.textContent : ''; };
        const on = document.querySelector('#mapFiles .map-file.on');
        return {
          file: on ? on.textContent : '',
          crumbs: txt('#mapCrumbs'),
          link: txt('#pmSide .pm-link'),
          onCell: document.querySelectorAll('#pmGrid .pm-c.on').length,
        };
      })()`);
      check('索引叶子点 RID：自动切到「数据页」、只高亮 1 格、关联块还在',
        idxPage !== null && chain.file === '数据页' && /数据页/.test(chain.crumbs) &&
          chain.onCell === 1 && /RID \d+ → 堆页 \d+ 槽 \d+/.test(chain.link),
        `索引页 ${idxPage} → ${chain.file} · 高亮 ${chain.onCell} 格 · ${chain.link.slice(0, 40) || '关联块丢了'}`);
    } else {
      check('索引叶子点 RID：自动切到「数据页」、只高亮 1 格、关联块还在', true, '这个库里没有索引，跳过');
    }

    // 文本视图：页里能认出业务文本（这个库的示例数据是中文）
    await cdp.eval(`[...document.querySelectorAll('#pmSide .pm-tab')].find(b => b.textContent === '文本').click()`);
    await sleep(400);
    const pmText = await cdp.eval(`(() => ({
      n: document.querySelectorAll('#pmSide .pm-text').length,
      first: (document.querySelector('#pmSide .pm-text .s') || {}).textContent || '',
    }))()`);
    check('页检查器有「文本」视图（从字节里认出业务文本）',
      pmText.n > 0, `${pmText.n} 段 · 首段「${pmText.first.slice(0, 12)}」`);

    // 十六进制视图：整页 8KB = 512 行，命中文本的行要高亮
    await cdp.eval(`[...document.querySelectorAll('#pmSide .pm-tab')].find(b => b.textContent === '十六进制').click()`);
    await sleep(500);
    const pmHex = await cdp.eval(`(() => {
      const lines = document.querySelectorAll('#pmSide .pm-hex .ln');
      return {
        n: lines.length,
        hl: document.querySelectorAll('#pmSide .pm-hex .ln.hl').length,
        first: lines.length ? lines[0].textContent.slice(0, 24) : '',
      };
    })()`);
    check('十六进制视图渲染整页 512 行，并高亮文本片段',
      pmHex.n === 512 && pmHex.hl > 0, `${pmHex.n} 行 / 高亮 ${pmHex.hl} 行 / ${pmHex.first}`);

    // 一层一层退：返回键 → 库视图；Esc 在表视图里只退一级
    await cdp.eval(`document.getElementById('mapBack').click()`);
    await sleep(800);
    const backUi = await cdp.eval(`(() => ({
      cells: document.querySelectorAll('#mapTree .tv-cell').length,
      crumbs: (document.getElementById('mapCrumbs') || {}).textContent || '',
      gridHidden: document.getElementById('pmGrid').classList.contains('hidden'),
    }))()`);
    check('点返回：回到库视图（树图重新铺开、页网格收起）',
      backUi.cells > 0 && backUi.gridHidden && !/数据页/.test(backUi.crumbs),
      `${backUi.cells} 块 / ${backUi.crumbs}`);

    // 同上：挑面积最大的可下钻块，WAL / 「其它」块不挂双击
    await cdp.eval(`(() => {
      const cells = [...document.querySelectorAll('#mapTree .tv-cell')].filter(x =>
        !x.classList.contains('other')
        && !/^WAL$/.test(((x.querySelector('.tv-name') || {}).textContent || '').trim()));
      const area = x => { const r = x.getBoundingClientRect(); return r.width * r.height; };
      cells.sort((a, b) => area(b) - area(a));
      cells[0].dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    })()`);
    await waitFor(cdp, `document.querySelectorAll('#pmGrid .pm-c').length > 0`,
      { label: '表视图页网格（Esc 用例）' });
    await cdp.eval(`document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))`);
    await settle(cdp);
    const escUi = await cdp.eval(`(() => ({
      panelOpen: !document.getElementById('monPanel').classList.contains('hidden'),
      gridHidden: document.getElementById('pmGrid').classList.contains('hidden'),
      crumbs: (document.getElementById('mapCrumbs') || {}).textContent || '',
    }))()`);
    check('Esc 在表视图里只退一级，不会顺手关掉整个面板',
      escUi.panelOpen && escUi.gridHidden && !/数据页/.test(escUi.crumbs),
      `面板${escUi.panelOpen ? '还开着' : '被关了'} · ${escUi.crumbs}`);

    // 诊断页：空间已经移走，只剩慢SQL / 慢事务 / 锁明细
    await cdp.eval(`document.querySelector('.mon-nav-tab[data-page="diag"]').click()`);
    await sleep(700);
    const diagUi = await cdp.eval(`(() => {
      const tabs = [...document.querySelectorAll('#monTabs .mon-tab')];
      return { n: tabs.length, labels: tabs.map(b => b.dataset.tab).join(',') };
    })()`);
    check('诊断页只剩三类诊断（空间已移到「存储」页）',
      diagUi.n === 3 && diagUi.labels === 'sql,tx,lock', `${diagUi.n} 个：${diagUi.labels}`);

    await cdp.eval(`document.querySelector('.mon-nav-tab[data-page="overview"]').click()`);
    await sleep(400);
    await cdp.eval(`document.querySelector('#monSlowSQL tr').click()`);
    await sleep(700);
    const detUi = await cdp.eval(`(() => {
      const m = document.getElementById('slowModal');
      return {
        open: !m.classList.contains('hidden'),
        stats: m.querySelectorAll('.sm-stats .sm-stat').length,
        // 新详情：一行是「语句结构」，卡片给次数/累计/平均/最大，下面挂明细表
        stats_ok: /执行次数/.test(m.textContent) && /累计耗时/.test(m.textContent),
        detail: /最慢的/.test(m.textContent) && !!m.querySelector('#smDetHead th'),
        sql: (m.querySelector('#smSQL') || {}).textContent || '',
      };
    })()`);
    check('点慢SQL行打开详情（统计卡 + 结构 SQL + 明细表）',
      detUi.open && detUi.stats >= 4 && detUi.stats_ok && detUi.detail && detUi.sql.length > 0,
      `${detUi.stats} 张卡 / 明细 ${detUi.detail} / ${detUi.sql.slice(0, 24)}`);
    await cdp.eval(`document.getElementById('btnSlowDone').click()`);

    // 字号调到最大再看面板：.mon-card 是 overflow:hidden，按 flex 规范它的自动
    // 最小尺寸会退化成 0 —— 纵向空间不够时 flex 不滚动而是把卡片压成 0 高，
    // 内容被裁掉且滚不出来（表现就是「下面显示不全、页面不能滚」）。
    await cdp.eval(`applyFontScale(165, true)`);
    await sleep(500);
    // 三个页面各测一遍：隐藏页的卡片高度当然是 0，混在一起数会把「没显示」当成「被压扁」
    const fits = [];
    for (const page of ['overview', 'storage', 'diag']) {
      await cdp.eval(`document.querySelector('.mon-nav-tab[data-page="${page}"]').click()`);
      await sleep(450);
      const r = await cdp.eval(`(() => {
        const b = document.getElementById('monBody');
        const cards = [...document.querySelectorAll('.mon-page:not(.hidden) .mon-card')];
        b.scrollTop = 999999;
        const last = cards.length ? cards[cards.length - 1].getBoundingClientRect() : null;
        return {
          n: cards.length,
          crushed: cards.filter(c => c.getBoundingClientRect().height < 20).length,
          scrollTop: Math.round(b.scrollTop),
          max: Math.round(b.scrollHeight - b.clientHeight),
          lastBottom: last ? Math.round(last.bottom) : 0,
          panelBottom: Math.round(document.getElementById('monPanel').getBoundingClientRect().bottom),
        };
      })()`);
      fits.push(Object.assign({ page }, r));
    }
    const badPage = fits.find(f => f.n === 0 || f.crushed > 0 || f.scrollTop < f.max - 2 ||
      f.lastBottom > f.panelBottom + 1);
    check('165% 字号：三页都能滚到底、卡片不被压扁',
      !badPage,
      badPage ? `${badPage.page}: 卡片 ${badPage.n} 张（压扁 ${badPage.crushed}）· 滚到 ${badPage.scrollTop}/${badPage.max}`
        : fits.map(f => `${f.page} ${f.n} 张`).join(' / '));

    // 空间地图不靠卡片高度，靠「画布吃满剩余高度」——单独量：画布不能塌、页面不能溢出。
    await cdp.eval(`document.querySelector('.mon-nav-tab[data-page="map"]').click()`);
    await sleep(700);
    const mapFit = await cdp.eval(`(() => {
      const b = document.getElementById('monBody');
      const t = document.getElementById('mapTree').getBoundingClientRect();
      const foot = document.getElementById('mapFoot').getBoundingClientRect();
      return {
        treeH: Math.round(t.height),
        footBottom: Math.round(foot.bottom),
        panelBottom: Math.round(document.getElementById('monPanel').getBoundingClientRect().bottom),
        overflow: Math.round(b.scrollHeight - b.clientHeight),
        cells: document.querySelectorAll('#mapTree .tv-cell').length,
      };
    })()`);
    check('165% 字号：空间地图的画布仍然吃满、不溢出面板',
      mapFit.treeH >= 200 && mapFit.cells > 0 && mapFit.footBottom <= mapFit.panelBottom + 1 &&
        mapFit.overflow <= 1,
      `树图高 ${mapFit.treeH} · ${mapFit.cells} 块 · 溢出 ${mapFit.overflow}px`);
    await cdp.eval(`document.querySelector('.mon-nav-tab[data-page="overview"]').click()`);
    await cdp.eval(`applyFontScale(120, true)`);
    await sleep(300);

    await cdp.eval(`document.getElementById('btnMonClose').click()`);
    await sleep(400);

    // 0.5) 编辑器高度：默认够高、能拖、双击回自动
    check('CodeMirror 已接管编辑区（语法高亮 + 补全）',
      await cdp.eval(`!!document.querySelector('.CodeMirror') && !!window.CodeMirror`),
      await cdp.eval(`document.querySelector('.CodeMirror') ? document.querySelector('.CodeMirror').className : '无'`));

    // 输入层（隐藏的 textarea / contenteditable）要声明成拉丁输入：
    // 手机弹英文键盘，且别把 select 自动首字母大写成 Select
    const ime = await cdp.eval(`(() => {
      const f = cm.getInputField();
      const g = k => f.getAttribute(k);
      return {
        tag: f.tagName.toLowerCase(),
        inputmode: g('inputmode'),
        autocap: g('autocapitalize'),
        autocorrect: g('autocorrect'),
        spell: g('spellcheck'),
      };
    })()`);
    check('编辑器输入层声明为英文键盘、关掉自动大写/纠错',
      ime.inputmode === 'latin' && ime.autocap === 'off' &&
        ime.autocorrect === 'off' && ime.spell === 'false',
      `<${ime.tag}> inputmode=${ime.inputmode} autocapitalize=${ime.autocap} autocorrect=${ime.autocorrect} spellcheck=${ime.spell}`);
    const eh0 = await cdp.eval(`Math.round(document.querySelector('.CodeMirror').getBoundingClientRect().height)`);
    check('编辑器默认高度够用（≥170px）', eh0 >= 170, `${eh0}px`);

    const er = await cdp.eval(`(() => {
      const g = document.getElementById('editorResize').getBoundingClientRect();
      return { x: g.left + g.width / 2, y: g.top + g.height / 2 };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: er.x, y: er.y, button: 'left', buttons: 1, clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: er.x, y: er.y + 40, button: 'left', buttons: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: er.x, y: er.y + 80, button: 'left', buttons: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: er.x, y: er.y + 80, button: 'left', buttons: 0, clickCount: 1 });
    await sleep(200);
    const eh1 = await cdp.eval(`Math.round(document.querySelector('.CodeMirror').getBoundingClientRect().height)`);
    // 高度有上限（innerHeight - 320，给结果区留位），拖到顶就不该再涨：
    // 期望值按「min(起点+80, 上限)」算，有空间时必须真的长大了才算过
    const maxEh = await cdp.eval(`Math.max(180, window.innerHeight - 320)`);
    const wantEh = Math.min(eh0 + 80, maxEh);
    const hasRoom = eh0 + 60 <= maxEh;
    check('拖编辑器底边能加高',
      Math.abs(eh1 - wantEh) < 3 && (!hasRoom || eh1 > eh0 + 40),
      `${eh0}px → ${eh1}px（目标 ${wantEh}，上限 ${maxEh}）`);

    // 高度应当被记住：刷新后还是这个高度（这是「拖完下次打开不用再拖」的承诺）
    check('高度写进 localStorage',
      await cdp.eval(`localStorage.getItem('godb.editorH') === '${eh1}'`),
      await cdp.eval(`String(localStorage.getItem('godb.editorH'))`));
    await cdp.send('Page.reload');
    await sleep(2200);
    const ehReload = await cdp.eval(`Math.round(document.querySelector('.CodeMirror').getBoundingClientRect().height)`);
    check('刷新后仍保持拖过的高度', Math.abs(ehReload - eh1) < 3, `${eh1}px → ${ehReload}px`);

    // 双击拖手：回到自动高度并清掉记忆
    const er2 = await cdp.eval(`(() => {
      const g = document.getElementById('editorResize').getBoundingClientRect();
      return { x: g.left + g.width / 2, y: g.top + g.height / 2 };
    })()`);
    for (const cc of [1, 2]) {
      await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: er2.x, y: er2.y, button: 'left', buttons: 1, clickCount: cc });
      await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: er2.x, y: er2.y, button: 'left', buttons: 0, clickCount: cc });
    }
    await sleep(250);
    check('双击拖手回到自动高度',
      await cdp.eval(`localStorage.getItem('godb.editorH') === null &&
        Math.round(document.querySelector('.CodeMirror').getBoundingClientRect().height) < ${eh1}`));

    // 拖手的悬停观感：常态是一条淡线 + 小短条，移上去才「点亮」成渐变线 + 胶囊手柄。
    // 这不是纯装饰 —— 分隔条只有 11px 高，没有任何提示的话根本看不出这条缝能拖。
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: 5, y: 5, button: 'none' });
    await sleep(250);
    const gripIdle = await cdp.eval(`(() => {
      const g = document.getElementById('editorResize');
      const a = getComputedStyle(g, '::after');
      return { op: getComputedStyle(g, '::before').opacity, w: a.width, h: a.height };
    })()`);
    check('拖手常态：渐变线收起，只剩一小段淡色短条',
      gripIdle.op === '0' && gripIdle.w === '36px' && gripIdle.h === '3px', JSON.stringify(gripIdle));

    const gripPt = await cdp.eval(`(() => {
      const b = document.getElementById('editorResize').getBoundingClientRect();
      return { x: Math.round(b.left + b.width * 0.5), y: Math.round(b.top + b.height / 2) };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: gripPt.x, y: gripPt.y, button: 'none' });
    await sleep(80);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: gripPt.x, y: gripPt.y, button: 'none' });
    await sleep(260);
    const gripHover = await cdp.eval(`(() => {
      const g = document.getElementById('editorResize');
      const a = getComputedStyle(g, '::after');
      const b = getComputedStyle(g, '::before');
      return {
        hovered: g.matches(':hover'),
        lineOp: b.opacity,
        lineGrad: /linear-gradient/.test(b.backgroundImage),
        // 渐变必须带「透明」端点：中间实色、两端淡出，才是一条光柱而不是一条整色的线
        lineFade: /rgba\\(0, 0, 0, 0\\)/.test(b.backgroundImage),
        w: a.width, h: a.height, radius: a.borderRadius,
        // 胶囊浮起来但不改布局：容器还是 11px（改了就成「鼠标一过页面抖一下」）
        gripH: Math.round(g.getBoundingClientRect().height),
      };
    })()`);
    check('拖手悬停：渐变线浮现（中间实、两端透明）+ 胶囊手柄，容器高度不变',
      gripHover.hovered && gripHover.lineOp === '0.9' && gripHover.lineGrad && gripHover.lineFade
        && gripHover.w === '44px' && gripHover.h === '13px' && gripHover.radius === '7px'
        && gripHover.gripH === 11,
      JSON.stringify(gripHover));

    // 自动模式下粘一大段 SQL：应该长高，但封顶在「和结果区 1:2」——
    // 上限不再按视口百分比算，而是量 .main 的实际高度再切三份。
    const grow = await cdp.eval(`(() => {
      cm.setValue(Array.from({length: 40}, (_, i) => 'SELECT ' + i + ';').join('\\n'));
      const h = document.querySelector('.CodeMirror').getBoundingClientRect().height;
      const main = document.querySelector('.main').clientHeight;
      const bar = document.querySelector('.editor-bar').offsetHeight || 40;
      return { h: Math.round(h), cap: Math.floor((main - bar) / 3) + 2 };
    })()`);
    check('长 SQL 会自动长高，但不超过「和结果区 1:2」的上限',
      grow.h > 200 && grow.h <= grow.cap, `${grow.h}px（上限 ${grow.cap}px）`);
    await cdp.eval(`cm.setValue('')`);
    await sleep(200);

    // 默认分屏：编辑器小、结果区大。1:2 是设计值，浏览器里会有几十像素的
    // 边距/边框误差，所以给一个区间而不是死磕 2.00。
    const split = await cdp.eval(`(() => {
      const e = document.querySelector('.editor-wrap').getBoundingClientRect().height;
      const r = document.getElementById('results').getBoundingClientRect().height;
      return { e: Math.round(e), r: Math.round(r), ratio: +(r / e).toFixed(2) };
    })()`);
    check('默认分屏：结果区是编辑器的 2 倍左右',
      split.ratio >= 1.7 && split.ratio <= 2.6,
      `${split.e}px : ${split.r}px（${split.ratio}×）`);

    // 0.6) 智能提示：库表列 + PG 关键字，全本地无 CDN
    const assets = await cdp.eval(`[...document.querySelectorAll('script[src],link[rel=stylesheet]')]
      .map(e => e.src || e.href)`);
    check('前端资源全部本地（无 CDN）',
      assets.length > 0 && assets.every(u => u.indexOf('127.0.0.1:8080') >= 0),
      assets.length + ' 个资源');

    // 别名点号：列名 + 类型（说明 FAR 列来自 /api/schema，不是写死的）
    await cdp.eval(`cm.setValue('SELECT e. FROM employees e'); cm.setCursor({line:0, ch:9}); cm.showHint()`);
    await sleep(500);
    let hint = await cdp.eval(`(() => {
      const ul = document.querySelector('.CodeMirror-hints');
      if (!ul) return null;
      return [...ul.querySelectorAll('li')].slice(0, 3).map(li => li.textContent.replace(/\s+/g, ' ').trim());
    })()`);
    check('别名点号只提示该表的列（带类型）',
      !!hint && hint.length === 3 && hint[0].indexOf('id') === 0 && hint[0].indexOf('integer') > 0,
      hint ? JSON.stringify(hint) : '没有弹层');

    // 选中一条并插入：用真实鼠标点第一项
    // （show-hit 的选中处理挂的是 mousedown 不是 click，JS 里 .click() 不生效）
    const item = await cdp.eval(`(() => {
      const li = document.querySelector('.CodeMirror-hints li');
      const r = li.getBoundingClientRect();
      return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: item.x, y: item.y, button: 'left', buttons: 1, clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: item.x, y: item.y, button: 'left', buttons: 0, clickCount: 1 });
    await sleep(300);
    check('点候选能把列名补进 SQL',
      await cdp.eval(`cm.getValue()`).then(v => v.indexOf('SELECT e.id FROM employees e') === 0),
      await cdp.eval(`cm.getValue()`));

    // FROM 之后：表名排在最前
    await cdp.eval(`cm.setValue('SELECT * FROM '); cm.setCursor({line:0, ch:14})`);
    await sleep(150);
    await cdp.send('Input.insertText', { text: 'emp' });
    await sleep(600);
    hint = await cdp.eval(`(() => {
      const ul = document.querySelector('.CodeMirror-hints');
      return ul ? ul.querySelector('li') && ul.querySelector('li').textContent.trim() : null;
    })()`);
    check('FROM 后按前缀提示表名', hint === 'employees表', String(hint));

    // 关键字也在词表里（PG 10）
    await cdp.eval(`cm.setValue('SEL'); cm.setCursor({line:0, ch:3}); cm.showHint()`);
    await sleep(400);
    hint = await cdp.eval(`(() => {
      const ul = document.querySelector('.CodeMirror-hints');
      return ul ? [...ul.querySelectorAll('li')].map(li => li.textContent.trim()).slice(0, 3) : null;
    })()`);
    check('PG 关键字能补全', !!hint && hint[0] === 'SELECT关键字', hint ? JSON.stringify(hint) : '没有弹层');

    // 排序权重：高频关键字永远压最上面，然后才是表、字段（真实写 SQL 的顺序）
    await cdp.eval(`cm.setValue('SELECT * FROM employees WHERE ');
      cm.setCursor({line:0, ch: cm.getLine(0).length}); cm.showHint()`);
    await sleep(500);
    const order = await cdp.eval(`(() => {
      const ul = document.querySelector('.CodeMirror-hints');
      if (!ul) return null;
      const items = [...ul.querySelectorAll('li')].map(li => {
        const l = li.querySelector('.cm-hint-label');
        return (l || li).textContent.trim();
      });
      return {
        head: items.slice(0, 5).map(s => s.toLowerCase()),
        hot: items.indexOf('SELECT'),
        table: items.indexOf('employees'),
        column: items.indexOf('id'),
        total: items.length,
      };
    })()`);
    check('无输入时高频关键字排最前',
      order && order.head.join(',') === 'select,from,where,join,update',
      order ? order.head.join(' / ') : '没有弹层');
    check('排序档位：关键字 → 表 → 字段',
      order && order.hot >= 0 && order.table > order.hot && order.column > order.table,
      order ? `SELECT@${order.hot} employees@${order.table} id@${order.column}（共 ${order.total} 条）` : '');

    // 候选框的选中行要「一眼看得出来」：底色和弹层底色拉开一档 + 主题色左条 + 命中片段加粗。
    // 之前拿 accent-soft-bg 当选中色，深色皮肤下它和弹层底色几乎一样，等于没高亮。
    const hintLook = await cdp.eval(`(() => {
      cm.setValue('SELECT emp'); cm.setCursor({line:0, ch:10}); cm.focus();
      cm.execCommand('autocomplete');
      const ul = document.querySelector('.CodeMirror-hints');
      if (!ul) return { err: '没有弹层' };
      const act = ul.querySelector('li.CodeMirror-hint-active');
      if (!act) return { err: '没有选中行' };
      const rgb = s => (s.match(/\\d+/g) || []).slice(0, 3).map(Number);
      const bg = rgb(getComputedStyle(ul).backgroundColor);
      const ab = rgb(getComputedStyle(act).backgroundColor);
      const hit = ul.querySelector('.cm-hint-hit');
      return {
        delta: Math.abs(bg[0]-ab[0]) + Math.abs(bg[1]-ab[1]) + Math.abs(bg[2]-ab[2]),
        bar: getComputedStyle(act).boxShadow,
        hit: hit ? hit.textContent : '',
        weight: hit ? getComputedStyle(hit).fontWeight : '',
        fits: ul.scrollWidth <= ul.clientWidth + 1,
      };
    })()`);
    check('候选选中行与弹层底色明显区分', hintLook && hintLook.delta >= 40,
      hintLook ? `色差 ${hintLook.delta}` : String(hintLook && hintLook.err));
    check('候选选中行有主题色左条', !!(hintLook && /inset/.test(hintLook.bar || '')),
      (hintLook && hintLook.bar) || '');
    check('已输入片段在候选中被标出（加粗）',
      !!(hintLook && /emp/i.test(hintLook.hit || '')) && Number(hintLook.weight) >= 700,
      hintLook ? `命中「${hintLook.hit}」，字重 ${hintLook.weight}` : '');
    check('候选弹层没被长签名撑破', !!(hintLook && hintLook.fits));

    // 换皮肤不能把选中色弄丢（五套都要有 --hint-active-bg，否则退回透明 = 又看不见了）
    const themeVars = await cdp.eval(`(() => {
      const out = {};
      const html = document.documentElement;
      const keep = html.getAttribute('data-theme');
      for (const t of ['dark', 'steel', 'sky', 'taro', 'mint']) {
        html.setAttribute('data-theme', t);
        out[t] = getComputedStyle(html).getPropertyValue('--hint-active-bg').trim();
      }
      html.setAttribute('data-theme', keep || 'sky');
      return out;
    })()`);
    check('五套皮肤都定义了候选选中色',
      Object.keys(themeVars).every(k => themeVars[k]), JSON.stringify(themeVars));
    await cdp.eval(`cm.setValue('')`);

    // 0.6) 帮助弹窗：原来的侧栏语法清单 + 编辑器下的示例行都搬到这里
    const helpMoved = await cdp.eval(`(() => {
      const b = document.getElementById('btnHelp');
      const l = document.querySelector('.editor-label');
      return {
        leftInSidebar: !!document.querySelector('.sidebar-foot') || !!document.querySelector('.hint-list'),
        chips: document.querySelectorAll('.examples, .chip').length,
        hasBtn: !!b,
        leftOfLabel: !!(b && l) && b.getBoundingClientRect().right <= l.getBoundingClientRect().left + 1,
      };
    })()`);
    check('侧栏不再挂语法清单、编辑器下不再有示例行',
      !helpMoved.leftInSidebar && helpMoved.chips === 0, `侧栏残留 ${helpMoved.leftInSidebar}，示例行 ${helpMoved.chips}`);
    check('帮助按钮在「SQL 编辑器」左边', helpMoved.hasBtn && helpMoved.leftOfLabel);

    await cdp.eval(`document.getElementById('btnHelp').click()`);
    await sleep(300);
    check('点问号能打开帮助弹窗',
      await cdp.eval(`!document.getElementById('helpModal').classList.contains('hidden')`));
    const helpUi = await cdp.eval(`(() => {
      const m = document.getElementById('helpModal');
      const ex = [...m.querySelectorAll('.help-ex-item')];
      const lv = [...m.querySelectorAll('.help-lv')];
      return {
        ex: ex.length,
        first: ex.length ? ex[0].querySelector('.help-ex-name').textContent.trim() : '',
        allHaveNote: ex.every(b => (b.querySelector('.help-ex-note') || {}).textContent.trim().length > 4),
        levels: lv.map(el => el.querySelector('.help-lv-title').textContent.trim()),
        // 「明确不支持」那一级没有示例代码是正常的
        noCode: lv.filter(el => !el.classList.contains('is-muted') && !el.querySelector('pre.help-code')).length,
        // 写类示例（含 INSERT/UPDATE/DELETE/ALTER/TRUNCATE/DROP）不许落在示例数据表上，
        // 否则用户点一下「执行」就把 departments/employees 改了或清空了
        writeHitsSample: lv.map(el => el.querySelector('pre.help-code')).filter(Boolean)
          .filter(co => /\b(INSERT|UPDATE|DELETE|ALTER|TRUNCATE|DROP)\b/i.test(co.textContent))
          .filter(co => /employees|departments/.test(co.textContent)).length,
        toc: m.querySelectorAll('.help-toc button').length,
        keys: m.querySelectorAll('.help-keys kbd').length,
        fits: m.querySelector('.modal-body').scrollWidth <= m.querySelector('.modal-body').clientWidth + 1,
        // 向量那一节要同时说到「支持什么」和「不支持什么」，只写一半等于没写
        vectorNote: (() => {
          const n = m.querySelector('#h10 .help-note');
          if (!n) return false;
          const t = n.textContent;
          return t.indexOf('DISTINCT') >= 0 && t.indexOf('CREATE INDEX') >= 0 && t.indexOf('PRIMARY KEY') >= 0;
        })(),
      };
    })()`);
    check('帮助里有 8 个示例、每个都带一句说明',
      helpUi.ex === 8 && helpUi.allHaveNote, `${helpUi.ex} 个，第一个是「${helpUi.first}」`);
    // 手册由浅入深：第 1 级是增删改查，最后一级必须是「明确不支持」
    check('语法手册按由浅入深分了 11 级 + 不支持',
      helpUi.levels.length === 12 &&
        helpUi.levels[0].indexOf('建表') >= 0 &&
        helpUi.levels[11] === '明确不支持',
      helpUi.levels.join(' / '));
    // 向量的限制必须写在帮助里：报错只解释了「这一句」，用户还需要知道「整类写法都不行」
    check('帮助里讲清了向量只支持距离检索、不支持等值/分组/索引',
      helpUi.levels[9].indexOf('向量') >= 0 && helpUi.vectorNote,
      `第 10 级「${helpUi.levels[9]}」· 注意事项 ${helpUi.vectorNote ? '有' : '缺失'}`);
    check('每个支持性章节都带可执行的示例代码', helpUi.noCode === 0, `${helpUi.noCode} 级没有代码`);
    check('写类示例只落在 demo 临时表上（不碰示例数据）', helpUi.writeHitsSample === 0,
      `${helpUi.writeHitsSample} 段写到示例表上`);
    check('目录条目与章节数一致', helpUi.toc === helpUi.levels.length,
      `${helpUi.toc} 个目录 / ${helpUi.levels.length} 级`);
    check('帮助里有快捷键说明', helpUi.keys >= 3, helpUi.keys + ' 个按键');
    check('帮助内容没有被挤破（不横向溢出）', helpUi.fits);

    // 点目录要能跳到对应章节（modal-body 是滚动容器，平滑滚动留点时间）
    await cdp.eval(`document.querySelector('.help-toc [data-goto="h9"]').click()`);
    await sleep(900);
    const helpScroll = await cdp.eval(`document.querySelector('#helpModal .modal-body').scrollTop`);
    check('点目录能跳到对应章节', helpScroll > 200, 'scrollTop=' + Math.round(helpScroll));

    // 页脚要说明「示例数据在不在」：不在就得给出一键补齐的按钮，
    // 否则别人打开空库时会看到一堆 relation does not exist 却不知道先做什么
    const helpData = await cdp.eval(`(() => {
      const h = document.getElementById('helpDataHint');
      const b = document.getElementById('btnHelpSample');
      const read = () => ({ text: h.textContent.trim(), hidden: b.classList.contains('hidden') });
      const now = read();
      schemaCache.length = 0;          // 模拟空库：当前库没有任何表
      updateHelpDataHint();
      const empty = read();
      return { now, empty };
    })()`);
    check('帮助页脚：示例数据就绪时说明状态、不留载入按钮',
      helpData.now.text.indexOf('已就绪') >= 0 && helpData.now.hidden === true,
      helpData.now.text);
    check('帮助页脚：缺示例表时给出提示 + 载入按钮',
      helpData.empty.text.indexOf('没有示例表') >= 0 && helpData.empty.hidden === false,
      helpData.empty.text);
    // 把 schema 恢复回去，后面的补全断言还要用
    await cdp.eval(`loadSchema().then(() => updateHelpDataHint())`);
    await sleep(400);

    // 深链 #help 一进页面就打开帮助：此时 schema 可能还没加载完，
    // 页脚不能因此误报「没有示例表」（实测踩过：两张截图字节完全一样）
    await cdp.send('Page.navigate', { url: 'http://127.0.0.1:8080/?theme=sky#help' });
    await waitFor(cdp, `(() => { const m = document.getElementById('helpModal');
      return !!m && !m.classList.contains('hidden'); })()`, { label: '#help 深链打开帮助' });
    const deepLink = await cdp.eval(`(() => ({
      text: document.getElementById('helpDataHint').textContent.trim(),
      hidden: document.getElementById('btnHelpSample').classList.contains('hidden'),
    }))()`);
    check('深链 #help 打开时页脚不会误报缺示例表',
      deepLink.text.indexOf('已就绪') >= 0 && deepLink.hidden === true,
      deepLink.text);

    // 点示例只填进编辑器、不自动执行
    await cdp.eval(`[...document.querySelectorAll('.help-ex-item')].find(b => b.dataset.ex === 'join').click()`);
    await sleep(400);
    check('点示例填进编辑器并关掉帮助',
      await cdp.eval(`cm.getValue().indexOf('LEFT JOIN departments d') > 0 &&
        document.getElementById('helpModal').classList.contains('hidden')`));

    // Esc 能关
    await cdp.eval(`document.getElementById('btnHelp').click()`);
    await sleep(250);
    await cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await sleep(200);
    check('帮助弹窗 Esc 能关',
      await cdp.eval(`document.getElementById('helpModal').classList.contains('hidden')`));
    await cdp.eval(`cm.setValue('')`);

    // 1) 数据库面板：点遮罩不关闭，点 × 才关闭
    await cdp.eval(`openDBModal()`);
    await sleep(400);
    check('库管理：打开后可见', await cdp.eval(`!document.getElementById('dbModal').classList.contains('hidden')`));

    await cdp.eval(`document.getElementById('dbModal').dispatchEvent(new MouseEvent('click',{bubbles:true}))`);
    await sleep(200);
    check('库管理：点遮罩后仍然打开', await cdp.eval(`!document.getElementById('dbModal').classList.contains('hidden')`));

    // 真实鼠标点在左上角空白处（遮罩区域，远离卡片）
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: 8, y: 8, button: 'left', clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: 8, y: 8, button: 'left', clickCount: 1 });
    await sleep(200);
    check('库管理：真实点击遮罩空白处仍打开', await cdp.eval(`!document.getElementById('dbModal').classList.contains('hidden')`));

    await cdp.eval(`document.getElementById('btnDBDone').click()`);
    await sleep(200);
    check('库管理：点「关闭」能关掉', await cdp.eval(`document.getElementById('dbModal').classList.contains('hidden')`));

    // 1.5) 拖动 / 缩放 / 双击复位
    await cdp.eval(`openDBModal()`);
    await sleep(400);
    const geo = await cdp.eval(`(() => {
      const card = document.querySelector('#dbModal .modal-card');
      const head = card.querySelector('.modal-head').getBoundingClientRect();
      const r = card.getBoundingClientRect();
      return { x: r.left, y: r.top, w: r.width, h: r.height,
               hx: head.left + 40, hy: head.top + head.height / 2 };
    })()`);

    // 抓标题栏往右下拖 (100, 50)
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: geo.hx, y: geo.hy, button: 'left', buttons: 1, clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: geo.hx + 50, y: geo.hy + 25, button: 'left', buttons: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: geo.hx + 100, y: geo.hy + 50, button: 'left', buttons: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: geo.hx + 100, y: geo.hy + 50, button: 'left', buttons: 0, clickCount: 1 });
    await sleep(250);
    const moved = await cdp.eval(`(() => {
      const r = document.querySelector('#dbModal .modal-card').getBoundingClientRect();
      return { x: r.left, y: r.top };
    })()`);
    check('拖动标题栏能移动窗口',
      Math.abs(moved.x - (geo.x + 100)) < 3 && Math.abs(moved.y - (geo.y + 50)) < 3,
      `(${Math.round(geo.x)},${Math.round(geo.y)}) → (${Math.round(moved.x)},${Math.round(moved.y)})`);

    // 拽右下角手柄放大 (120, 60)
    const gripPos = await cdp.eval(`(() => {
      const g = document.querySelector('#dbModal .win-grip').getBoundingClientRect();
      return { x: g.left + g.width / 2, y: g.top + g.height / 2 };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: gripPos.x, y: gripPos.y, button: 'left', buttons: 1, clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: gripPos.x + 60, y: gripPos.y + 30, button: 'left', buttons: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: gripPos.x + 120, y: gripPos.y + 60, button: 'left', buttons: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: gripPos.x + 120, y: gripPos.y + 60, button: 'left', buttons: 0, clickCount: 1 });
    await sleep(250);
    const sized = await cdp.eval(`(() => {
      const r = document.querySelector('#dbModal .modal-card').getBoundingClientRect();
      return { w: r.width, h: r.height, x: r.left, y: r.top };
    })()`);
    // 期望尺寸按「指针落点 - 左上角」算：手柄中心比卡片边缘内缩 8px，
    // 直接用 w+120 会差那 8px（不是实现问题，是断言算法问题）
    const expW = gripPos.x + 120 - sized.x;
    const expH = gripPos.y + 60 - sized.y;
    check('拽右下角能改大小（尺寸跟手）',
      Math.abs(sized.w - expW) < 3 && Math.abs(sized.h - expH) < 3,
      `${Math.round(geo.w)}x${Math.round(geo.h)} → ${Math.round(sized.w)}x${Math.round(sized.h)}` +
      `（指针期望 ${Math.round(expW)}x${Math.round(expH)}）`);

    // 拖动之后 × 仍然能关（拖拽不能把点击吃掉）
    await cdp.eval(`document.getElementById('btnDBDone').click()`);
    await sleep(200);
    check('拖动/缩放后仍能正常关闭',
      await cdp.eval(`document.getElementById('dbModal').classList.contains('hidden')`));

    // 双击标题栏复位
    await cdp.eval(`openDBModal()`);
    await sleep(400);
    const before = await cdp.eval(`(() => {
      const card = document.querySelector('#dbModal .modal-card');
      const head = card.querySelector('.modal-head').getBoundingClientRect();
      return { x: card.getBoundingClientRect().left, hy: head.top + head.height / 2, hx: head.left + 40 };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: before.hx, y: before.hy, button: 'left', buttons: 1, clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: before.hx + 120, y: before.hy + 80, button: 'left', buttons: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: before.hx + 120, y: before.hy + 80, button: 'left', buttons: 0, clickCount: 1 });
    await sleep(200);
    // 双击标题栏（两次 clickCount=1 会被识别成 dblclick 需要 clickCount=2）
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: before.hx + 120, y: before.hy + 80, button: 'left', buttons: 1, clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: before.hx + 120, y: before.hy + 80, button: 'left', buttons: 0, clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: before.hx + 120, y: before.hy + 80, button: 'left', buttons: 1, clickCount: 2 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: before.hx + 120, y: before.hy + 80, button: 'left', buttons: 0, clickCount: 2 });
    await sleep(250);
    check('双击标题栏回到居中',
      await cdp.eval(`(() => {
        const c = document.querySelector('#dbModal .modal-card');
        return c.dataset.framed === '0' && !c.style.left && !c.style.width;
      })()`));
    await cdp.eval(`document.getElementById('btnDBDone').click()`);
    await sleep(200);

    // 2) 设置面板
    await cdp.eval(`openSettings()`);
    await sleep(900);
    check('设置：打开后可见', await cdp.eval(`!document.getElementById('settingsPanel').classList.contains('hidden')`));

    // 顶栏只留高频入口，皮肤/字号/示例数据/清库都进设置页
    const setUi = await cdp.eval(`(() => {
      const bar = document.querySelector('.top-actions');
      const top = document.querySelector('.topbar');
      const body = document.getElementById('settingsBody');
      const picks = [...body.querySelectorAll('.theme-pick')];
      const active = body.querySelector('.theme-pick[aria-pressed="true"]');
      const btns = [...body.querySelectorAll('button')].map(b => b.textContent.trim());
      return {
        // 库选择在品牌右边（左侧），不在 .top-actions 里了 → 断言看整个顶栏
        barText: top.textContent.replace(/\\s+/g, ' ').trim(),
        // 库选择必须落在右半区（动作区）之前：切换库是上下文，不是操作
        dbPickLeftOfActions: (() => {
          const db = top.querySelector('.db-pick');
          if (!db || !bar) return false;
          return db.compareDocumentPosition(bar) === Node.DOCUMENT_POSITION_FOLLOWING;
        })(),
        barHasTheme: !!bar.querySelector('.theme-pick, .theme-dot, .fs-ctl'),
        barHasData: !!document.getElementById('btnSample') || !!document.getElementById('btnReset'),
        picks: picks.length,
        pickNames: picks.map(p => p.textContent.trim()),
        activeTheme: active ? active.dataset.theme : '',
        fsValues: body.querySelectorAll('.fs-value').length,
        fsBtns: body.querySelectorAll('.fs-btn').length,
        fsText: (body.querySelector('.fs-value') || {}).textContent || '',
        hasSample: btns.indexOf('载入示例数据') >= 0,
        hasReset: btns.indexOf('清空当前库') >= 0,
        secs: [...body.querySelectorAll('.set-sec')].map(s => s.textContent.trim()),
        // 空闲药丸已撤：空闲是常态，靠圆的绿点 + hover 文字表达
        idlePillHidden: (() => {
          const p = document.getElementById('txPill');
          return !!p && (p.classList.contains('hidden') || getComputedStyle(p).display === 'none');
        })(),
        dotSize: (() => {
          const d = document.getElementById('monDot');
          if (!d) return 0;
          return Math.round(d.getBoundingClientRect().width);
        })(),
        dotTip: (() => {
          const d = document.getElementById('monDot');
          return d ? (d.title || '') : '';
        })(),
      };
    })()`);
    check('顶栏清瘦：不再挂皮肤/字号/示例数据/清库',
      !setUi.barHasTheme && !setUi.barHasData, setUi.barText);
    // 0.2.0 起「锁状态」换成「监控」（面板里带锁/慢 SQL/超时开关）
    check('顶栏仍保留 库/监控/设置',
      ['库', '监控', '设置'].every(k => setUi.barText.indexOf(k) >= 0), setUi.barText);
    check('库选择在顶栏左侧（品牌右边，动作区之前）', setUi.dbPickLeftOfActions,
      setUi.dbPickLeftOfActions ? '在左' : '仍在右侧动作区');
    check('空闲状态不占顶栏（药丸隐藏，靠圆点表达）', setUi.idlePillHidden,
      setUi.idlePillHidden ? '药丸已隐藏' : '药丸仍显示');
    check('监控圆点够大且有 hover 文字', setUi.dotSize >= 10 && setUi.dotTip.length > 0,
      setUi.dotSize + 'px / ' + setUi.dotTip);

    // 字标：GoDB，「DB」用主色 —— 顶栏里唯一一处有颜色的文字，不许再叠渐变/阴影
    const brand = await cdp.eval(`(() => {
      const w = document.querySelector('.wordmark');
      const span = w && w.querySelector('span');
      const probe = document.createElement('span');
      probe.style.color = 'var(--accent)';
      document.body.appendChild(probe);
      const accent = getComputedStyle(probe).color;
      probe.remove();
      return {
        text: w ? w.textContent.trim() : '',
        dbColor: span ? getComputedStyle(span).color : '',
        accent: accent,
        shadow: w ? getComputedStyle(w).textShadow : '',
        bgImage: w ? getComputedStyle(w).backgroundImage : '',
        title: document.title,
      };
    })()`);
    check('品牌字标 GoDB：DB 用主色、无渐变/文字阴影',
      brand.text === 'GoDB' && brand.dbColor === brand.accent &&
        brand.shadow === 'none' && brand.bgImage === 'none' && /^GoDB/.test(brand.title),
      `${brand.text} · DB 色 ${brand.dbColor} · title ${brand.title}`);

    /* 品牌区（logo + 字标）也是「回首页」的入口 —— 用户的直觉就是点左上角那个名字。
       它是个 role=button 的 div（里装着 h1，换成 <button> 是非法嵌套），所以
       光标、role、以及 Enter 键都得自己写，也就都得测。

       注意：这条会**关掉设置面板**（回首页的语义就是连设置一起收），而后面几条
       要量设置页的尺寸。所以进来先把设置收干净、跑完再按原样打开，否则后面那些
       getBoundingClientRect 全是 0 —— 这不是回归，是测试自己把状态改了。 */
    const brandHome = await cdp.eval(`(async () => {
      const panel = document.getElementById('monPanel');
      const setPanel = document.getElementById('settingsPanel');
      const wait = () => new Promise(r => setTimeout(r, 350));
      const setWasOpen = !setPanel.classList.contains('hidden');
      if (setWasOpen) { closeSettings(true); await wait(); }

      document.getElementById('btnMonitor').click();
      await wait();
      const opened = !panel.classList.contains('hidden');
      document.getElementById('btnBrand').click();
      await wait();
      const afterClick = { hidden: panel.classList.contains('hidden'), mon: monOpen };

      document.getElementById('btnMonitor').click();
      await wait();
      const b = document.getElementById('btnBrand');
      b.focus();
      b.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      await wait();
      const afterKey = { hidden: panel.classList.contains('hidden'), mon: monOpen };

      const out = {
        opened, afterClick, afterKey, setWasOpen,
        role: b.getAttribute('role'),
        cursor: getComputedStyle(b).cursor,
      };
      if (setWasOpen) { document.getElementById('btnSettings').click(); await wait(); await wait(); }
      return out;
    })()`);
    check('点左上角 logo / GoDB 字标回首页（鼠标和 Enter 都行）',
      brandHome.opened && brandHome.afterClick.hidden && brandHome.afterClick.mon === false &&
        brandHome.afterKey.hidden && brandHome.afterKey.mon === false &&
        brandHome.role === 'button' && brandHome.cursor === 'pointer',
      `点击 ${brandHome.afterClick.mon === false} · Enter ${brandHome.afterKey.mon === false} · ${brandHome.role}/${brandHome.cursor}`);
    check('设置页有 5 个带名字的皮肤卡片',
      setUi.picks === 5 && setUi.pickNames.join('/') === '科技灰/深色/淡蓝/芋紫/薄荷绿',
      setUi.pickNames.join('/') + '，选中 ' + setUi.activeTheme);

    // 科技灰：深色系但底色偏冷、主色是浅天蓝；顺手确认它真的生效（不是只加了个名字）
    const steel = await cdp.eval(`(() => {
      applyTheme('steel', false);
      const cs = getComputedStyle(document.documentElement);
      const get = n => cs.getPropertyValue(n).trim();
      return {
        theme: document.documentElement.getAttribute('data-theme'),
        bg: get('--bg'), accent: get('--accent'), text: get('--text'),
        bodyBg: getComputedStyle(document.body).backgroundColor,
      };
    })()`);
    // 底色要「冷灰」而不是纯黑：三通道都很低、蓝分量不低于红分量（冷调）
    const bodyRGB = (steel.bodyBg.match(/\d+/g) || []).map(Number);
    check('科技灰皮肤：冷调深灰（不是纯黑）+ 浅天蓝主色，且能真正切换',
      steel.theme === 'steel' && steel.accent === '#89b4fa' && bodyRGB.length === 3 &&
        bodyRGB.every(v => v >= 15 && v <= 45) && bodyRGB[2] >= bodyRGB[0],
      `${steel.theme} / accent ${steel.accent} / body ${steel.bodyBg}`);
    await cdp.eval(`applyTheme('sky', false)`);
    check('设置页有字号控件', setUi.fsBtns === 2 && setUi.fsValues === 1 && /%$/.test(setUi.fsText), setUi.fsText);
    check('设置页有载入示例数据 / 清空当前库',
      setUi.hasSample && setUi.hasReset);
    // 五组 Tab：连接 / 安全 / 性能 / 外观 / 数据。旧版是一条长列滚两屏，找一项得翻半天。
    const setTabs = await cdp.eval(`(() => {
      const tabs = [...document.querySelectorAll('.set-tab')];
      return {
        names: tabs.map(t => t.textContent.trim()),
        active: (tabs.find(t => t.classList.contains('on')) || {}).textContent || '',
        panes: [...document.querySelectorAll('.set-pane')].filter(p => !p.classList.contains('hidden')).map(p => p.dataset.pane),
      };
    })()`);
    check('设置页分成五组：连接/安全/性能/外观/数据',
      setTabs.names.join('/') === '连接/安全/性能/外观/数据', setTabs.names.join('/'));
    check('设置页默认停在「连接」，一屏只显示一组',
      setTabs.active === '连接' && setTabs.panes.join() === 'conn', setTabs.active + ' / ' + setTabs.panes.join());

    /* 远程控制台必须先设密码：后端在 config.Validate 里硬拦，前端在安全页先提示。
       两处一起验 —— 只验前端的话，绕过页面直接 POST 照样能开门。 */
    const secUi = await cdp.eval(`(() => {
      [...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'sec').click();
      const txt = document.querySelector('.set-pane[data-pane="sec"]').textContent;
      [...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'conn').click();
      return { login: /远程访问时必须登录/.test(txt), pw: /密码/.test(txt) };
    })()`);
    check('安全页写明「远程访问必须登录、本机免登录」', secUi.login && secUi.pw,
      `提到登录 ${secUi.login}`);

    const secGuard = await cdp.eval(`(async () => {
      const cur = await fetch('/api/config').then(r => r.json());
      const body = {
        storage: cur.storage, performance: cur.performance, timeouts: cur.timeouts,
        network: Object.assign({}, cur.network, { httpRemote: true }),
        auth: { requirePassword: false, user: cur.auth.user },
      };
      const r = await fetch('/api/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      });
      const d = await r.json();
      return { code: r.status, error: d.error || '' };
    })()`);
    check('后端拒绝「没密码就开远程」（错误里点名要密码）',
      secGuard.code === 400 && /密码/.test(secGuard.error), `${secGuard.code} ${secGuard.error}`);

    /* 备份导出：接口要能直接下载一份 .sql，内容含建表/数据/序列对齐；
       设置页「数据」组里要能看到两个按钮。恢复链路在 db 的 Go 测试里跑过
       （导出→灌回空库→逐行比对），这里只验浏览器这一侧。 */
    const bkApi = await cdp.eval(`(async () => {
      const r = await fetch('/api/backup?name=godb');
      const text = await r.text();
      return {
        code: r.status,
        cd: r.headers.get('content-disposition') || '',
        create: (text.match(/CREATE TABLE/g) || []).length,
        ins: text.includes('INSERT INTO'),
        setval: text.includes('setval('),
        head: text.slice(0, 24),
      };
    })()`);
    check('备份接口直接给一份可执行的 .sql（建表 / 数据 / 序列对齐）',
      bkApi.code === 200 && /attachment; filename="godb-.*\.sql"/.test(bkApi.cd)
      && bkApi.create >= 1 && bkApi.ins && bkApi.setval && /godb 备份/.test(bkApi.head),
      `${bkApi.code} · ${bkApi.cd} · 建表 ${bkApi.create} 条`);

    const bkUi = await cdp.eval(`(() => {
      [...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'data').click();
      const pane = document.querySelector('.set-pane[data-pane="data"]');
      const btns = [...pane.querySelectorAll('button')].map(b => b.textContent.trim());
      const hasFile = !!pane.querySelector('input[type=file]');
      [...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'conn').click();
      return { btns, hasFile };
    })()`);
    check('设置→数据里有「下载备份」「从备份恢复」，恢复是选文件不是粘贴框',
      bkUi.btns.some(t => /下载备份/.test(t)) && bkUi.btns.some(t => /从备份恢复/.test(t))
      && bkUi.hasFile,
      bkUi.btns.join(' / '));

    // 切组不重新渲染：在「连接」里改了一半再去看「性能」，回来还得在
    const keep = await cdp.eval(`(() => {
      document.getElementById('setAuthUser').value = 'keepme';
      document.getElementById('setAuthUser').dispatchEvent(new Event('input', { bubbles: true }));
      [...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'perf').click();
      return {
        user: document.getElementById('setAuthUser').value,
        dirty: document.getElementById('settingsDirty').textContent.trim(),
        active: (document.querySelector('.set-tab.on') || {}).textContent || '',
      };
    })()`);
    check('切组后另一组的改动还在，并提示「1 处未保存」',
      keep.user === 'keepme' && keep.active === '性能' && /1 处未保存/.test(keep.dirty),
      `${keep.active} / ${keep.dirty}`);

    // 端口填 70000：立刻标红 + 底部禁用保存，不等到提交才报错
    const badPort = await cdp.eval(`(() => {
      [...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'conn').click();
      const n = document.getElementById('setHTTPPort');
      n.value = '70000';
      n.dispatchEvent(new Event('input', { bubbles: true }));
      const r = {
        bad: n.classList.contains('bad'),
        hint: document.getElementById('settingsDirty').textContent.trim(),
        disabled: document.getElementById('btnSettingsSave').disabled,
      };
      n.value = n.dataset.init;
      n.dispatchEvent(new Event('input', { bubbles: true }));
      return r;
    })()`);
    check('端口超范围：输入框标红 + 保存被禁用',
      badPort.bad && /输入不合法/.test(badPort.hint) && badPort.disabled,
      `${badPort.hint} / 保存禁用 ${badPort.disabled}`);

    // 外观组：本地偏好，不进保存。有别的改动时按钮还是「保存并应用」，别让人以为没处可存；
    // 一条改动都没有时才是「关闭」。
    const lookWithDirty = await cdp.eval(`(() => {
      [...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'look').click();
      const b = document.getElementById('btnSettingsSave');
      return { btn: b.textContent.trim(), mode: b.dataset.mode, dirty: document.getElementById('settingsDirty').textContent.trim() };
    })()`);
    check('外观组：还压着别处改动时，按钮仍是「保存并应用」',
      lookWithDirty.mode === 'save' && /未保存/.test(lookWithDirty.dirty),
      `${lookWithDirty.btn} / ${lookWithDirty.dirty}`);

    // 把连接的改动还原，再回外观组：这时才是「关闭」
    await cdp.eval(`(() => {
      const n = document.getElementById('setAuthUser');
      n.value = n.dataset.init;
      n.dispatchEvent(new Event('input', { bubbles: true }));
    })()`);
    const look = await cdp.eval(`(() => {
      const pane = document.querySelector('.set-pane[data-pane="look"]');
      const b = document.getElementById('btnSettingsSave');
      return {
        btn: b.textContent.trim(),
        mode: b.dataset.mode,
        picks: pane.querySelectorAll('.theme-pick').length,
        fs: pane.querySelectorAll('.fs-btn').length,
      };
    })()`);
    check('外观组：皮肤/字号都在这一组，无改动时按钮变成「关闭」',
      look.mode === 'close' && look.btn === '关闭' && look.picks === 5 && look.fs === 2,
      `${look.btn} / ${look.picks} 皮肤 / ${look.fs} 字号按钮`);

    // 数据组：危险操作独处红框，不和普通配置混排
    const dataPane = await cdp.eval(`(() => {
      [...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'data').click();
      const zone = document.querySelector('.set-pane[data-pane="data"] .set-danger');
      return {
        hasZone: !!zone,
        borderStyle: zone ? getComputedStyle(zone).borderStyle : '',
        stats: document.querySelectorAll('.set-pane[data-pane="data"] .set-stats span').length,
        btns: zone ? zone.querySelectorAll('button').length : 0,
      };
    })()`);
    check('数据组：清库/示例在虚线危险框里，现状统计也在这组',
      dataPane.hasZone && dataPane.borderStyle === 'dashed' && dataPane.btns === 2 && dataPane.stats === 9,
      `危险框 ${dataPane.borderStyle} / ${dataPane.btns} 按钮 / ${dataPane.stats} 项统计`);
    await cdp.eval(`[...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'look').click()`);

    // 设置是「页面」不是「弹窗」：高度天然恒定，切 Tab 不再需要钉死高度；
    // 内容超出时由正文自己滚（底栏始终可见）。
    const setH = await cdp.eval(`(() => {
      const panel = document.getElementById('settingsPanel');
      const h = [];
      for (const t of document.querySelectorAll('.set-tab')) {
        t.click();
        h.push(Math.round(panel.getBoundingClientRect().height));
      }
      const body = document.querySelector('#settingsPanel .set-body');
      const foot = document.querySelector('#settingsPanel .set-foot');
      const pr = panel.getBoundingClientRect();
      const fr = foot.getBoundingClientRect();
      return { h, scroll: getComputedStyle(body).overflowY,
               top: Math.round(pr.top), width: Math.round(pr.width),
               footAtBottom: Math.abs(fr.bottom - pr.bottom) < 2 };
    })()`);
    check('设置是页面级视图：切五个 Tab 高度不动，正文自己滚，底栏贴底',
      new Set(setH.h).size === 1 && setH.h[0] > 400 && setH.scroll === 'auto' && setH.footAtBottom,
      setH.h.join(' / ') + ' · overflow ' + setH.scroll + ' · 底栏贴底 ' + setH.footAtBottom);
    check('设置页不遮挡顶栏（不是浮层遮罩）',
      setH.top >= 40 && setH.width >= 1000, `top ${setH.top}px · 宽 ${setH.width}px`);
    await cdp.eval(`[...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'look').click()`);

    // 点了要立刻生效（不依赖「保存并应用」）
    await cdp.eval(`document.querySelector('.theme-pick[data-theme="taro"]').click()`);
    await sleep(250);
    check('设置页点皮肤立即生效并记住',
      await cdp.eval(`document.documentElement.getAttribute('data-theme') === 'taro' &&
        localStorage.getItem('godb.theme') === 'taro'`),
      await cdp.eval(`document.documentElement.getAttribute('data-theme')`));
    const fs0 = await cdp.eval(`getComputedStyle(document.documentElement).getPropertyValue('--fs').trim()`);
    await cdp.eval(`[...document.querySelectorAll('.fs-btn')].find(b => b.dataset.fsStep === '1').click()`);
    await sleep(200);
    const fs1 = await cdp.eval(`getComputedStyle(document.documentElement).getPropertyValue('--fs').trim()`);
    check('设置页点 A+ 字号立即变大', parseFloat(fs1) > parseFloat(fs0), `${fs0} → ${fs1}`);
    // 回到原皮肤，别影响后面的断言
    await cdp.eval(`document.querySelector('.theme-pick[data-theme="sky"]').click()`);
    await sleep(150);

    // 顶栏三颗是同级页面：同一时刻只亮一颗。设置页开着时点「监控」应该切过去。
    const navOn = await cdp.eval(`({
      set: document.getElementById('btnSettings').classList.contains('on'),
      home: document.getElementById('btnHome').classList.contains('on'),
      mon: document.getElementById('btnMonitor').classList.contains('on'),
    })`);
    check('设置页打开时顶栏只有「设置」亮着',
      navOn.set && !navOn.home && !navOn.mon, JSON.stringify(navOn));
    await cdp.eval(`document.getElementById('btnMonitor').click()`);
    await sleep(400);
    const swap1 = await cdp.eval(`({
      setHidden: document.getElementById('settingsPanel').classList.contains('hidden'),
      monShown: !document.getElementById('monPanel').classList.contains('hidden'),
    })`);
    check('点「监控」会收起设置（同级页面互斥）',
      swap1.setHidden && swap1.monShown, JSON.stringify(swap1));
    await cdp.eval(`document.getElementById('btnSettings').click()`);
    await sleep(500);
    const swap2 = await cdp.eval(`({
      setShown: !document.getElementById('settingsPanel').classList.contains('hidden'),
      monHidden: document.getElementById('monPanel').classList.contains('hidden'),
    })`);
    check('点「设置」会收起监控',
      swap2.setShown && swap2.monHidden, JSON.stringify(swap2));
    await cdp.eval(`document.getElementById('btnSettingsClose').click()`);
    await sleep(200);
    check('设置：点 × 能关掉', await cdp.eval(`document.getElementById('settingsPanel').classList.contains('hidden')`));

    // 基准：从独立全屏页收进监控第 5 页，顶栏那颗按钮去掉了。
    // 轻量冒烟 —— 只验证入口位置、切页、接口返回空（不真跑 100 万行，
    // 那是 web/bench_test.go 的 Go 单测用 2000 行小规模在跑的）。
    const benchEntry = await cdp.eval(`({
      topBtn: !!document.getElementById('btnBench'),
      tab: !!document.querySelector('#monNav .mon-nav-tab[data-page="bench"]'),
      badgeHidden: document.getElementById('benchBadge').classList.contains('hidden'),
    })`);
    check('基准入口挪进监控 Tab：顶栏不再有独立按钮，监控按钮挂进度 badge（无任务时隐藏）',
      !benchEntry.topBtn && benchEntry.tab && benchEntry.badgeHidden, JSON.stringify(benchEntry));

    // 前面几条用例已经把监控开关过几轮，先确保它是开着的再切 Tab
    await cdp.eval(`(() => { if (document.getElementById('monPanel').classList.contains('hidden'))
      document.getElementById('btnMonitor').click(); })()`);
    await sleep(300);
    await cdp.eval(`document.querySelector('#monNav .mon-nav-tab[data-page="bench"]').click()`);
    await sleep(400);
    const benchUi = await cdp.eval(`({
      shown: !document.querySelector('#monPanel .mon-page[data-page="bench"]').classList.contains('hidden'),
      hasStart: !!document.getElementById('btnBenchStart'),
      scale: (document.getElementById('benchScale') || {}).textContent,
      navOn: document.querySelector('#monNav .mon-nav-tab[data-page="bench"]').classList.contains('on'),
      overviewHidden: document.querySelector('#monPanel .mon-page[data-page="overview"]').classList.contains('hidden'),
      pauseHidden: document.getElementById('btnMonPause').classList.contains('hidden'),
      refreshHidden: document.getElementById('btnMonRefresh').classList.contains('hidden'),
    })`);
    check('基准页能打开：开始按钮、规模 1,000,000、Tab 高亮、其它页收起',
      benchUi.shown && benchUi.hasStart && /1,?000,?000/.test(benchUi.scale) && benchUi.navOn
        && benchUi.overviewHidden, JSON.stringify(benchUi));
    check('基准页收起「暂停刷新 / 立即刷新」（它不跟每秒水位轮询）',
      benchUi.pauseHidden && benchUi.refreshHidden, JSON.stringify(benchUi));

    // 接口无任务时返回空 result（冒烟，不发 start 以免真灌 100 万行）
    const benchStatus = await cdp.eval(`fetch('/api/bench/status').then(r => r.json())`);
    // 没在跑：result 要么是 null（本次进程还没跑过），要么是上一轮跑完的结果。
    // 别写死 null —— 跑过一次基准之后再回归，这条就会红，而那是正常状态。
    check('基准接口：没在跑时 running=false，result 为 null 或已结束的上一轮',
      benchStatus && benchStatus.running === false
        && (benchStatus.result === null || benchStatus.result.finished === true),
      JSON.stringify(benchStatus).slice(0, 80));

    // 切回概览：头部控件必须还回来，否则整个监控页都刷不了
    await cdp.eval(`document.querySelector('#monNav .mon-nav-tab[data-page="overview"]').click()`);
    await sleep(300);
    check('离开基准页后「暂停刷新 / 立即刷新」恢复',
      await cdp.eval(`!document.getElementById('btnMonPause').classList.contains('hidden') &&
        !document.getElementById('btnMonRefresh').classList.contains('hidden')`));

    // 顶栏进度 badge：不真灌 100 万行，把「正在跑」的状态直接灌进去看反应。
    // 这几条守的是「异步任务跨页可见」—— 它原来是独立全屏页，点了开始切回去干活就
    // 彻底看不见了，只能凭感觉等。
    const badgeOn = await cdp.eval(`(() => {
      benchRunning = true;
      syncBenchBadge({ running: true, percent: 37.4, step: '批量插入', detail: 'batch=1000 · 第 2/3 轮' });
      const b = document.getElementById('benchBadge');
      return { hidden: b.classList.contains('hidden'), text: b.textContent.trim(), tip: b.title };
    })()`);
    check('基准跑起来时顶栏「监控」挂百分比 badge（带当前步骤与细分）',
      !badgeOn.hidden && badgeOn.text === '37%' && /批量插入/.test(badgeOn.tip)
        && /batch=1000/.test(badgeOn.tip),
      JSON.stringify(badgeOn));

    await cdp.eval(`document.getElementById('btnMonitor').click()`);
    await sleep(300);
    check('监控面板关掉后 badge 仍在顶栏（任务跑着切走也看得见）',
      await cdp.eval(`!document.getElementById('benchBadge').classList.contains('hidden')`));

    await cdp.eval(`document.getElementById('btnMonitor').click()`);
    await sleep(400);
    check('badge 亮着时点「监控」，直接落到基准页',
      (await cdp.eval(`monPageTab`)) === 'bench');

    const badgeOff = await cdp.eval(`(() => {
      benchRunning = false;
      syncBenchBadge({ running: false });
      return document.getElementById('benchBadge').classList.contains('hidden');
    })()`);
    check('任务结束后 badge 收起，不留 100% 挂在顶栏', badgeOff === true);

    // 进度块：一次基准要好几分钟，而「批量删除」一个场景就要重灌 5 次一百万行的表。
    // 只显示「正在跑 批量删除」的话，「这一秒没变化」和「卡死了」看起来一模一样。
    const runUi = await cdp.eval(`(() => {
      renderBenchRun({ running: true, step: '批量删除',
        detail: '第 2/3 轮 · 重灌 · 灌数据 500,000 / 1,000,000 行',
        percent: 57.3, elapsedMs: 83000, etaMs: 45000 });
      const q = id => document.getElementById(id).textContent.trim();
      return {
        shown: !document.getElementById('benchRun').classList.contains('hidden'),
        step: q('benchStep'), detail: q('benchDetail'), pct: q('benchPct'),
        barW: document.getElementById('benchBar').style.width,
        elapsed: q('benchElapsed'), eta: q('benchEta'),
        // 刷新页面时是「先看到任务在跑」，startBench 没执行过 —— 按钮状态得由这里管，
        // 否则开始按钮还能点、停止不出现，跑着的任务停不下来
        startDisabled: document.getElementById('btnBenchStart').disabled,
        cancelShown: !document.getElementById('btnBenchCancel').classList.contains('hidden'),
      };
    })()`);
    check('基准进度块：步骤 + 场景内细分 + 百分比 + 进度条 + 已用/预计剩余',
      runUi.shown && runUi.step === '批量删除' && /灌数据 500,000/.test(runUi.detail)
        && runUi.pct === '57%' && parseFloat(runUi.barW) > 50
        && runUi.elapsed === '已用 1:23' && runUi.eta === '预计还需 45 秒',
      JSON.stringify(runUi));
    check('跑着的时候「开始」禁用、「停止」可见（刷新后也能停）',
      runUi.startDisabled && runUi.cancelShown,
      `开始 disabled=${runUi.startDisabled} / 停止 显示=${runUi.cancelShown}`);

    const runOff = await cdp.eval(`(() => {
      renderBenchRun({ running: false });
      const hidden = document.getElementById('benchRun').classList.contains('hidden');
      // 收尾：把按钮还原，别让后面用例看见一个禁用的「开始基准」
      document.getElementById('btnBenchStart').disabled = false;
      document.getElementById('btnBenchCancel').classList.add('hidden');
      return { hidden };
    })()`);
    check('任务结束后进度块收起', runOff.hidden === true, JSON.stringify(runOff));

    // 结果表：新加的「逐条提交插入」要能在表里原样出现，并把 fsync / 提交次数带出来。
    // 表格是数据驱动的，但「后端加了场景、前端没显示」这种断链只有真渲染一次才看得见。
    const tableUi = await cdp.eval(`(() => {
      renderBenchResult({ startedAt: '2026-11-03 22:32:30', version: '0.4.0', scenarios: [
        { name: '单条插入', commit: '单事务逐条', rows: 20000, perSec: 1490.07 },
        { name: '逐条提交插入', commit: '每行一事务 · 15,078 fsync / 20,001 提交', rows: 5000, perSec: 1243.77 },
      ] });
      const trs = [...document.querySelectorAll('#benchRows tr')];
      const txt = i => trs[i] ? trs[i].textContent.replace(/\\s+/g, ' ').trim() : '';
      const out = { n: trs.length, last: txt(1) };
      renderBenchResult({ scenarios: [] });
      return out;
    })()`);
    check('结果表渲染「逐条提交插入」，并带出 fsync / 提交次数',
      tableUi.n === 2 && /逐条提交插入/.test(tableUi.last)
        && /15,078 fsync \/ 20,001 提交/.test(tableUi.last),
      JSON.stringify(tableUi));

    // 存成功就把弹窗收掉，结果交给 toast —— 用户改完就想接着干活，不该再点一次 ×
    await cdp.eval(`openSettings()`);
    await waitFor(cdp, `!!document.getElementById('setPoolPages')`, { label: '设置表单渲染' });
    // 外观组在无改动时按钮是「关闭」，保存用例必须先切回可保存的组
    await cdp.eval(`[...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'conn').click()`);
    await cdp.eval(`document.getElementById('btnSettingsSave').click()`);
    // 保存是异步的：等面板关闭 + toast 出现（它就是「保存成功」的可见信号）
    await waitFor(cdp, `(() => { const t = document.getElementById('toast');
      return document.getElementById('settingsPanel').classList.contains('hidden') &&
        !!t && !t.classList.contains('hidden'); })()`, { label: '保存完成并提示' });
    const saved = await cdp.eval(`(() => {
      const t = document.getElementById('toast');
      return {
        closed: document.getElementById('settingsPanel').classList.contains('hidden'),
        toast: (t.textContent || ''),
        shown: !t.classList.contains('hidden'),
      };
    })()`);
    check('设置：「保存并应用」成功后自动关闭 + 提示',
      saved.closed && saved.shown && /已保存/.test(saved.toast), JSON.stringify(saved));

    // 缓冲池页数：行能换出之后，这个值决定「每张表留多少热页的行在内存里」。
    // 改完要重启才生效，但值本身必须存得住（不然用户以为没保存）。
    await cdp.eval(`openSettings()`);
    await waitFor(cdp, `!!document.getElementById('setPoolPages')`, { label: '设置表单（缓冲池）' });
    const pool0 = await cdp.eval(`(() => { const n = document.getElementById('setPoolPages'); return n ? n.value : null; })()`);
    check('设置页有「缓冲池页数」字段', pool0 !== null && pool0 !== '', String(pool0));
    await cdp.eval(`[...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'perf').click()`);
    await sleep(200);
    // 高精度计时：开关默认开；状态行必须说清「此刻实际用的是哪个时钟」——
    // 开关写着开 ≠ 真的生效（标定要 1.5 秒，平台也可能不支持）。
    const timer = await cdp.eval(`(() => {
      const cb = document.getElementById('setHighRes');
      const notes = [...document.querySelectorAll('.set-note')].map(n => n.textContent.trim());
      return { has: !!cb, checked: cb ? cb.checked : null, note: notes.join(' | ') };
    })()`);
    check('设置→性能有「高精度计时（TSC）」开关，默认开',
      timer.has && timer.checked === true, JSON.stringify({ has: timer.has, checked: timer.checked }));
    check('计时状态如实显示（哪种时钟 + 当前刻度）',
      /^当前：/.test(timer.note) && /刻度/.test(timer.note), timer.note);
    await cdp.eval(`document.getElementById('setPoolPages').value = '512'`);
    await cdp.eval(`document.getElementById('setPoolPages').dispatchEvent(new Event('input', { bubbles: true }))`);
    await cdp.eval(`document.getElementById('btnSettingsSave').click()`);
    await waitFor(cdp, `document.getElementById('settingsPanel').classList.contains('hidden')`,
      { label: '保存后面板关闭' });
    await cdp.eval(`openSettings()`);
    await waitFor(cdp, `!!document.getElementById('setPoolPages')`, { label: '重新打开设置' });
    const pool1 = await cdp.eval(`document.getElementById('setPoolPages').value`);
    check('缓冲池页数能存住（512）', pool1 === '512', `保存前 ${pool0} → 重新打开 ${pool1}`);
    // 还原成打开时读到的值（默认是 4096 页 = 32MB），别把测试的副作用留在配置文件里
    await cdp.eval(`[...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'perf').click()`);
    await cdp.eval(`document.getElementById('setPoolPages').value = '${pool0}'`);
    await cdp.eval(`document.getElementById('setPoolPages').dispatchEvent(new Event('input', { bubbles: true }))`);
    await cdp.eval(`document.getElementById('btnSettingsSave').click()`);
    await sleep(1000);

    // 3) 历史面板
    await cdp.eval(`openHistory()`);
    await sleep(300);

    // 620px 的时候一条 SELECT 会被折成三四段，读起来像碎片 → 放宽到 860px，
    // 时间列定宽让多条记录的时间戳对齐成一列
    const hist = await cdp.eval(`(() => {
      const card = document.querySelector('#historyModal .modal-card');
      const item = document.querySelector('.hist-item');
      const sql = document.querySelector('.hist-sql');
      return {
        w: Math.round(card.getBoundingClientRect().width),
        items: document.querySelectorAll('.hist-item').length,
        cols: item ? getComputedStyle(item).gridTemplateColumns : '',
        clamp: sql ? getComputedStyle(sql).webkitLineClamp : '',
      };
    })()`);
    check('历史弹窗够宽（≥820px）+ 时间列定宽 + SQL 两行截断',
      hist.w >= 820 && hist.items > 0 && /^74px /.test(hist.cols) && hist.clamp === '2',
      `${hist.w}px / ${hist.items} 条 / 列 ${hist.cols} / clamp ${hist.clamp}`);

    // 74px 的时间列放不下年份，但历史跨年会留着 —— 完整时刻必须能从 title 读到，
    // 否则去年的 11/2 和今年的 11/2 在列表里完全一样。
    const histWhen = await cdp.eval(`(() => {
      const w = document.querySelector('.hist-when');
      return w ? { text: w.textContent, tip: w.title } : null;
    })()`);
    check('历史：时间列只显示 时:分，完整年月日挂在 title 上',
      histWhen && /^\d{2}:\d{2}$/.test(histWhen.text) && /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/.test(histWhen.tip),
      histWhen ? `${histWhen.text} · ${histWhen.tip}` : '没找到时间列');

    await cdp.eval(`document.getElementById('historyModal').dispatchEvent(new MouseEvent('click',{bubbles:true}))`);
    await sleep(200);
    check('历史：点遮罩后仍然打开', await cdp.eval(`!document.getElementById('historyModal').classList.contains('hidden')`));
    await cdp.eval(`document.getElementById('btnHistoryClose').click()`);
    await sleep(200);
    check('历史：点 × 能关掉', await cdp.eval(`document.getElementById('historyModal').classList.contains('hidden')`));

    // 4) 表设计器（本来就没挂遮罩关闭，顺手确认没被别处传染）
    await cdp.eval(`openDesigner(null)`);
    await sleep(400);
    await cdp.eval(`document.getElementById('designerModal').dispatchEvent(new MouseEvent('click',{bubbles:true}))`);
    await sleep(200);
    check('表设计器：点遮罩后仍然打开', await cdp.eval(`!document.getElementById('designerModal').classList.contains('hidden')`));

    // 默认值提示按类型给：原来所有列都写「如 now()」，id serial / name text 上都是错的。
    // 逐个切类型再读回占位符 —— 顺带验证「改类型后提示跟着换」这条联动，
    // 只断言初始值的话，改了类型还挂着旧提示这种问题测不出来。
    const defPh = await cdp.eval(`(() => {
      const tr = document.querySelector('#designerRows tr');
      const sel = tr.querySelector('.c-type select');
      const inp = tr.querySelector('.c-def input');
      const out = {};
      for (const t of ['int4', 'serial', 'text', 'timestamp', 'boolean', 'uuid']) {
        sel.value = t;
        sel.dispatchEvent(new Event('change', { bubbles: true }));
        out[t] = inp.placeholder;
      }
      return out;
    })()`);
    check('表设计器：默认值提示跟着列类型变（不是一律「如 now()」）',
      defPh.int4 === '如 0' && defPh.serial === '自增，可不填' && defPh.text === "如 'unknown'"
        && defPh.timestamp === '如 now()' && defPh.boolean === '如 TRUE' && /a0eebc99/.test(defPh.uuid),
      Object.entries(defPh).map(([k, v]) => `${k}→${v}`).join(', '));
    await cdp.eval(`document.getElementById('btnDesignerCancel').click()`);
    await sleep(300);

    // 5) 窄屏：左侧表结构默认收起成抽屉，点 ☰ 拉开
    //    注意 headless 的 --window-size 会把宽度钳到 500，所以这里显式设视口
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 390, height: 780, deviceScaleFactor: 1, mobile: true });
    await sleep(700);
    const mob = await cdp.eval(`(() => {
      const sb = document.getElementById('sidebar').getBoundingClientRect();
      return {
        sidebarRight: Math.round(sb.right),
        toggle: getComputedStyle(document.getElementById('btnSidebar')).display,
        bodyScroll: document.body.scrollWidth,
        inner: window.innerWidth,
      };
    })()`);
    check('窄屏下侧栏自动收起（不占位）', mob.sidebarRight <= 0, `右边界 ${mob.sidebarRight}px`);
    check('窄屏下有 ☰ 按钮', mob.toggle !== 'none', mob.toggle);
    check('窄屏顶栏不撑破页面（无横向滚动）',
      mob.bodyScroll <= mob.inner + 1, `${mob.bodyScroll} / ${mob.inner}`);

    // 秒表在窄屏让位：帮助 + 标题 + 执行 + 秒表 + 四个操作，那一行实在塞不下
    await cdp.eval(`setRunning(true)`);
    await sleep(200);
    const mobTimer = await cdp.eval(`getComputedStyle(document.getElementById('runTimer')).display`);
    await cdp.eval(`setRunning(false)`);
    check('窄屏下秒表自动让位（不挤执行按钮）', mobTimer === 'none', mobTimer);

    await cdp.eval(`document.getElementById('btnSidebar').click()`);
    await sleep(400);
    const opened = await cdp.eval(`(() => {
      const sb = document.getElementById('sidebar').getBoundingClientRect();
      return {
        left: Math.round(sb.left),
        w: Math.round(sb.width),
        cls: document.body.classList.contains('sidebar-open'),
        expanded: document.getElementById('btnSidebar').getAttribute('aria-expanded'),
        scrim: getComputedStyle(document.getElementById('sidebarScrim')).opacity,
      };
    })()`);
    check('点 ☰ 拉开抽屉（带遮罩）',
      opened.cls && opened.left === 0 && opened.w > 200 && Number(opened.scrim) > 0.5,
      `${opened.w}px，遮罩 ${opened.scrim}，aria-expanded=${opened.expanded}`);

    await cdp.eval(`document.getElementById('sidebarScrim').click()`);
    await sleep(300);
    check('点遮罩收起抽屉', await cdp.eval(`!document.body.classList.contains('sidebar-open')`));

    // 点表之后自动收起：手机上接着要看的是结果
    await cdp.eval(`document.getElementById('btnSidebar').click()`);
    await sleep(300);
    await cdp.eval(`document.querySelector('.table-row').click()`);
    await sleep(600);
    check('窄屏点表后抽屉自动收起',
      await cdp.eval(`!document.body.classList.contains('sidebar-open')`));

    // 转回宽屏：侧栏恢复常驻、☰ 消失、状态复位
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 1400, height: 900, deviceScaleFactor: 1, mobile: false });
    await sleep(700);
    const wide = await cdp.eval(`(() => {
      const sb = document.getElementById('sidebar').getBoundingClientRect();
      return {
        w: Math.round(sb.width),
        toggle: getComputedStyle(document.getElementById('btnSidebar')).display,
        cls: document.body.classList.contains('sidebar-open'),
      };
    })()`);
    check('转回宽屏：侧栏常驻、☰ 隐藏、状态复位',
      wide.w > 200 && wide.toggle === 'none' && !wide.cls,
      `侧栏 ${wide.w}px，☰ ${wide.toggle}`);

    // 诊断区三张表：10 行一页，空态和满页一样高 —— 高度不一致，切 Tab / 翻页就会跳。
    // 数据是注入的（走 renderSlowSQL 同一条路径），不依赖库里真有多少条慢 SQL。
    const pg = await cdp.eval(`(() => {
      toggleMonitor(true);
      setMonPage('diag');
      setMonTab('sql', true);
      const panel = document.getElementById('monTabSql');
      const pager = document.getElementById('monSqlPager');
      const body = document.getElementById('monSlowSQL');
      const setRows = n => {
        // 新结构：一行是一个「语句结构」的统计，不再是单次执行
        monData = { stmtLive: n, txLive: 0, slowSql: Array.from({ length: n }, (_, i) => ({
          n: 3, totalNs: 3e6, avgNs: 1e6, maxNs: 1.5e6, minNs: 8e5,
          firstMs: 1, lastMs: 2, tables: ['t'], write: false, sql: 'SELECT ' + i })) };
        renderSlowSQL(monData);
        return Math.round(panel.getBoundingClientRect().height);
      };
      const emptyH = setRows(0);
      const fullH = setRows(15);
      const pageRows = body.querySelectorAll('tr').length;
      pager.querySelectorAll('button')[1].click();
      return {
        emptyH, fullH, pageRows,
        page2Rows: body.querySelectorAll('tr').length,
        page2First: body.querySelector('tr').textContent,
        pagerText: pager.textContent,
      };
    })()`);
    check('诊断区空态与满页同高', pg.emptyH === pg.fullH && pg.emptyH > 300,
      `空 ${pg.emptyH}px / 满 ${pg.fullH}px`);
    check('一页只放 10 行', pg.pageRows === 10, `${pg.pageRows} 行`);
    check('翻页从第 11 条起', pg.page2Rows === 5 && pg.page2First.includes('SELECT 10'),
      `${pg.page2Rows} 行，首行 "${pg.page2First}"`);

    // 两个排序口径来自两份榜单（累计 / 单次最慢），切换只是换一份数据重画。
    // 一份榜单里放两个明显不同的结构，切过去必须换成另一个。
    const srt = await cdp.eval(`(() => {
      const row = (sql, n, total, max) => ({
        n, totalNs: total, avgNs: total / n, maxNs: max, minNs: total / n,
        firstMs: 1, lastMs: 2, tables: ['t'], write: false, sql });
      monData = {
        stmtLive: 2, txLive: 0,
        slowSql: [row('SELECT hot', 100, 1e8, 1e6), row('SELECT cold', 1, 5e7, 5e7)],
        slowSqlMax: [row('SELECT cold', 1, 5e7, 5e7), row('SELECT hot', 100, 1e8, 1e6)],
      };
      monSortBy = 'total'; renderSlowSQL(monData);
      const totalFirst = document.querySelector('#monSlowSQL tr .c-sql').textContent;
      monSortBy = 'max'; renderSlowSQL(monData);
      const maxFirst = document.querySelector('#monSlowSQL tr .c-sql').textContent;
      // 锁 Tab 没有这个维度，切换器要藏起来
      setMonTab('lock', true);
      const hiddenOnLock = document.getElementById('monSort').classList.contains('hidden');
      setMonTab('sql', true);
      const shownOnSQL = !document.getElementById('monSort').classList.contains('hidden');
      monSortBy = 'total';
      return { totalFirst, maxFirst, hiddenOnLock, shownOnSQL };
    })()`);
    check('排序切换：累计榜与单次榜各取各的',
      srt.totalFirst === 'SELECT hot' && srt.maxFirst === 'SELECT cold',
      `累计首行 ${srt.totalFirst} / 单次首行 ${srt.maxFirst}`);
    check('锁明细 Tab 藏起排序切换', srt.hiddenOnLock && srt.shownOnSQL,
      `lock ${srt.hiddenOnLock} / sql ${srt.shownOnSQL}`);

    // 长文本单元格：大字段能存 1MB，原来 pre-wrap 全文展开会把一行撑到几百像素
    // （实测 1200 字 = 411px），现在只显示三行预览、点开看全文。
    const cell = await cdp.eval(`(() => {
      const big = '长文本'.repeat(400);
      renderResults({ results: [{ command: 'SELECT', tag: 'SELECT 1', rowCount: 1,
        columns: [{ name: 'body', type: 'text' }], rows: [[big]], tookNs: 1000 }] }, 0);
      const tr = document.querySelector('#resultPane table.grid tbody tr');
      const h = tr ? tr.getBoundingClientRect().height : -1;
      const td = document.querySelector('#resultPane table.grid td.long');
      const clicked = !!td;
      if (td) td.click();
      const opened = !document.getElementById('cellModal').classList.contains('hidden');
      const meta = document.getElementById('cellMeta').textContent;
      const shown = document.getElementById('cellText').textContent.length;
      document.getElementById('cellClose').click();
      const closed = document.getElementById('cellModal').classList.contains('hidden');
      return { h, clicked, opened, meta, shown, closed, want: big.length };
    })()`);
    check('长文本单元格夹成几行预览', cell.clicked && cell.h > 0 && cell.h < 120,
      `行高 ${Math.round(cell.h)}px`);
    check('点开单元格能看全文并关闭', cell.opened && cell.closed && cell.shown === cell.want,
      `${cell.meta}，渲染 ${cell.shown} 字符`);

    // 结果 tab 的关闭键：关掉一个就少一个，编号顺延，只剩「信息」时也不报错
    const closable = await cdp.eval(`(() => {
      renderResults({ results: [
        { command: 'SELECT', tag: 'SELECT 1', rowCount: 1, columns: [{ name: 'a', type: 'int' }], rows: [[1]], tookNs: 1000 },
        { command: 'SELECT', tag: 'SELECT 1', rowCount: 1, columns: [{ name: 'b', type: 'int' }], rows: [[2]], tookNs: 1000 },
      ] }, 0);
      const before = [...document.querySelectorAll('#resultTabs .tab-label')].map(e => e.textContent);
      const hasX = document.querySelectorAll('#resultTabs .tab-x').length;
      document.querySelector('#resultTabs .tab-x').click();
      const after = [...document.querySelectorAll('#resultTabs .tab-label')].map(e => e.textContent);
      // 再执行一次要能重新出现（hiddenResults 在新结果时清空）
      renderResults({ results: [
        { command: 'SELECT', tag: 'SELECT 1', rowCount: 1, columns: [{ name: 'a', type: 'int' }], rows: [[1]], tookNs: 1000 },
        { command: 'SELECT', tag: 'SELECT 1', rowCount: 1, columns: [{ name: 'b', type: 'int' }], rows: [[2]], takes: 1, tookNs: 1000 },
      ] }, 0);
      const again = [...document.querySelectorAll('#resultTabs .tab-label')].map(e => e.textContent);
      return { before, hasX, after, again };
    })()`);
    check('结果 tab 可关闭且编号顺延',
      closable.hasX === 2 && closable.after.join('|') === '信息|结果 1' &&
      closable.before.join('|') === '信息|结果 1|结果 2', `${closable.before.join('|')} → ${closable.after.join('|')}`);
    check('重新执行后关掉的结果会回来',
      closable.again.join('|') === '信息|结果 1|结果 2', closable.again.join('|'));

    // 历史弹窗：搜索过滤 + 单条删除
    const histBox = await cdp.eval(`(() => {
      const k = 'godb.history';
      const seed = [
        { sql: 'SELECT * FROM t_hist_a', at: 1000 },
        { sql: 'SELECT count(*) FROM t_hist_b', at: 2000 },
      ];
      localStorage.setItem(k, JSON.stringify(seed));
      openHistoryPanel();
      const total = document.querySelectorAll('#historyBody .hist-item').length;
      const i = document.getElementById('histSearch');
      i.value = 'hist_b';
      i.dispatchEvent(new Event('input', { bubbles: true }));
      const filtered = document.querySelectorAll('#historyBody .hist-item').length;
      const hint = document.getElementById('historyHint').textContent;
      i.value = '';
      i.dispatchEvent(new Event('input', { bubbles: true }));
      document.querySelectorAll('#historyBody .hist-x')[0].click();
      const left = JSON.parse(localStorage.getItem(k) || '[]').length;
      document.getElementById('btnHistoryClose').click();
      localStorage.removeItem(k);
      return { total, filtered, hint, left };
    })()`);
    check('历史弹窗能按关键字过滤',
      histBox.total === 2 && histBox.filtered === 1 && histBox.hint.includes('匹配 1 条'),
      `${histBox.total} 条 → ${histBox.filtered} 条（${histBox.hint}）`);
    check('历史弹窗能单条删除', histBox.left === 1, `删完剩 ${histBox.left} 条`);

    /* 块级页地图：几十万页的表不能「一格一页」交给浏览器（20 万个 DOM 节点布局就卡死），
       所以后端按 step 页聚合成块，点进去才是逐页。这里用 ?pmblock=64 把 big_demo
       的 785 页压成 13 块来验证整条下钻链路 —— 真造一张二十万页的表跑不动测试。 */
    await cdp.send('Page.navigate', { url: URL + '&pmblock=64' });
    await waitFor(cdp, `typeof cm !== 'undefined' && cm && !!document.querySelector('.CodeMirror')`,
      { label: '刷新后控制台就绪', timeout: 15000 });
    await cdp.eval(`toggleMonitor(true)`);
    await cdp.eval(`gotoMap('big_demo')`);
    await waitFor(cdp, `document.querySelectorAll('#pmGrid .pm-c').length > 0`,
      { label: '块级页地图' });
    await settle(cdp);
    const bv = await cdp.eval(`(() => ({
      n: document.querySelectorAll('#pmGrid .pm-c').length,
      note: (document.getElementById('mapNote') || {}).textContent || '',
      crumb: (document.getElementById('mapCrumbs') || {}).textContent || '',
      tip: (document.querySelector('#pmGrid .pm-c') || {}).title || '',
      more: !document.getElementById('pmMore').classList.contains('hidden'),
    }))()`);
    const wantBlk = Math.ceil(seedTotal / 64);
    check('页数超阈值时按块显示（一块 = step 页），并写明「单击一块看逐页」',
      bv.n === wantBlk && bv.note.includes(`${wantBlk} 块（每块 64 页）`)
      && /每格 64 页/.test(bv.crumb) && /第 0–63 页/.test(bv.tip) && !bv.more,
      `${bv.n} 块（共 ${seedTotal} 页）· ${bv.note}`);

    await cdp.eval(`document.querySelector('#pmGrid .pm-c').click()`);
    await sleep(1200);
    const bvi = await cdp.eval(`(() => ({
      n: document.querySelectorAll('#pmGrid .pm-c').length,
      first: (document.querySelector('#pmGrid .pm-c') || {}).dataset.no,
      crumb: (document.getElementById('mapCrumbs') || {}).textContent || '',
      note: (document.getElementById('mapNote') || {}).textContent || '',
    }))()`);
    check('点一块钻到逐页：只读这一块，面包屑多一级',
      bvi.n === Math.min(64, seedTotal) && bvi.first === '0'
      && /第 0–63 页/.test(bvi.crumb) && bvi.note.includes(`共 ${seedTotal} 页`),
      `${bvi.n} 格 · ${bvi.crumb}`);

    // 退要一级一级退：块内 → 块视图 → 库视图
    await cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', windowsVirtualKeyCode: 27 });
    await settle(cdp);
    const esc1 = await cdp.eval(`({
      n: document.querySelectorAll('#pmGrid .pm-c').length,
      crumb: (document.getElementById('mapCrumbs') || {}).textContent || '',
    })`);
    await cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', windowsVirtualKeyCode: 27 });
    await sleep(800);
    const esc2 = await cdp.eval(`({
      tree: !document.getElementById('mapTree').classList.contains('hidden'),
      level: mapLevel,
    })`);
    check('从块内按 Esc 一级一级退：块内 → 块视图 → 库视图',
      esc1.n === wantBlk && /每格 64 页/.test(esc1.crumb) && esc2.tree && esc2.level === 'db',
      `第一次 ${esc1.n} 块 · 第二次 level=${esc2.level}`);

    /* 块级「整块回收建议」：判据在后端（洞 > 容量 1/4，和 needsVacuum 同口径），
       前端只负责说清「这块值得整理」和「VACUUM 是整表动作，只填不执行」。
       这里不去造一张真能留洞的表 —— checkpoint 有时会把页紧凑重写，洞不稳定；
       直接喂一块带 advice 的块数据，验证渲染、文案和按钮这条链路。 */
    await cdp.send('Page.navigate', { url: URL + '&pmblock=64' });
    await waitFor(cdp, `typeof cm !== 'undefined' && cm && !!document.querySelector('.CodeMirror')`,
      { label: '刷新后控制台就绪', timeout: 15000 });
    await cdp.eval(`toggleMonitor(true)`);
    await cdp.eval(`gotoMap('big_demo')`);
    await waitFor(cdp, `document.querySelectorAll('#pmGrid .pm-c').length > 0`,
      { label: '块级页地图' });
    await settle(cdp);
    await cdp.eval(`(() => {
      pmMap = { table: pmSel.table, kind: 'data', total: 1000, step: 256, vacuumThreshold: 4, blocks: [
        { from: 0, to: 256, pages: 256, live: 100, slots: 300, used: 200000, free: 8000,
          fill: 20, holes: 40, holeBytes: 900000, empty: 0, bad: 0, advice: 'vacuum' },
        { from: 256, to: 512, pages: 256, live: 0, slots: 0, used: 0, free: 0,
          fill: 0, holes: 0, holeBytes: 0, empty: 256, bad: 0 },
      ] };
      pmSig = '';
      renderPageMap();
    })()`);
    await sleep(500);
    const vacUi = await cdp.eval(`(() => {
      const cells = [...document.querySelectorAll('#pmGrid .pm-c')];
      return {
        n: cells.length,
        tip: cells[0] ? cells[0].title : '',
        btn: !document.getElementById('pmVac').classList.contains('hidden'),
        note: (document.getElementById('mapNote') || {}).textContent || '',
      };
    })()`);
    check('块里洞超过容量的 1/4 时给「建议整理」，并亮出「填入 VACUUM」按钮',
      vacUi.n === 2 && /建议整理/.test(vacUi.tip) && /1\/4/.test(vacUi.tip)
        && vacUi.btn && /1 块待整理/.test(vacUi.note),
      `2 块 · 按钮 ${vacUi.btn} · ${vacUi.note.slice(-24)}`);

    await cdp.eval(`document.getElementById('pmVac').click()`);
    await sleep(700);
    const vacSql = await cdp.eval(`editor.value`);
    const vacClosed = await cdp.eval(`!monOpen`);
    check('点「填入 VACUUM」只把语句写进编辑器，不替你执行',
      /VACUUM big_demo;/.test(vacSql) && vacClosed,
      `编辑器尾部 ${JSON.stringify(vacSql.slice(-24))} · 面板已关 ${vacClosed}`);

    /* 顶栏「首页」：任何时候一键回编辑器。监控打开时选中态要给到「监控」，
       点首页之后换回「首页」—— 不然顶栏三个入口看起来永远都是平的，不知道自己在哪。 */
    await cdp.eval(`toggleMonitor(true)`);
    await sleep(500);
    const navBefore = await cdp.eval(`(() => ({
      home: document.getElementById('btnHome').classList.contains('on'),
      mon: document.getElementById('btnMonitor').classList.contains('on'),
      open: !document.getElementById('monPanel').classList.contains('hidden'),
    }))()`);
    await cdp.eval(`document.getElementById('btnHome').click()`);
    await sleep(500);
    const navAfter = await cdp.eval(`(() => ({
      home: document.getElementById('btnHome').classList.contains('on'),
      mon: document.getElementById('btnMonitor').classList.contains('on'),
      open: !document.getElementById('monPanel').classList.contains('hidden'),
      order: [...document.querySelectorAll('.top-actions .btn')].map(b => b.id),
    }))()`);
    check('顶栏有「首页」：监控打开时点它回编辑器，选中态跟着换',
      navBefore.open && !navBefore.home && navBefore.mon
      && !navAfter.open && navAfter.home && !navAfter.mon
      && navAfter.order[0] === 'btnHome',
      `监控 ${navBefore.open} → 编辑器 ${!navAfter.open} · 顶栏 ${navAfter.order.join('/')}`);

    /* 「科技灰」的四项增强：顶栏光带、十字准星光标、右键拦截、皮肤名。
       这些只在 steel 上开（其它皮肤一行都不该变），所以先切皮肤再断言，
       最后切回淡蓝，检查它确实没被污染 —— 浅色皮肤上叠一层蓝光会立刻显脏。 */
    await cdp.eval(`applyTheme('steel', false)`);
    await sleep(300);
    const fx = await cdp.eval(`(() => {
      const after = getComputedStyle(document.querySelector('.topbar'), '::after');
      const fire = (el, shift) => {
        const ev = new MouseEvent('contextmenu', { bubbles: true, cancelable: true, shiftKey: !!shift });
        el.dispatchEvent(ev);
        return ev.defaultPrevented;
      };
      const blank = document.getElementById('monBody');
      return {
        scan: /gradient/.test(after.backgroundImage || 'none'),
        cursor: getComputedStyle(document.querySelector('.mon-body')).cursor.startsWith('url('),
        labels: THEME_LIST.map(t => t.name),
        blocked: fire(blank),
        editor: fire(document.querySelector('.CodeMirror')),
        shift: fire(blank, true),
      };
    })()`);
    // 编辑器也从「放行」改成「拦住并弹自己的菜单」，所以这里期望 true；
    // 菜单本身在下面单独测。
    check('「科技灰」：顶栏光带 + 十字光标 + 右键拦截（编辑器弹自己的菜单，Shift 放行）',
      fx.scan && fx.cursor && fx.blocked && fx.editor && !fx.shift
      && fx.labels[0] === '科技灰',
      `${fx.labels.join(' / ')} · 拦截 ${fx.blocked} / 编辑器 ${fx.editor} / Shift ${fx.shift}`);

    /* 焦点环不能画在 outline 上：outline-offset 会在它和 border 之间留一道 1px 缝，
       缝里露出页面底色 —— 放大 4 倍就是「两条蓝线夹一条黑缝」，用户就是瞄到它来报的。
       现在改用 box-shadow 的环（紧贴 border，没有缝），transparent outline 只给
       高对比度模式占位。 */
    await cdp.eval(`document.getElementById('dbModal').classList.remove('hidden')`);
    await sleep(200);
    const ringBox = await cdp.eval(`(() => {
      const r = document.getElementById('dbNewName').getBoundingClientRect();
      return { x: Math.round(r.x + 40), y: Math.round(r.y + r.height / 2) };
    })()`);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: ringBox.x, y: ringBox.y, button: 'left', clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: ringBox.x, y: ringBox.y, button: 'left', clickCount: 1 });
    await sleep(300);
    const ring = await cdp.eval(`(() => {
      const el = document.getElementById('dbNewName');
      const cs = getComputedStyle(el);
      return {
        outline: cs.outlineColor,
        width: cs.outlineWidth,
        shadow: cs.boxShadow,
        focused: el.matches(':focus-visible'),
      };
    })()`);
    await cdp.eval(`document.getElementById('dbModal').classList.add('hidden')`);
    check('聚焦输入框只有一圈光，没有 outline 留下的 1px 缝',
      ring.focused && (ring.outline.includes('0, 0, 0, 0') || ring.outline === 'transparent')
      && ring.shadow.includes('3px'),
      `outline ${ring.width} ${ring.outline} · ${ring.shadow}`);

    await cdp.eval(`applyTheme('sky', false)`);
    await sleep(300);
    const fxSky = await cdp.eval(`(() => ({
      scan: getComputedStyle(document.querySelector('.topbar'), '::after').backgroundImage,
      cursor: getComputedStyle(document.querySelector('.mon-body')).cursor,
    }))()`);
    check('这些特效不外溢到其它皮肤（淡蓝下没有光带、没有定制光标）',
      !/gradient/.test(fxSky.scan || 'none') && !/^url/.test(fxSky.cursor),
      `光带 ${(fxSky.scan || 'none').slice(0, 12)} · 光标 ${fxSky.cursor}`);

    /* 编辑器右键：自己画菜单（浏览器那张在深色界面上是块灰白板子）。
       顺便测「有选区只跑选区」—— 改一条语句再试，不用把整段删掉重写。 */
    // 换行用 fromCharCode(10) 拼：这段在模板字符串里，写 \n 会被解成真换行，
    // 传过去的 JS 就变成「字符串字面量跨行」——SyntaxError。
    await cdp.eval(`cm.setValue(['SELECT 1 AS a;', 'SELECT 2 AS b;'].join(String.fromCharCode(10)))`);
    await sleep(200);
    const ctx = await cdp.eval(`(() => {
      const box = document.querySelector('.CodeMirror');
      const r = box.getBoundingClientRect();
      const ev = new MouseEvent('contextmenu', {
        bubbles: true, cancelable: true, clientX: r.left + 60, clientY: r.top + 18,
      });
      box.dispatchEvent(ev);
      const m = document.getElementById('ctxMenu');
      const b = m.getBoundingClientRect();
      return {
        prevented: ev.defaultPrevented,
        open: !m.classList.contains('hidden'),
        items: [...m.querySelectorAll('.ctx-item')].map(x => x.textContent.trim()),
        seps: m.querySelectorAll('.ctx-sep').length,
        inView: b.right <= window.innerWidth + 1 && b.bottom <= window.innerHeight + 1,
      };
    })()`);
    // 8 项：剪切/复制/粘贴/全选/美化/插入 SQL 模板/历史/清空（模板那项另有专门断言）。
    // 「执行」不在菜单里 —— 编辑器已经有 Ctrl+Enter / F5 / 工具栏的执行按钮。
    check('编辑器右键弹出自己的菜单（剪切/复制/粘贴/全选/美化/模板/历史/清空）',
      ctx.prevented && ctx.open && ctx.items.length === 8 && ctx.seps === 3 && ctx.inView,
      ctx.items.join(' · '));

    // 工具栏右端只剩执行计划 / 执行探测。历史、清空、美化都在右键里。
    const toolbar = await cdp.eval(`({
      history: !!document.getElementById('btnHistory'),
      clear: !!document.getElementById('btnClear'),
      format: !!document.getElementById('btnFormat'),
      probe: (document.getElementById('btnProbe') || {}).textContent,
      actions: [...document.querySelectorAll('.editor-actions button')].map(b => b.textContent.trim()),
    })`);
    check('工具栏右端只留「执行计划 / 执行探测」，历史清空美化都收进右键',
      !toolbar.history && !toolbar.clear && !toolbar.format &&
        toolbar.probe === '执行探测' &&
        toolbar.actions.join('/') === '执行计划/执行探测',
      toolbar.actions.join(' / '));

    // 右键里的「清空」要真的清空。以前工具栏那颗改的是隐藏的 textarea，
    // CodeMirror 会把自己的内容同步回去，看着像「点了没反应」。
    await cdp.eval(`(() => {
      cm.setValue('SELECT 1');
      openEditorMenu(40, 40);
      [...document.querySelectorAll('#ctxMenu .ctx-item')]
        .find(x => x.textContent.trim() === '清空').click();
    })()`);
    await sleep(250);
    check('右键「清空」真的清空（CodeMirror 下也生效）',
      await cdp.eval(`cm.getValue() === ''`),
      JSON.stringify(await cdp.eval(`cm.getValue()`)));

    await cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', windowsVirtualKeyCode: 27 });
    await sleep(250);
    const ctxClosed = await cdp.eval(`document.getElementById('ctxMenu').classList.contains('hidden')`);
    check('右键菜单按 Esc 就关，不连带关掉别的面板', ctxClosed, `隐藏 ${ctxClosed}`);

    /* 右键这一下本身会让 CodeMirror 滚一次（mousedown 要把光标挪到点击处，
       顺手 scrollIntoView），那个 scroll 事件是异步派发的、落在菜单画出来之后。
       曾经没有闸门，菜单打开几毫秒就被自己关掉 —— 症状是「美化之后第一次右键
       没反应，要点第二次」。必须走真实事件流（pointerdown + contextmenu），
       直接调 openEditorMenu() 测不出来：那条路根本没产生滚动。 */
    await cdp.eval(`cm.setValue('select d.name, count(e.id) as n from departments d left join employees e on e.dept_id=d.id group by d.name order by n desc;')`);
    await cdp.eval(`formatSQL()`);
    await sleep(300);
    const ctxFirst = await cdp.eval(`(() => {
      const el = document.querySelector('.CodeMirror');
      const r = el.getBoundingClientRect();
      const x = Math.round(r.left + r.width / 2), y = Math.round(r.top + 40);
      const tgt = document.elementFromPoint(x, y) || el;
      tgt.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, cancelable: true, clientX: x, clientY: y, button: 2, buttons: 2, pointerType: 'mouse' }));
      tgt.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: x, clientY: y, button: 2 }));
      const m = document.getElementById('ctxMenu');
      return { hidden: m.classList.contains('hidden'), items: m.querySelectorAll('.ctx-item').length };
    })()`);
    await sleep(260); // 跨过那次补偿滚动的异步派发
    const ctxStill = await cdp.eval(`(() => {
      const m = document.getElementById('ctxMenu');
      const r = m.getBoundingClientRect();
      return { hidden: m.classList.contains('hidden'), w: Math.round(r.width), h: Math.round(r.height) };
    })()`);
    check('美化之后第一次右键，菜单不会被自己触发的那次滚动关掉',
      ctxFirst.hidden === false && ctxStill.hidden === false && ctxStill.w > 0,
      `打开后立即 ${JSON.stringify(ctxFirst)} · 260ms 后 ${JSON.stringify(ctxStill)}`);

    /* SQL 模板是**三级**的：右键 → 分类（4 个）→ 具体语句。15 条摊平要 577px，
       一屏菜单太长；分下去每层最多 5 行。
       验：分类那一层短、贴主菜单右侧；hover 分类飞出语句；点一条就插进编辑器，
       表名自动填、同名占位符一起选中、两层菜单都收起。 */
    await cdp.eval(`lastTableName = 'employees'; cm.setValue(''); openEditorMenu(320, 200)`);
    await sleep(300);
    await cdp.eval(`(() => {
      const it = [...document.querySelectorAll('#ctxMenu .ctx-item')].find(x => x.textContent.includes('插入 SQL 模板'));
      it.dispatchEvent(new MouseEvent('mouseenter'));
    })()`);
    await sleep(350);
    const tplUi = await cdp.eval(`(() => {
      const s = document.getElementById('ctxSub');
      const r = s.getBoundingClientRect();
      const m = document.getElementById('ctxMenu').getBoundingClientRect();
      return {
        open: !s.classList.contains('hidden'),
        labels: [...s.querySelectorAll('.ctx-item')].map(b => b.firstChild.textContent),
        counts: [...s.querySelectorAll('.ctx-item kbd')].map(k => k.textContent),
        h: Math.round(r.height),
        right: r.left >= m.right,
        inView: r.right <= window.innerWidth + 1 && r.bottom <= window.innerHeight + 1,
      };
    })()`);
    check('右键「插入 SQL 模板」只列 4 个分类（15 条不摊平），贴主菜单右侧且短',
      tplUi.open && tplUi.labels.join('/') === '建表/修改表/查询/更新与删除'
      && tplUi.counts.join('/') === '2 条/4 条/5 条/4 条'
      && tplUi.right && tplUi.inView && tplUi.h < 220,
      `${tplUi.labels.join('/')} · ${tplUi.counts.join('/')} · 高 ${tplUi.h}px`);

    await cdp.eval(`(() => {
      const b = [...document.getElementById('ctxSub').querySelectorAll('.ctx-item')]
        .find(x => x.firstChild.textContent === '查询');
      b.dispatchEvent(new MouseEvent('mouseenter'));
    })()`);
    await sleep(350);
    const tplLv3 = await cdp.eval(`(() => {
      const s = document.getElementById('ctxSub2');
      const r = s.getBoundingClientRect();
      const p = document.getElementById('ctxSub').getBoundingClientRect();
      return {
        open: !s.classList.contains('hidden'),
        labels: [...s.querySelectorAll('.ctx-item')].map(b => b.firstChild.textContent),
        right: r.left >= p.right,
        inView: r.right <= window.innerWidth + 1 && r.bottom <= window.innerHeight + 1,
      };
    })()`);
    check('hover 分类飞出第三级语句（查询 → 5 条），贴分类面板右侧、不出屏',
      tplLv3.open && tplLv3.labels.length === 5
      && tplLv3.labels[0] === '单表查询' && tplLv3.labels[4] === '子查询 EXISTS'
      && tplLv3.right && tplLv3.inView,
      tplLv3.labels.join('/'));

    await cdp.eval(`(() => {
      const b = [...document.querySelectorAll('#ctxSub2 .ctx-item')].find(x => x.firstChild.textContent === '分组统计');
      b.click();
    })()`);
    await sleep(400);
    const tplIns = await cdp.eval(`(() => ({
      text: cm.getValue(),
      sels: cm.listSelections().length,
      selText: cm.listSelections().map(x => cm.getRange(x.anchor, x.head)),
      closed: [document.getElementById('ctxMenu'), document.getElementById('ctxSub'), document.getElementById('ctxSub2')]
        .every(m => m.classList.contains('hidden')),
    }))()`);
    check('点一条语句：表名自动填、同名占位符一起选中、三层菜单一起收起',
      /FROM employees/.test(tplIns.text) && /GROUP BY 分组列/.test(tplIns.text)
      && tplIns.sels === 2 && tplIns.selText.every(t => t === '分组列') && tplIns.closed,
      `选中 ${tplIns.sels} 处 ${JSON.stringify(tplIns.selText)} · 收起 ${tplIns.closed}`);

    // 自带内容，别依赖前面谁留下的编辑器状态（模板断言会改它）
    await cdp.eval(`cm.setValue(['SELECT 1 AS a;', 'SELECT 2 AS b;'].join(String.fromCharCode(10)))`);
    await cdp.eval(`(() => {
      const d = cm.getDoc();
      d.setSelection({ line: 0, ch: 0 }, { line: 0, ch: cm.getLine(0).length });
    })()`);
    await cdp.eval(`document.getElementById('btnRun').click()`);
    await waitIdle(cdp);
    const selRun = await cdp.eval(`lastSubmittedSQL`);
    check('有选区时只执行选区（改一句不用把整段删掉重写）',
      selRun === 'SELECT 1 AS a;', JSON.stringify(selRun));

    /* 编辑器里的两处标记：当前语句（行号列一条竖线）+ 出错行（红底 + 行号变红）。
       竖线是行级的（addLineClass 到 gutter），不再是字符级的 markText ——
       所以这里验「哪几行被标」，而不是「标了哪段文字」。 */
    await cdp.eval(`cm.setValue(['SELECT 1 AS a;','','SELECT 2 AS b,','  3 AS c;','SELECT 4 AS d;'].join(String.fromCharCode(10)))`);
    await cdp.eval(`cm.setCursor({line:2,ch:4})`);
    await sleep(300);
    const stmt = await cdp.eval(`(() => {
      const g = i => (cm.getLineHandle(i) || {}).gutterClass || '';
      return { l0: g(0), l1: g(1), l2: g(2), l3: g(3), l4: g(4),
               bg: document.querySelectorAll('.CodeMirror-gutter-background.cm-stmt-line').length,
               marks: cm.getAllMarks().length };
    })()`);
    check('光标所在的那条语句在行号列标竖线，且只标这一句（多语句脚本里不迷路）',
      stmt.l2 === 'cm-stmt-line' && stmt.l3 === 'cm-stmt-line'
      && stmt.l0 === '' && stmt.l1 === '' && stmt.l4 === '' && stmt.bg === 2 && stmt.marks === 0,
      JSON.stringify(stmt));

    // 语法错：第 2 行第 8 列 → 红底 + 行号露出来 + 光标跳到出错位置
    await cdp.eval(`cm.setValue(['SELECT 1;','SELECT FROM;','SELECT 3;'].join(String.fromCharCode(10)))`);
    await cdp.eval(`document.getElementById('btnRun').click()`);
    await waitIdle(cdp);
    const errMark = await cdp.eval(`(() => ({
      lines: document.querySelectorAll('.cm-errline').length,
      gutter: cm.getOption('lineNumbers'),
      cur: cm.getCursor(),
    }))()`);
    check('SQL 报错时标红出错那行（行号常驻，这一步只验红行）',
      errMark.lines === 1 && errMark.gutter === true
      && errMark.cur.line === 1 && errMark.cur.ch === 7,
      `红行 ${errMark.lines} · 行号 ${errMark.gutter} · 光标 ${errMark.cur.line}:${errMark.cur.ch}`);

    // 执行成功 → 标记清掉、行号收回
    await cdp.eval(`cm.setValue('SELECT 42 AS ok;')`);
    await cdp.eval(`document.getElementById('btnRun').click()`);
    await waitIdle(cdp);
    const errGone = await cdp.eval(`(() => ({
      lines: document.querySelectorAll('.cm-errline').length,
      gutter: cm.getOption('lineNumbers'),
    }))()`);
    check('执行成功后红行清掉，行号常驻不动',
      errGone.lines === 0 && errGone.gutter === true,
      `红行 ${errGone.lines} · 行号 ${errGone.gutter}`);

    /* 执行中可以停：连点两次同一颗按钮（第一次开跑、第二次停止）。
       不用真造慢查询 —— abort 是前端立刻生效的，这里验的是按钮态和面板文案。 */
    await cdp.eval(`cm.setValue('SELECT 1 AS a;')`);
    await sleep(150);
    await cdp.eval(`(() => { const b = document.getElementById('btnRun'); b.click(); b.click(); })()`);
    await sleep(900);
    const stopUi = await cdp.eval(`(() => ({
      label: document.getElementById('btnRunText').textContent,
      running: document.getElementById('btnRun').classList.contains('is-running'),
      text: document.getElementById('results').textContent.slice(0, 60),
    }))()`);
    check('执行中点同一颗按钮就是「停止」：界面立刻放行，并说明语句会跑完',
      stopUi.label === '执行' && !stopUi.running && stopUi.text.includes('已停止'),
      `${stopUi.label} · ${stopUi.text.replace(/\s+/g, ' ').slice(0, 30)}`);

    // 有选区时按钮写「执行选中」
    await cdp.eval(`(() => { const d = cm.getDoc(); d.setSelection({line:0,ch:0},{line:0,ch:6}); })()`);
    await sleep(250);
    const selBtn = await cdp.eval(`document.getElementById('btnRunText').textContent`);
    await cdp.eval(`cm.setCursor({line:0,ch:0})`);
    await sleep(250);
    const allBtn = await cdp.eval(`document.getElementById('btnRunText').textContent`);
    check('有选区时按钮写「执行选中」，没选区回到「执行」',
      selBtn === '执行选中' && allBtn === '执行', `${selBtn} → ${allBtn}`);

    // 表树右键：插入 SELECT 只插不跑
    const insSel = await cdp.eval(`(() => {
      cm.setValue('');
      lastSubmittedSQL = null;
      const items = tableMenuItems({ name: 'big_demo' });
      const it = items.find(x => x.label === '插入 SELECT');
      if (!it) return { has: false };
      it.run();
      return { has: true, text: editor.value, ran: lastSubmittedSQL };
    })()`);
    check('表树右键「插入 SELECT」：插到光标处但不执行',
      insSel.has && /SELECT \* FROM big_demo/.test(insSel.text || '') && insSel.ran === null,
      JSON.stringify((insSel.text || '').slice(0, 40)));

    // 大结果集提示：表格只画当前页，但数据是全量进来的
    await cdp.eval(`cm.setValue('SELECT * FROM big_demo LIMIT 3000')`);
    await cdp.eval(`document.getElementById('btnRun').click()`);
    await waitIdle(cdp);
    const bigWarn = await cdp.eval(`(() => {
      const w = document.querySelector('#results .grid-warn');
      return { has: !!w, text: w ? w.textContent.slice(0, 40) : '' };
    })()`);
    check('结果超过 2000 行时提示「数据已全量载入，表格只画当前页」',
      bigWarn.has && /3,?000 行/.test(bigWarn.text), bigWarn.text || '没有提示');

    /* 探测执行：真跑一遍，给出「每一步花了多少」。
       这三条正好是「把 EXPLAIN 文本排版好看」做不到的：① 每步都有行数（以前只有
       扫描节点有数字）；② 最贵的一步被点出来（独占时间，不是累计时间）；
       ③ 视图是算子账本而不是一张 QUERY PLAN 文本表格。 */
    await cdp.eval(`cm.setValue('SELECT v, count(*) AS n FROM big_demo GROUP BY v ORDER BY n DESC LIMIT 5')`);
    await cdp.eval(`document.getElementById('btnProbe').click()`);
    await waitFor(cdp, `document.querySelectorAll('.probe-row').length > 0`,
      { label: '探测算子账本', timeout: 20000 });
    const probe = await cdp.eval(`(() => {
      const rows = [...document.querySelectorAll('.probe-row')];
      const worst = document.querySelector('.probe-row.is-worst');
      return {
        rows: rows.length,
        names: rows.map(r => (r.querySelector('.probe-name') || {}).textContent || ''),
        seqs: rows.map(r => Number((r.querySelector('.probe-seq') || {}).textContent)),
        chips: rows.filter(r => r.querySelector('.probe-chip')).length,
        worst: worst ? (worst.querySelector('.probe-name') || {}).textContent : '',
        verdict: (document.querySelector('.probe-verdict-text') || {}).textContent || '',
        tab: [...document.querySelectorAll('.result-tab')].map(b => b.textContent).join(','),
        raw: (() => {
          const b = document.querySelector('.probe-raw');
          return b ? b.classList.contains('show') : null;
        })(),
      };
    })()`);
    check('执行探测给出算子账本（不是 QUERY PLAN 文本表格）',
      probe.rows >= 4 && probe.tab.includes('探测结果'),
      `${probe.rows} 行 · tab=${probe.tab}`);
    check('探测里每一步都有行数，不只最底下那行',
      probe.chips >= 4, `${probe.chips} 行带计数标签`);
    /* 时长单位：探测卡上的每一段都是「纳秒 → ns/µs/ms」。上面那个重名事故就是
       从这里露头的，所以直接断言「不许出现秒」——「12 秒」读起来太合理，只看截图
       根本不会怀疑，只能靠断言把这个口子封住。 */
    const units = await cdp.eval(`(() => {
      const legend = document.querySelector('.probe-phase-legend');
      const times = [...document.querySelectorAll('.probe-row')]
        .map(r => { const c = r.querySelector('.probe-chip'); return c ? c.textContent : ''; })
        .filter(Boolean);
      return { legend: legend ? legend.textContent : '', times };
    })()`);
    {
      const all = units.legend + ' ' + units.times.join(' ');
      check('探测卡的时长用 ns/µs/ms，没有把纳秒当毫秒显示成「秒」',
        !/秒|\d+:\d+/.test(all) && /(ns|µs|ms)/.test(all), units.legend.slice(0, 64));
    }

    check('最贵的一步被标出来，并有一句结论',
      /HashAggregate/.test(probe.worst) && /占 \d+%/.test(probe.verdict),
      `${probe.worst} · ${probe.verdict.slice(0, 40)}`);
    /* 左边那列数字是**执行顺序**（后序：子先父后、同级左先右），不是行号。
       按显示顺序编号会把 Project/Limit 说成第一步，正好反了 —— 这条同时钉住
       「1..N 不重不漏」和「根节点排最后」，只改样式不改语义的话它不会红。 */
    const seqs = probe.seqs;
    const asc = seqs.slice().sort((a, b) => a - b);
    check('每行左边是执行序号：1..N 不重不漏，根节点排最后',
      seqs.length === probe.rows && asc.every((v, i) => v === i + 1) && seqs[0] === probe.rows,
      seqs.join(', '));

    /* 物化算子的内存占用：全量排序把 5 万行拉进内存，这块**不在页池预算里**，
       是「进程内存为什么涨」最可能的答案。以前探测只能说「5 万行、55ms」，
       说不出它吃了多少 —— 数字要显示，且必须让人看出是下限（「约」+ title 说明）。 */
    await cdp.eval(`cm.setValue('SELECT id, v FROM big_demo ORDER BY v DESC')`);
    await cdp.eval(`document.getElementById('btnProbe').click()`);
    // 等的是「出现 Sort 行」，不是「有 .probe-row」—— 上一轮的探测卡还在 DOM 里，
    // 只等行数大于 0 会立刻返回，然后去读上一轮的结果（那轮是 Top-N，没有 Sort）。
    await waitFor(cdp, `[...document.querySelectorAll('.probe-name')].some(n => /^Sort/i.test(n.textContent.trim()))`,
      { label: '排序探测', timeout: 20000 });
    const mem = await cdp.eval(`(() => {
      const rows = [...document.querySelectorAll('.probe-row')];
      const sortRow = rows.find(r => /^Sort/i.test(((r.querySelector('.probe-name') || {}).textContent || '').trim()));
      const chip = sortRow ? sortRow.querySelector('.probe-chip.mem') : null;
      return {
        has: !!chip,
        text: chip ? chip.textContent : '',
        tip: chip ? chip.title : '',
      };
    })()`);
    check('全量排序显示内存占用，并说明这是下限',
      mem.has && /约 [\d.]+ MB/.test(mem.text) && /下限/.test(mem.tip),
      `${mem.text} · ${mem.tip.slice(0, 26)}`);
    /* 表达式求值单独计时：`ORDER BY 函数(...)` / 向量距离的时间记在排序算子的
       「独占」里，只显示「Sort 12ms」会让人以为是排序本身慢。这条钉住「求值」标签，
       以及 tooltip 说清楚它含在独占时间里。 */
    await cdp.eval(`cm.setValue('SELECT id FROM big_demo ORDER BY lower(v) LIMIT 20')`);
    await cdp.eval(`document.getElementById('btnProbe').click()`);
    await waitFor(cdp, `[...document.querySelectorAll('.probe-chip.eval')].length > 0`,
      { label: '求值计时标签', timeout: 20000 });
    const evalChip = await cdp.eval(`(() => {
      const chip = document.querySelector('.probe-chip.eval');
      return { text: chip ? chip.textContent : '', tip: chip ? chip.title : '' };
    })()`);
    check('排序里算表达式的时间单独标出来（不是全算成「排序慢」）',
      /^求值 /.test(evalChip.text) && /独占/.test(evalChip.tip) && /距离|表达式/.test(evalChip.tip),
      `${evalChip.text} · ${evalChip.tip.slice(0, 30)}`);
    /* 过滤只发生在展示层：服务端照实报（一个分组的聚合也有 56 B），前端按
       256KB 以下不显示。这条把分工钉住 —— 哪天有人把阈值挪到服务端，数据就少了。
       注意不能用 `count(*)` 探：那条会走 Fast Aggregate（直接读行数），
       压根不进聚合算子的内存，找不到节点；得用 count(列) 逼出真聚合。 */
    const small = await cdp.eval(`(async () => {
      const r = await fetch('/api/query', { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ sql: 'EXPLAIN ANALYZE SELECT count(v) FROM big_demo' }) }).then(x => x.json());
      const find = n => {
        if ((n.memBytes || 0) > 0) return n;
        for (const c of (n.children || [])) { const f = find(c); if (f) return f; }
        return null;
      };
      const hit = find(r.results[0].planTree);
      return hit ? { name: hit.name, memRows: hit.memRows || 0, memBytes: hit.memBytes || 0 } : null;
    })()`);
    check('小块内存服务端照实报，由前端按 256KB 过滤展示',
      small && small.memRows === 1 && small.memBytes > 0 && small.memBytes < 256 * 1024,
      JSON.stringify(small));

    /* 探测卡要说清「这轮读没读盘」。以前没读盘时界面上什么都不说，
       于是「扫描 12 行」被当成「从磁盘读了 12 行」（真被这么问过）。
       两种状态都算过，但两种都必须把「扫描行数」和「读盘页数」分开讲。 */
    const ioLine = await cdp.eval(`(() => {
      const el = document.querySelector('.probe-card .probe-order.io');
      return el ? el.textContent : '';
    })()`);
    check('探测卡明确说清这轮有没有读盘，并把「扫描行数」和「读盘页数」分开',
      (/没有磁盘读/.test(ioLine) || /从磁盘读了 [\d,]+ 页/.test(ioLine))
      && /扫描 N 行/.test(ioLine),
      ioLine.slice(0, 56));

    /* 连接算子把「规划时估多少」和「实际读到多少」并排摆出来。
       估的是**表行数**（没有列统计信息），所以「估 5 万 / 实际 19」不是算错，
       而是优化器手上只有这么多信息 —— 这正是选错连接顺序的常见原因，得让人看见。 */
    await cdp.eval(`cm.setValue('SELECT count(*) FROM big_demo a JOIN big_demo b ON a.id < b.id WHERE a.id < 20 AND b.id < 40')`);
    await cdp.eval(`document.getElementById('btnProbe').click()`);
    await waitFor(cdp, `[...document.querySelectorAll('.probe-name')].some(n => /Join/.test(n.textContent))`,
      { label: '连接探测', timeout: 20000 });
    const est = await cdp.eval(`(() => {
      const row = [...document.querySelectorAll('.probe-row')].find(r => /Join/.test((r.querySelector('.probe-name') || {}).textContent || ''));
      return { detail: row ? ((row.querySelector('.probe-detail') || {}).textContent || '') : '' };
    })()`);
    check('连接算子并排显示「估算 / 实际」，且两组数字确实不同',
      /估 \d+\/\d+（表行数/.test(est.detail) && /左 \d+ 行 \/ 右 \d+ 行/.test(est.detail)
      // 自连接两侧同表，估算都是 50000；实际只读到 19/39 —— 数字必须不一样，
      // 否则说明 estRows 根本没被填（拿的是同一份默认值）
      && !/估 19\/39/.test(est.detail),
      est.detail.slice(0, 70));

    /* 编辑器里已经写着 EXPLAIN 时，「执行探测」必须升级成 ANALYZE。
       否则发出去的是「只规划、不执行」的普通 EXPLAIN —— 探测卡上每一步的行数和耗时
       都是 0，配上「服务端 197 µs」就成了自相矛盾的画面（真的发生过）。 */
    const up = await cdp.eval(`(() => {
      const a = withExplain('EXPLAIN SELECT 1', 'EXPLAIN ANALYZE ');
      const b = withExplain('EXPLAIN ANALYZE SELECT 2', 'EXPLAIN ANALYZE ');
      const c = withExplain('EXPLAIN SELECT 3', 'EXPLAIN ');
      return { a: a.sql, b: b.sql, c: c.sql };
    })()`);
    check('执行探测把已有的 EXPLAIN（含 ANALYZE）统一升级成 EXPLAIN ANALYZE',
      up.a === 'EXPLAIN ANALYZE SELECT 1;' &&
        up.b === 'EXPLAIN ANALYZE SELECT 2;' &&
        up.c === 'EXPLAIN SELECT 3;',
      JSON.stringify(up));

    // 普通 EXPLAIN 不执行：后端不该给统计树，否则前端会照着画一排 0 ns
    const rawPlan = await cdp.eval(`(async () => {
      const res = await fetch('/api/query', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ sql: 'EXPLAIN SELECT 1' }),
      });
      const d = await res.json();
      const r0 = (d.results || [])[0] || {};
      return { tree: r0.planTree || null, lines: (r0.plan || []).length };
    })()`);
    check('普通 EXPLAIN 只给文本、不给统计树',
      rawPlan.tree === null && rawPlan.lines > 0, JSON.stringify(rawPlan));

    /* 「测不出来」的解释只该在时钟真的粗时出现。
       TSC 生效后步进是 1 ns，再说「整条查询短于本机时钟步进（1 ns）」就是胡话 ——
       不存在短于一纳秒的查询。这里同时盯住 `<1 ns` 这种时间文案。 */
    const granTxt = await cdp.eval(`(() => {
      const c = document.querySelector('.probe-card');
      const txt = c ? c.textContent : '';
      return { gran: clockGranNs,
               empty: !!(c && c.querySelector('.probe-empty')),
               bad: /短于本机时钟步进|<\\s*\\d+\\s*ns/.test(txt) };
    })()`);
    check('时钟够细（TSC 1 ns）时不出现「短于时钟步进 / <1 ns」',
      granTxt.gran >= 1000 || (!granTxt.empty && !granTxt.bad),
      `步进 ${granTxt.gran} ns · 空提示 ${granTxt.empty} · 命中 ${granTxt.bad}`);

    /* 时间去向：服务端 19ms、每一步却 0.5ms 的悬案，靠这条分段对上账。
       这里跑的是热查询（big_demo 前面已经查过），所以只该有「执行」一段 ——
       装载表那段只在服务启动后第一次碰这张表时出现。 */
    const phase = await cdp.eval(`(() => {
      const box = document.querySelector('.probe-phases');
      if (!box) return null;
      return {
        text: box.textContent.replace(/\s+/g, ' '),
        segs: [...box.querySelectorAll('.probe-phase-bar i')].length,
      };
    })()`);
    // 「执行」必须拆成 规划 / 执行 / 整理输出 三段：不拆的话条上写「执行 204 µs」，
    // 而下面的算子各行加起来只有 26 µs —— 那 178 µs 在规划里，却没人认领。
    check('探测卡给出「时间去向」：等锁 / 规划 / 执行 / 整理输出 / 收尾',
      !!phase && /规划/.test(phase.text) && /执行/.test(phase.text) &&
        /整理输出/.test(phase.text) && phase.segs >= 3,
      phase ? phase.text.slice(0, 90) : '没有分段');
    check('表已在内存时不该出现「装载表」那一段',
      !!phase && !/装载表/.test(phase.text),
      phase ? phase.text.slice(0, 50) : '');

    // 每个图例圆点都必须真的显示出来。曾经因为阶段类名叫 `.plan`，撞上 EXPLAIN
    // 文本块的 `.plan { display: none }` —— 圆点整个消失、条子上那一段也一起没了，
    // 而 DOM 里查得到、颜色也对，只有 getBoundingClientRect 是 0。
    const dots = await cdp.eval(`[...document.querySelectorAll('.probe-phase-item')].map(it => {
      const d = it.querySelector('.probe-phase-dot');
      const r = d ? d.getBoundingClientRect() : { width: 0 };
      return { label: it.textContent.trim().slice(0, 4), w: Math.round(r.width) };
    })`);
    check('时间去向每个图例都有可见圆点（类名不能撞 .plan）',
      dots.length >= 3 && dots.every(d => d.w > 0),
      dots.map(d => d.label + ':' + d.w + 'px').join(' '));

    // 规划占大头时给一句解释。用「轻查询」来复现：扫 100 行、执行几十微秒，
    // 规划那一两百微秒就成了大头（聚合那种 ms 级查询反而看不到这句）。
    await cdp.eval(`cm.setValue('SELECT * FROM big_demo LIMIT 100')`);
    await cdp.eval(`document.getElementById('btnProbe').click()`);
    await waitFor(cdp, `document.querySelectorAll('.probe-row').length > 0`,
      { label: '探测算子账本（规划占比）', timeout: 20000 });
    const planHint = await cdp.eval(`(() => {
      const box = document.querySelector('.probe-phases');
      const items = box ? [...box.querySelectorAll('.probe-phase-item')].map(x => x.textContent) : [];
      const h = [...document.querySelectorAll('.probe-phase-hint')].map(x => x.textContent).join(' ');
      return { items, has: /规划/.test(h), txt: h.slice(0, 90) };
    })()`);
    // 那句解释的规则是：规划占执行阶段（规划+执行+整理）≥40% 且 ≥20µs 才出现。
    // 这条断言量「出现与否和占比是否一致」——规划修好之后（README「规划为什么贵」）
    // 轻查询的规划只有十几 µs，占比不到 40%，所以现在这里是「不该出现」。
    // 哪天规划又慢回去，它照样能抓住「占了大头却没解释」的情况。
    const share = await cdp.eval(`(() => {
      const num = label => {
        const it = [...document.querySelectorAll('.probe-phase-item')]
          .find(x => x.textContent.trim().startsWith(label));
        if (!it) return 0;
        const m = it.textContent.match(/([\\d.]+)\\s*(ns|µs|ms|s)/);
        if (!m) return 0;
        const k = { ns: 1, 'µs': 1e3, ms: 1e6, s: 1e9 }[m[2]] || 1;
        return parseFloat(m[1]) * k;
      };
      const plan = num('规划'), run = num('执行'), tail = num('整理输出');
      const all = plan + run + tail;
      return { plan: Math.round(plan), all: Math.round(all), ratio: all > 0 ? plan / all : 0 };
    })()`);
    const hintOK = share.ratio >= 0.45 ? planHint.has
      : (share.ratio <= 0.35 ? !planHint.has : true);
    check('「规划占大头」的解释与占比一致（≥40% 才出现）',
      hintOK,
      `规划 ${share.plan} / 执行阶段 ${share.all}（${Math.round(share.ratio * 100)}%）· 提示 ${planHint.has}`);

    /* 探测说明默认收起：那几句每次执行都一样，常驻会把卡片撑高三行。
       点右上角的「?」才展开，再点收起。 */
    const helpOff = await cdp.eval(`(() => {
      const box = document.querySelector('.probe-card .probe-note');
      const btn = document.querySelector('.probe-card .probe-help');
      return { has: !!box && !!btn, visible: box ? !box.classList.contains('hidden') : null };
    })()`);
    check('探测说明默认收起，不占结果区空间',
      helpOff.has && helpOff.visible === false, JSON.stringify(helpOff));
    await cdp.eval(`document.querySelector('.probe-card .probe-help').click()`);
    await sleep(150);
    const helpOn = await cdp.eval(`(() => {
      const box = document.querySelector('.probe-card .probe-note');
      return { visible: !box.classList.contains('hidden'), text: box.textContent.slice(0, 40) };
    })()`);
    check('点「?」展开说明，里面有「真的跑了一遍」',
      helpOn.visible === true && helpOn.text.indexOf('真的跑了一遍') >= 0,
      helpOn.text);
    await cdp.eval(`document.querySelector('.probe-card .probe-help').click()`);
    await sleep(150);
    check('再点一次收起',
      await cdp.eval(`document.querySelector('.probe-card .probe-note').classList.contains('hidden')`));

    // 原始文本仍在（有人就是习惯看树形文本）：点一下能展开
    await cdp.eval(`[...document.querySelectorAll('.probe-card .plan-btn')][0].click()`);
    await sleep(200);
    const rawShown = await cdp.eval(`(() => {
      const b = document.querySelector('.probe-raw');
      return { shown: b ? b.classList.contains('show') : null, text: b ? b.textContent.slice(0, 30) : '' };
    })()`);
    check('探测卡能展开原始 EXPLAIN 文本',
      rawShown.shown === true && /Limit|Aggregate|Scan/.test(rawShown.text),
      rawShown.text.replace(/\s+/g, ' '));

    // 普通「执行」不该带着探测视图（否则一个 SELECT 也会被当成探测结果显示）
    await cdp.eval(`cm.setValue('SELECT 7 AS x')`);
    await cdp.eval(`document.getElementById('btnRun').click()`);
    await waitIdle(cdp);
    const noProbe = await cdp.eval(`({
      rows: document.querySelectorAll('.probe-row').length,
      tab: [...document.querySelectorAll('.result-tab')].map(b => b.textContent).join(','),
    })`);
    check('普通执行不会串到执行探测视图',
      noProbe.rows === 0 && !noProbe.tab.includes('探测结果'), `tab=${noProbe.tab}`);

    const bad = results.filter(r => !r.ok);
    console.log(`\n${results.length - bad.length}/${results.length} 通过`);
    if (bad.length) process.exitCode = 1;
  } finally {
    chrome.kill();
  }
})().catch(e => { console.error('脚本出错:', e.message); process.exit(1); });
