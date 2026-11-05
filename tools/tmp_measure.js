// 只读：量「离圆点多远还能点中」——沿行方向探几个偏移，看命中的是不是圆点。
const http = require('http');
const fs = require('fs');
const { spawn } = require('child_process');

const PORT = 9227;
const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const prof = 'C:/Users/Administrator/AppData/Local/Temp/godb-cdp-measure';
fs.rmSync(prof, { recursive: true, force: true });
const chrome = spawn(CHROME, [
  '--headless=new', '--disable-gpu', '--hide-scrollbars',
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${prof}`,
  '--window-size=1400,900', 'about:blank',
], { stdio: 'ignore' });
const sleep = ms => new Promise(r => setTimeout(r, ms));
const targets = () => new Promise((res, rej) => {
  http.get({ host: '127.0.0.1', port: PORT, path: '/json/list' }, r => {
    let b = '';
    r.on('data', d => (b += d));
    r.on('end', () => res(JSON.parse(b).filter(t => t.type === 'page')));
  }).on('error', rej);
});

(async () => {
  for (let i = 0; i < 40; i++) {
    try { await targets(); break; } catch (e) { await sleep(250); }
  }
  const page = (await targets())[0];
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let id = 0;
  const pend = new Map();
  const send = (m, p = {}) => new Promise((res) => {
    const mid = ++id;
    pend.set(mid, res);
    ws.send(JSON.stringify({ id: mid, method: m, params: p }));
  });
  await new Promise(r => ws.addEventListener('open', r));
  ws.addEventListener('message', ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pend.has(m.id)) { pend.get(m.id)(m.result); pend.delete(m.id); }
  });
  const js = async expr => {
    const r = await send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 300));
    return r.result.value;
  };

  await send('Page.navigate', { url: 'http://127.0.0.1:8080/' });
  for (let i = 0; i < 60; i++) {
    if (await js(`document.querySelectorAll('.er-card').length`).catch(() => 0)) break;
    await sleep(250);
  }
  await js(`erOpen || document.getElementById('btnEr').click()`);
  await sleep(800);

  console.log(await js(`(() => {
    const card = document.querySelector('.er-card');
    const dots = [...card.querySelectorAll('.er-col-dot')];
    const d = dots[1] || dots[0];
    const r = d.getBoundingClientRect();
    const cx = r.left + r.width / 2, cy = r.top + r.height / 2;
    const hit = (x, y) => {
      const el = document.elementFromPoint(x, y);
      return !!(el && el.closest && el.closest('.er-col-dot'));
    };
    const scan = [];
    for (let dx = 0; dx >= -30; dx -= 2) {
      if (!hit(cx + dx, cy)) { scan.push('内侧断在 ' + (-dx) + 'px'); break; }
    }
    for (let dy = 0; dy <= 14; dy += 2) {
      if (!hit(cx, cy + dy)) { scan.push('下方断在 ' + dy + 'px'); break; }
    }
    for (let dy = 0; dy <= 14; dy -= 2) {
      if (!hit(cx, cy + dy)) { scan.push('上方断在 ' + (-dy) + 'px'); break; }
    }
    // 点在这行圆点上，取到的是不是这一行的圆点（别窜到隔壁行）
    const rows = [...card.querySelectorAll('.er-col')];
    let wrongRow = 0;
    for (const row of rows) {
      const dot = row.querySelector('.er-col-dot');
      if (!dot) continue;
      const b = dot.getBoundingClientRect();
      const el = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
      const got = el && el.closest('.er-col-dot');
      if (got !== dot) wrongRow++;
    }
    return { scan, rows: rows.length, wrongRow, zoom: erView.zoom,
             dot: { w: Math.round(r.width * 10) / 10, h: Math.round(r.height * 10) / 10 } };
  })()`));

  await send('Page.reload');
  await sleep(300);
  ws.close();
  chrome.kill();
})();
