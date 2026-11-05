// 临时：看模型面板、模型列表、版本弹窗长什么样。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');

const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const PORT = 9488;
const URL = 'http://127.0.0.1:8080/?theme=sky';
const OUT = 'C:/Users/Administrator/WorkBuddy/2026-10-26-14-50-18/godb/';
const sleep = ms => new Promise(r => setTimeout(r, ms));
const get = p => new Promise((res, rej) => {
  http.get({ host: '127.0.0.1', port: PORT, path: p }, r => {
    let b = ''; r.on('data', d => (b += d)); r.on('end', () => res(JSON.parse(b)));
  }).on('error', rej);
});

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
    return new Promise(res => { this.wait.set(id, res); this.ws.send(JSON.stringify({ id, method, params })); });
  }
  async eval(expr) {
    const r = await this.send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
    if (r.result && r.result.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails).slice(0, 400));
    return r.result.result.value;
  }
  async shot(name, clip) {
    const r = await this.send('Page.captureScreenshot', clip
      ? { format: 'png', clip: { ...clip, scale: 1 } }
      : { format: 'png' });
    fs.writeFileSync(OUT + name, Buffer.from(r.result.data, 'base64'));
  }
}

(async () => {
  const prof = 'C:/Users/Administrator/AppData/Local/Temp/godb-shot2';
  fs.rmSync(prof, { recursive: true, force: true });
  const chrome = spawn(CHROME, ['--headless=new', '--disable-gpu', '--hide-scrollbars',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${prof}`, '--window-size=1400,900', 'about:blank'], { stdio: 'ignore' });
  try {
    for (let i = 0; i < 40; i++) { try { await get('/json/version'); break; } catch (e) { await sleep(250); } }
    const targets = await get('/json/list');
    const cdp = await CDP.open(targets.find(t => t.type === 'page').webSocketDebuggerUrl);
    await cdp.send('Page.enable');
    await cdp.send('Page.navigate', { url: URL });
    for (let i = 0; i < 100; i++) {
      if (await cdp.eval(`typeof cm !== 'undefined' && !!document.querySelector('.CodeMirror')`)) break;
      await sleep(200);
    }
    await cdp.eval(`document.getElementById('btnEr').click()`);
    await sleep(1200);
    console.log('卡片', await cdp.eval(`document.querySelectorAll('.er-card').length`),
      '模型名', await cdp.eval(`(document.getElementById('erModelName')||{}).textContent`));
    await cdp.shot('tmp_panel.png');

    await cdp.eval(`document.getElementById('btnErModels').click()`);
    await sleep(600);
    await cdp.shot('tmp_models.png');
    await cdp.eval(`document.getElementById('erModelsClose').click()`);

    await cdp.eval(`document.getElementById('btnErRev').click()`);
    await sleep(500);
    await cdp.shot('tmp_revs.png');
    console.log('异常', await cdp.eval(`'ok'`));
  } finally {
    chrome.kill();
  }
})().catch(e => { console.error('ERR', e && e.message); process.exit(1); });
