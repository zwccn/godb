// 只读截图：空白处右键菜单 + 两个二级菜单。
const http = require('http');
const fs = require('fs');
const { spawn } = require('child_process');

const PORT = 9234;
const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const prof = 'C:/Users/Administrator/AppData/Local/Temp/godb-cdp-ctx';
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
    if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 400));
    return r.result.value;
  };

  await send('Page.navigate', { url: 'http://127.0.0.1:8080/' });
  await sleep(1600);
  await js(`document.getElementById('btnEr').click()`);
  await sleep(1200);

  // 空白处右键（真实右键事件：pressed+released 会触发 contextmenu）
  // 找一个真正空白的位置（别落在组框 / 卡片上，那样弹的是它们自己的菜单）
  const spot = await js(`(() => {
    const r = document.getElementById('erStage').getBoundingClientRect();
    for (let y = r.top + 60; y < r.bottom - 60; y += 20) {
      for (let x = r.left + 40; x < r.right - 40; x += 20) {
        const el = document.elementFromPoint(x, y);
        if (el && !el.closest('.er-group, .er-card, .er-menu, .er-legend')) {
          return { x: Math.round(x), y: Math.round(y) };
        }
      }
    }
    return { x: Math.round(r.left + 60), y: Math.round(r.bottom - 60) };
  })()`);
  await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: spot.x, y: spot.y, button: 'right', buttons: 2, clickCount: 1 });
  await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: spot.x, y: spot.y, button: 'right', buttons: 0, clickCount: 1 });
  await sleep(400);
  console.log('menu', await js(`(() => {
    const m = document.querySelector('.er-menu');
    if (!m) return null;
    return { items: [...m.querySelectorAll(':scope > .er-menu-item, :scope > .er-menu-sub > .er-menu-item')].map(b => b.textContent),
             flip: m.classList.contains('flip') };
  })()`));

  const clip = await js(`(() => {
    const r = document.getElementById('erStage').getBoundingClientRect();
    return { x: Math.round(r.left), y: Math.round(r.top + 150), width: 560, height: 420 };
  })()`);
  let shot = await send('Page.captureScreenshot', { format: 'png', clip: { ...clip, scale: 2 } });
  fs.writeFileSync('tmp_ctx.png', Buffer.from(shot.data, 'base64'));

  // 悬停第一个二级项（显示 / 隐藏）
  const sub1 = await js(`(() => {
    const b = [...document.querySelectorAll('.er-menu > .er-menu-sub > .er-menu-item')][0];
    if (!b) return null;
    const r = b.getBoundingClientRect();
    return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
  })()`);
  if (sub1) {
    await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: sub1.x, y: sub1.y });
    await sleep(400);
    const clip2 = await js(`(() => {
      const m = document.querySelector('.er-menu');
      const r = m.getBoundingClientRect();
      return { x: Math.round(r.left - 8), y: Math.round(r.top - 8),
               width: Math.round(r.width + 380), height: Math.round(r.height + 90) };
    })()`);
    shot = await send('Page.captureScreenshot', { format: 'png', clip: { ...clip2, scale: 2 } });
    fs.writeFileSync('tmp_ctx_show.png', Buffer.from(shot.data, 'base64'));
  }

  // 悬停第二个二级项（新建 / 导入）
  const sub2 = await js(`(() => {
    const b = [...document.querySelectorAll('.er-menu > .er-menu-sub > .er-menu-item')][1];
    if (!b) return null;
    const r = b.getBoundingClientRect();
    return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
  })()`);
  if (sub2) {
    await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: sub2.x, y: sub2.y });
    await sleep(400);
    const clip3 = await js(`(() => {
      const m = document.querySelector('.er-menu');
      const r = m.getBoundingClientRect();
      return { x: Math.round(r.left - 8), y: Math.round(r.top - 8),
               width: Math.round(r.width + 380), height: Math.round(r.height + 90) };
    })()`);
    shot = await send('Page.captureScreenshot', { format: 'png', clip: { ...clip3, scale: 2 } });
    fs.writeFileSync('tmp_ctx_new.png', Buffer.from(shot.data, 'base64'));
  }

  await send('Page.reload');
  await sleep(300);
  ws.close();
  chrome.kill();
})();
