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
    await sleep(2500);

    const results = [];
    const check = (name, ok, detail) => {
      results.push({ name, ok, detail });
      console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  ' + detail : ''}`);
    };

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
    await sleep(1200);
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

    // 执行中：图标让位给转圈，结束后再换回来（按钮结构不能被整体重写）
    await cdp.eval(`setRunning(true)`);
    const running = await cdp.eval(`(() => {
      const b = document.getElementById('btnRun');
      const ico = b.querySelector('.run-ico svg');
      return { label: document.getElementById('btnRunText').textContent,
               spin: !!b.querySelector('.run-spin') &&
                     b.querySelector('.run-spin').getBoundingClientRect().width > 0,
               icoW: ico ? Math.round(ico.getBoundingClientRect().width) : 0,
               disabled: b.disabled };
    })()`);
    await cdp.eval(`setRunning(false)`);
    const back = await cdp.eval(`document.getElementById('btnRunText').textContent`);
    check('执行中：文字变「执行中…」、图标换成转圈',
      running.label === '执行中…' && running.spin && running.icoW === 0 &&
        running.disabled && back === '执行',
      `${running.label} / 图标宽 ${running.icoW} → ${back}`);

    // 0.1b) 美化 SQL：子句换行 + 关键字大写，字符串里的内容一个字都不能动
    await cdp.eval(`cm.setValue("select id,name from t where a='it''s' and b=1 order by id")`);
    await cdp.eval(`document.getElementById('btnFormat').click()`);
    const fmted = await cdp.eval(`cm.getValue()`);
    check('美化：子句换行 + 关键字大写 + 字符串原样',
      /^SELECT id,/.test(fmted) && /\nFROM t\n/.test(fmted) &&
        /'it''s'/.test(fmted) && /ORDER BY id/.test(fmted),
      JSON.stringify(fmted).slice(0, 140));

    // 括号没配对就一个字都不改（宁可不动，也不要吐出半截 SQL 让人再改回去）
    await cdp.eval(`cm.setValue("select * from t where (a = 1")`);
    await cdp.eval(`document.getElementById('btnFormat').click()`);
    const broken = await cdp.eval(`cm.getValue()`);
    check('美化：括号不配对时保持原样', broken === 'select * from t where (a = 1', JSON.stringify(broken));

    const fmtKey = await cdp.eval(`!!((cm.getOption('extraKeys') || {})['Shift-Ctrl-F'])`);
    check('美化：Ctrl+Shift+F 已绑定', fmtKey === true, String(fmtKey));

    // 0.1c) 结果区：一条 tab 栏（信息 / 结果 N）+ 同一时刻只有一条滚动条。
    //      以前 .results 自己滚、卡片里的 .grid-wrap 又按 52vh 滚，必然出现两条。
    const runSqlX = async (sql, wait = 1100) => {
      await cdp.eval(`cm.setValue(${JSON.stringify(sql)}); document.getElementById('btnRun').click()`);
      await sleep(wait);
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
    await sleep(300);
    const tInfo = await tabState();
    check('「信息」tab = 往返时间 + 逐条语句明细',
      tInfo.pane.indexOf('info-body') === 0 && /浏览器往返/.test(tInfo.text) && /SELECT 1 AS a/.test(tInfo.text),
      tInfo.text.slice(0, 90));

    await runSqlX('SELECT * FROM no_such_table');
    const tErr = await tabState();
    check('报错时默认落到「信息」，且只有信息这一个 tab',
      tErr.labels.join('|') === '信息' && tErr.active === '信息' &&
        /does not exist|不存在/.test(tErr.text), tErr.text.slice(0, 70));

    // 0.2)「执行计划」按钮：只解释查询，别把 BEGIN/UPDATE 也套上 EXPLAIN
    //      （以前是 split(';') 全量加前缀，事务演示一按就报 only supports SELECT）
    const planText = () => cdp.eval(`(document.getElementById('results').textContent || '').replace(/\\s+/g, ' ')`);
    await cdp.eval(`cm.setValue("BEGIN; UPDATE employees SET salary = salary; SELECT * FROM employees LIMIT 1;")`);
    await cdp.eval(`document.getElementById('btnExplain').click()`);
    await sleep(1200);
    const mixed = await planText();
    check('执行计划：多语句里只解释 SELECT（不再报错）',
      /Scan|Project/.test(mixed) && !/only supports/i.test(mixed), mixed.slice(0, 120));

    await cdp.eval(`cm.setValue("EXPLAIN SELECT 1;")`);
    await cdp.eval(`document.getElementById('btnExplain').click()`);
    await sleep(900);
    const doubleEx = await planText();
    check('执行计划：已经是 EXPLAIN 的语句不再套一层',
      /Result|Project/.test(doubleEx) && !/only supports/i.test(doubleEx), doubleEx.slice(0, 120));

    // 字符串里的分号不能被当成分隔符
    await cdp.eval(`cm.setValue("SELECT 'a;b' AS v;")`);
    await cdp.eval(`document.getElementById('btnExplain').click()`);
    await sleep(900);
    const semi = await planText();
    check('执行计划：字符串里的分号不会被切开',
      /Result|Project/.test(semi) && !/only supports|执行失败/i.test(semi), semi.slice(0, 120));

    // 一条能解释的都没有：直接说清楚，别发个请求换回看不懂的报错
    await cdp.eval(`cm.setValue("UPDATE employees SET salary = salary WHERE id = 1;")`);
    await cdp.eval(`document.getElementById('btnExplain').click()`);
    await sleep(700);
    const onlyWrite = await planText();
    check('执行计划：全是非查询语句时给出明确提示',
      /只能解释查询语句/.test(onlyWrite) && /UPDATE/.test(onlyWrite), onlyWrite.slice(0, 100));

    // 0.3) 新能力：Top-N、interval、精确 numeric、快聚合、覆盖索引
    const runSql = async (sql, wait = 900) => {
      await cdp.eval(`cm.setValue(${JSON.stringify(sql)})`);
      await cdp.eval(`document.getElementById('btnRun').click()`);
      await sleep(wait);
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
    await sleep(700);
    await cdp.eval(`document.getElementById('btnMonitor').click()`);
    await sleep(1200);
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
    const monFit = await cdp.eval(`(() => {
      const b = document.querySelector('.mon-body');
      const cards = [...document.querySelectorAll('#monPanel .mon-card')];
      b.scrollTop = 999999;
      const last = cards[cards.length - 1].getBoundingClientRect();
      return {
        n: cards.length,
        crushed: cards.filter(c => c.getBoundingClientRect().height < 20).length,
        scrollTop: Math.round(b.scrollTop),
        max: Math.round(b.scrollHeight - b.clientHeight),
        lastBottom: Math.round(last.bottom),
        panelBottom: Math.round(document.getElementById('monPanel').getBoundingClientRect().bottom),
      };
    })()`);
    check('165% 字号：监控面板能滚到底、卡片不被压扁',
      monFit.crushed === 0 && monFit.scrollTop >= monFit.max - 2 &&
        monFit.lastBottom <= monFit.panelBottom + 1,
      `卡片 ${monFit.n} 张（压扁 ${monFit.crushed}）· 滚到 ${monFit.scrollTop}/${monFit.max}`);
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

    // 自动模式下粘一大段 SQL：应该长高（封顶 55vh），而不是原地出滚动条
    const grow = await cdp.eval(`(() => {
      cm.setValue(Array.from({length: 40}, (_, i) => 'SELECT ' + i + ';').join('\\n'));
      const h = document.querySelector('.CodeMirror').getBoundingClientRect().height;
      return { h: Math.round(h), cap: Math.round(window.innerHeight * 0.55) + 2 };
    })()`);
    check('长 SQL 会自动长高（不超过 55vh）',
      grow.h > 300 && grow.h <= grow.cap, `${grow.h}px（上限 ${grow.cap}px）`);
    await cdp.eval(`cm.setValue('')`);

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
      };
    })()`);
    check('帮助里有 8 个示例、每个都带一句说明',
      helpUi.ex === 8 && helpUi.allHaveNote, `${helpUi.ex} 个，第一个是「${helpUi.first}」`);
    // 手册由浅入深：第 1 级是增删改查，最后一级必须是「明确不支持」
    check('语法手册按由浅入深分了 10 级 + 不支持',
      helpUi.levels.length === 11 &&
        helpUi.levels[0].indexOf('建表') >= 0 &&
        helpUi.levels[10] === '明确不支持',
      helpUi.levels.join(' / '));
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
    await sleep(2600);
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
    check('设置：打开后可见', await cdp.eval(`!document.getElementById('settingsModal').classList.contains('hidden')`));

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

    // 固定高度：五组内容差得多（外观一百多、性能五百多），跟着内容走的话切 Tab 窗口忽大忽小，
    // 指针底下的按钮跟着跑。打开时按最高的一组钉死，矮组下方留白。
    const setH = await cdp.eval(`(() => {
      const card = document.querySelector('#settingsModal .modal-card');
      const h = [];
      for (const t of document.querySelectorAll('.set-tab')) {
        t.click();
        h.push(Math.round(card.getBoundingClientRect().height));
      }
      return { h, inline: card.style.height };
    })()`);
    check('切五个 Tab 弹窗高度纹丝不动',
      new Set(setH.h).size === 1 && setH.h[0] > 400 && !!setH.inline,
      setH.h.join(' / ') + ' · inline ' + setH.inline);
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

    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: 8, y: 8, button: 'left', clickCount: 1 });
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: 8, y: 8, button: 'left', clickCount: 1 });
    await sleep(200);
    check('设置：点遮罩后仍然打开', await cdp.eval(`!document.getElementById('settingsModal').classList.contains('hidden')`));
    await cdp.eval(`document.getElementById('btnSettingsClose').click()`);
    await sleep(200);
    check('设置：点 × 能关掉', await cdp.eval(`document.getElementById('settingsModal').classList.contains('hidden')`));

    // 存成功就把弹窗收掉，结果交给 toast —— 用户改完就想接着干活，不该再点一次 ×
    await cdp.eval(`openSettings()`);
    await sleep(800);
    // 外观组在无改动时按钮是「关闭」，保存用例必须先切回可保存的组
    await cdp.eval(`[...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'conn').click()`);
    await cdp.eval(`document.getElementById('btnSettingsSave').click()`);
    await sleep(1300);
    const saved = await cdp.eval(`(() => {
      const t = document.getElementById('toast');
      return {
        closed: document.getElementById('settingsModal').classList.contains('hidden'),
        toast: (t.textContent || ''),
        shown: !t.classList.contains('hidden'),
      };
    })()`);
    check('设置：「保存并应用」成功后自动关闭 + 提示',
      saved.closed && saved.shown && /已保存/.test(saved.toast), JSON.stringify(saved));

    // 缓冲池页数：行能换出之后，这个值决定「每张表留多少热页的行在内存里」。
    // 改完要重启才生效，但值本身必须存得住（不然用户以为没保存）。
    await cdp.eval(`openSettings()`);
    await sleep(700);
    const pool0 = await cdp.eval(`(() => { const n = document.getElementById('setPoolPages'); return n ? n.value : null; })()`);
    check('设置页有「缓冲池页数」字段', pool0 !== null && pool0 !== '', String(pool0));
    await cdp.eval(`[...document.querySelectorAll('.set-tab')].find(t => t.dataset.pane === 'perf').click()`);
    await cdp.eval(`document.getElementById('setPoolPages').value = '512'`);
    await cdp.eval(`document.getElementById('setPoolPages').dispatchEvent(new Event('input', { bubbles: true }))`);
    await cdp.eval(`document.getElementById('btnSettingsSave').click()`);
    await sleep(1200);
    await cdp.eval(`openSettings()`);
    await sleep(700);
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
      document.getElementById('btnHistory').click();
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

    const bad = results.filter(r => !r.ok);
    console.log(`\n${results.length - bad.length}/${results.length} 通过`);
    if (bad.length) process.exitCode = 1;
  } finally {
    chrome.kill();
  }
})().catch(e => { console.error('脚本出错:', e.message); process.exit(1); });
