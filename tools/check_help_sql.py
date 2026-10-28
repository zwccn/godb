"""逐条实跑帮助里的示例 SQL（在临时库上，绝不动你的示例数据）。

    python tools/check_help_sql.py

检查对象：
1. 帮助手册里的 10 段代码（web/assets/index.html 的 <pre class="help-code">）
2. 「上手示例」那 7 张卡片（web/assets/app.js 的 EXAMPLES）

做法：建临时库 zz_demo → 载入同样的示例数据 → 每条跑两轮（第二轮用来抓
「不可重复执行」，比如没先 DROP 的 CREATE TABLE）→ 删库。

判定：报错、或一条 SELECT 返回 0 行，都算失败 —— 示例查出空结果，读起来像功能坏了。
事务演示那种故意留未提交事务的，每条跑完补一个 ROLLBACK 再继续。
"""
import html
import http.cookiejar
import io
import json
import re
import sys
import urllib.error
import urllib.request

BASE = 'http://127.0.0.1:8080'
DB = 'zz_demo'

# 必须带上会话 cookie（godb_sid）：否则每个请求都是**新会话**，一条 BEGIN 会留在
# 无人认领的会话里，一直持有表锁 —— 上一次就被这个坑挂住过（载入示例数据时 3s 锁超时）。
JAR = http.cookiejar.CookieJar()
OPENER = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(JAR))


def use_db(name):
    """把「当前库」写进 cookie jar（真浏览器里是切库时服务端 Set-Cookie 下发的）。"""
    JAR.set_cookie(http.cookiejar.Cookie(
        0, 'godb_db', name, None, False, '127.0.0.1', False, False, '/', True,
        False, None, True, None, None, {}))


def get(path):
    try:
        with OPENER.open(urllib.request.Request(BASE + path)) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return {'error': {'message': 'HTTP %d' % e.code}}


def post(path, body, cookie=None):
    if cookie:
        use_db(cookie)
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode(),
                                 headers={'Content-Type': 'application/json'})
    try:
        with OPENER.open(req) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return {'error': {'message': 'HTTP %d %s' % (e.code, e.read()[:150])}}


def run(sql, cookie):
    """执行一段示例，返回 (错误信息|None, 结果摘要, 事务是否还开着)。"""
    d = post('/api/query', {'sql': sql}, cookie)
    if d.get('error'):
        # 出错也可能是「事务里某条失败」，先回滚，别让会话挂着锁
        post('/api/query', {'sql': 'ROLLBACK'}, cookie)
        return d['error']['message'][:100], '', False, ''
    tags, empty_select = [], []
    for i, r in enumerate(d['results']):
        n = len(r.get('rows') or [])
        if r.get('tag', '').startswith('SELECT'):
            tags.append('SELECT(%d行)' % n)
            if n == 0:
                empty_select.append(i)
        else:
            tags.append(r.get('tag', '?'))
    msg = ''
    if empty_select:
        msg = '第 %s 条 SELECT 返回 0 行' % ','.join(str(i + 1) for i in empty_select)
    return None, ' / '.join(tags), (d.get('tx') or {}).get('active', False), msg


def examples():
    out = []
    js = io.open('web/assets/app.js', encoding='utf-8').read()
    body = js[js.index('const EXAMPLES = {'):]
    body = body[:body.index('\n};')]
    for k, sql in re.findall(r'\n  (\w+): `([\s\S]*?)`,', body):
        out.append(('卡片·' + k, sql.strip()))

    src = io.open('web/assets/index.html', encoding='utf-8').read()
    blocks = re.findall(r'<pre class="help-code">(.*?)</pre>', src, re.S)
    for i, b in enumerate(blocks, 1):
        out.append(('手册·%d' % i, html.unescape(b).strip()))
    return out


def main():
    post('/api/dbs', {'action': 'create', 'name': DB})
    sample = post('/api/sample', {}, DB)
    if sample.get('error'):
        print('载入示例数据失败：', sample['error']['message'])
        return 2

    ex = examples()
    print('共 %d 段示例（%d 张卡片 + %d 段手册）\n'
          % (len(ex), sum(1 for n, _ in ex if n.startswith('卡片')), sum(1 for n, _ in ex if n.startswith('手册'))))

    fails = []
    for rnd in (1, 2):
        print('=== 第 %d 轮 ===' % rnd)
        for name, sql in ex:
            err, tags, active, empty = run(sql, DB)
            if active:  # 事务演示会故意留一个未提交事务，别污染下一条
                run('ROLLBACK', DB)
            bad = err or empty
            print('%-10s %-56s %s' % (name, tags[:56], ('ERR ' + err) if err else ('空结果：' + empty if empty else 'OK')))
            if bad and rnd == 2:
                fails.append((name, err or empty))

    # 删库前必须把「当前库」切回默认库：cookie 里还留着 godb_db=zz_demo，
    # 服务端会以「不能删除当前正在使用的库」拒绝（上一版就漏在这）
    use_db(get('/api/dbs').get('default', 'godb'))
    drop = post('/api/dbs', {'action': 'drop', 'name': DB})
    if drop.get('error'):
        print('清理临时库失败：' + drop['error']['message'])
    print()
    if fails:
        print('未通过 %d 段：' % len(fails))
        for n, m in fails:
            print('  %-10s %s' % (n, m))
        return 1
    print('全部通过（两轮都 OK，且每条 SELECT 都有行）')
    return 0


if __name__ == '__main__':
    sys.exit(main())
