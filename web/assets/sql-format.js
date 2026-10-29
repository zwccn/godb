'use strict';

// ---------------- SQL 美化 ----------------
//
// 自己实现，不引第三方 formatter（sql-formatter 之类要额外 vendor 一份 100KB+，
// 而这里的规则只要「子句换行 + 关键字大写 + 列表对齐」三件事）。
//
// 三条硬原则：
//   1. **语义零变化**。字符串、带引号标识符、`$$` 美元引用、注释一律原样搬运，
//      不参与任何改写；关键字大写对裸标识符是安全的（PG 里裸标识符本来就折叠
//      成小写，`SELECT date FROM t` 和 `SELECT DATE FROM t` 等价）。
//   2. **失败就不动**。括号不配对、字符串没闭合这类情况直接返回 null，调用方
//      保持原文 + 提示，绝不吐出半截 SQL 让人再改一遍。
//   3. **不猜宽度**。只在结构点换行（子句 / JOIN / AND / OR / 顶层逗号 / 长括号组），
//      不做按字数回卷 —— 回卷会让同一段 SQL 每次美化结果不稳定。

(function () {
  // 只用于「这个词要不要大写」。多余命中不会改坏语句（见原则 1）。
  const KEYWORDS = new Set(`
    SELECT FROM WHERE GROUP BY ORDER HAVING LIMIT OFFSET FETCH FIRST NEXT ROW ROWS ONLY
    UNION ALL DISTINCT INTERSECT EXCEPT
    INSERT INTO VALUES UPDATE SET DELETE RETURNING USING ON CONFLICT DO NOTHING
    JOIN LEFT RIGHT INNER OUTER FULL CROSS NATURAL
    AS AND OR NOT NULL IS IN EXISTS BETWEEN LIKE ILIKE SIMILAR TO ESCAPE
    CASE WHEN THEN ELSE END
    FOR SHARE NOWAIT SKIP LOCKED FILTER
    WITH RECURSIVE
    CREATE TABLE TEMP TEMPORARY IF INDEX UNIQUE VIEW SEQUENCE MATERIALIZED
    DROP ALTER TRUNCATE ADD COLUMN CONSTRAINT PRIMARY KEY FOREIGN REFERENCES
    DEFAULT CHECK CASCADE RESTRICT NO ACTION
    EXPLAIN ANALYZE VERBOSE
    BEGIN COMMIT ROLLBACK SAVEPOINT RELEASE TRANSACTION WORK
    COPY STDIN STDOUT
    ASC DESC NULLS LAST
    CAST EXTRACT OVERLAY POSITION SUBSTRING TRIM COALESCE NULLIF GREATEST LEAST
    ARRAY ROW ANY SOME
    COUNT SUM AVG MIN MAX
    TRUE FALSE
    INT INTEGER BIGINT SMALLINT SERIAL BIGSERIAL TEXT VARCHAR CHAR BOOLEAN BOOL
    NUMERIC DECIMAL REAL DOUBLE PRECISION DATE TIME TIMESTAMP TIMESTAMPTZ INTERVAL
    UUID JSON JSONB BYTEA
    LEFT_FROM_BY
  `.split(/\s+/).filter(Boolean));

  // 子句起点：换行到「本条语句当前的基准缩进」
  const CLAUSE = new Set([
    'SELECT', 'FROM', 'WHERE', 'GROUP BY', 'ORDER BY', 'HAVING', 'LIMIT', 'OFFSET',
    'RETURNING', 'VALUES', 'SET', 'UNION', 'UNION ALL', 'INTERSECT', 'EXCEPT', 'WITH',
    'INSERT', 'UPDATE', 'DELETE', 'CREATE', 'DROP', 'ALTER', 'TRUNCATE',
    'EXPLAIN', 'ANALYZE', 'COPY', 'BEGIN', 'COMMIT', 'ROLLBACK', 'SAVEPOINT',
    'RELEASE', 'ON CONFLICT', 'WINDOW', 'FETCH',
  ]);

  // 这些只在一句话开头换行：ALTER TABLE … DROP COLUMN 里的 DROP、INSERT 里的
  // 子句不该被当成新语句起头，否则一条 DDL 会被拆得七零八落
  const STMT_START = new Set([
    'INSERT', 'UPDATE', 'DELETE', 'CREATE', 'DROP', 'ALTER', 'TRUNCATE',
    'EXPLAIN', 'ANALYZE', 'COPY', 'BEGIN', 'COMMIT', 'ROLLBACK', 'SAVEPOINT',
    'RELEASE', 'WITH',
  ]);

  const JOIN_KW = new Set([
    'JOIN', 'LEFT JOIN', 'RIGHT JOIN', 'FULL JOIN', 'INNER JOIN', 'CROSS JOIN',
    'NATURAL JOIN', 'LEFT OUTER JOIN', 'RIGHT OUTER JOIN', 'FULL OUTER JOIN',
  ]);

  // 这些子句里的逗号是「列表」，一个元素一行、对齐到第一个元素
  const LIST_CLAUSE = new Set([
    'SELECT', 'FROM', 'GROUP BY', 'ORDER BY', 'RETURNING', 'SET', 'UNION', 'VALUES',
  ]);

  // 条件型子句：里面的 AND / OR 换行并缩进
  const COND_CLAUSE = new Set(['WHERE', 'HAVING', 'ON', 'ON CONFLICT']);

  // 这些关键字后面紧跟 `(` 时不留空格（当成函数调用看）
  const FUNC_LIKE = new Set([
    'CAST', 'EXTRACT', 'OVERLAY', 'POSITION', 'SUBSTRING', 'TRIM', 'COALESCE',
    'NULLIF', 'GREATEST', 'LEAST', 'ARRAY', 'ROW', 'COUNT', 'SUM', 'AVG', 'MIN', 'MAX',
    // 带长度/精度的类型名：NUMERIC(12,2)、VARCHAR(200) 不该有空格
    'NUMERIC', 'DECIMAL', 'VARCHAR', 'CHAR', 'TIMESTAMP', 'TIME', 'INTERVAL',
  ]);

  // 这些关键字后面跟 `(` 时其实是「表名 + 列名列表」，留一个空格更像 SQL
  const SPACE_PAREN_PREV = new Set(['INTO', 'TABLE', 'UPDATE', 'FROM', 'JOIN', 'VALUES']);

  const MERGE = [
    ['LEFT', 'OUTER', 'JOIN'], ['RIGHT', 'OUTER', 'JOIN'], ['FULL', 'OUTER', 'JOIN'],
    ['LEFT', 'JOIN'], ['RIGHT', 'JOIN'], ['FULL', 'JOIN'], ['INNER', 'JOIN'],
    ['CROSS', 'JOIN'], ['NATURAL', 'JOIN'],
    ['GROUP', 'BY'], ['ORDER', 'BY'], ['UNION', 'ALL'], ['UNION', 'DISTINCT'],
    ['ON', 'CONFLICT'],
  ];

  const WORD_START = /[A-Za-z_\u0080-\uFFFF]/;
  const WORD_PART = /[A-Za-z0-9_$\u0080-\uFFFF]/;
  const THREE_OPS = ['->>', '#>>'];
  const TWO_OPS = ['::', '<=', '>=', '<>', '!=', '||', '->', '=>', ':=', '#>', '@>', '<@', '!~', '~*'];

  /* ---------- 词法 ---------- */

  function tokenize(sql) {
    const out = [];
    let i = 0;
    const n = sql.length;
    while (i < n) {
      const c = sql[i];
      if (c === ' ' || c === '\t' || c === '\n' || c === '\r') { i++; continue; }

      if (c === '-' && sql[i + 1] === '-') {
        let j = sql.indexOf('\n', i);
        if (j < 0) j = n;
        out.push({ t: 'lcomment', v: sql.slice(i, j).replace(/\s+$/, '') });
        i = j;
        continue;
      }
      if (c === '/' && sql[i + 1] === '*') {
        let j = sql.indexOf('*/', i + 2);
        j = j < 0 ? n : j + 2;
        out.push({ t: 'bcomment', v: sql.slice(i, j) });
        i = j;
        continue;
      }
      if (c === "'") {
        let j = i + 1;
        let closed = false;
        while (j < n) {
          if (sql[j] === "'" && sql[j + 1] === "'") { j += 2; continue; }
          if (sql[j] === "'") { j++; closed = true; break; }
          j++;
        }
        if (!closed) return null;
        out.push({ t: 'str', v: sql.slice(i, j) });
        i = j;
        continue;
      }
      if (c === '$') {
        const m = /^\$([A-Za-z_][A-Za-z0-9_]*)?\$/.exec(sql.slice(i));
        if (m) {
          const tag = m[0];
          let j = sql.indexOf(tag, i + tag.length);
          if (j < 0) return null;
          j += tag.length;
          out.push({ t: 'str', v: sql.slice(i, j) });
          i = j;
          continue;
        }
        const p = /^\$\d+/.exec(sql.slice(i));
        if (p) { out.push({ t: 'param', v: p[0] }); i += p[0].length; continue; }
      }
      if (c === '"') {
        let j = i + 1;
        let closed = false;
        while (j < n) {
          if (sql[j] === '"' && sql[j + 1] === '"') { j += 2; continue; }
          if (sql[j] === '"') { j++; closed = true; break; }
          j++;
        }
        if (!closed) return null;
        out.push({ t: 'qident', v: sql.slice(i, j) });
        i = j;
        continue;
      }
      if (/[0-9]/.test(c) || (c === '.' && /[0-9]/.test(sql[i + 1] || ''))) {
        const m = /^(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?/.exec(sql.slice(i));
        out.push({ t: 'num', v: m[0] });
        i += m[0].length;
        continue;
      }
      if (WORD_START.test(c)) {
        let j = i + 1;
        while (j < n && WORD_PART.test(sql[j])) j++;
        const w = sql.slice(i, j);
        const up = w.toUpperCase();
        out.push(KEYWORDS.has(up) ? { t: 'kw', v: up } : { t: 'id', v: w });
        i = j;
        continue;
      }
      if ('(),;.[]'.indexOf(c) >= 0) { out.push({ t: 'punct', v: c }); i++; continue; }

      const three = sql.substr(i, 3);
      if (THREE_OPS.indexOf(three) >= 0) { out.push({ t: 'op', v: three }); i += 3; continue; }
      const two = sql.substr(i, 2);
      if (TWO_OPS.indexOf(two) >= 0) { out.push({ t: 'op', v: two }); i += 2; continue; }
      out.push({ t: 'op', v: c });
      i++;
    }
    return out;
  }

  // 把 LEFT OUTER JOIN / GROUP BY 这类多词子句合成一个 token，换行与空白都省事
  function mergeTokens(toks) {
    const out = [];
    for (let i = 0; i < toks.length; i++) {
      let hit = null;
      for (const pat of MERGE) {
        let ok = i + pat.length <= toks.length;
        for (let k = 0; ok && k < pat.length; k++) {
          const t = toks[i + k];
          ok = t && t.t === 'kw' && t.v === pat[k];
        }
        // 长的优先：LEFT OUTER JOIN 不能被 LEFT JOIN 抢走
        if (ok && (!hit || pat.length > hit.length)) hit = pat;
      }
      if (hit) {
        out.push({ t: 'kw', v: hit.join(' ') });
        i += hit.length - 1;
      } else {
        out.push(toks[i]);
      }
    }
    return out;
  }

  /* ---------- 结构探测 ---------- */

  // 括号里包的是子查询吗？(SELECT …) / (WITH …) / (VALUES …)
  function startsQuery(toks, i) {
    for (let j = i; j < toks.length; j++) {
      const t = toks[j];
      if (t.t === 'lcomment' || t.t === 'bcomment') continue;
      return t.t === 'kw' && (t.v === 'SELECT' || t.v === 'WITH' || t.v === 'VALUES');
    }
    return false;
  }

  // 一组括号的「内联长度」和顶层逗号数：长列表（列定义、超长 IN 列表）改成
  // 一项一行，否则整条语句会拉成一条几百字符的长线。
  function parenInfo(toks, i) {
    let depth = 1;
    let commas = 0;
    let len = 0;
    for (let j = i + 1; j < toks.length; j++) {
      const t = toks[j];
      if (t.t === 'punct' && t.v === '(') depth++;
      else if (t.t === 'punct' && t.v === ')') {
        depth--;
        if (depth === 0) return { end: j, commas: commas, len: len };
      } else if (depth === 1 && t.t === 'punct' && t.v === ',') {
        commas++;
      }
      len += t.v.length + 1;
    }
    return null;
  }

  /* ---------- 排版 ---------- */

  function layout(toks) {
    const MAX_INLINE = 72;

    const lines = [];
    let line = '';
    let indent = 0;
    let last = null;
    let prev2 = null;
    let prevUnary = false;
    let depth = 0;
    let blockDepth = 0;   // 当前查询块所在的圆括号深度：只有这一层的子句才换行
    let listAlign = -1;
    let listDepth = -1;
    let clause = '';
    let clauseDepth = 0;
    let betweenAt = -1;
    let sawJoin = false;  // 本语句里出现过 JOIN，ON 才算「联接条件」
    const pstack = [];

    function flush() {
      const text = line.replace(/\s+$/, '');
      if (text) lines.push(new Array(indent + 1).join(' ') + text);
      line = '';
      last = null;
      prev2 = null;
      prevUnary = false;
    }

    function baseIndent() {
      for (let i = pstack.length - 1; i >= 0; i--) {
        if (pstack[i].bodyIndent !== undefined) return pstack[i].bodyIndent;
      }
      return 0;
    }

    function isUnary(tok, before) {
      if (tok.t !== 'op' || (tok.v !== '-' && tok.v !== '+')) return false;
      if (!before) return true;
      if (before.t === 'op' || before.t === 'num') return false;
      if (before.t === 'punct' && (before.v === '(' || before.v === '[' || before.v === ',')) return true;
      if (before.t === 'kw' && ['SELECT', 'THEN', 'ELSE', 'WHEN', 'RETURNING', 'VALUES', 'SET', 'BY'].indexOf(before.v) >= 0) return true;
      return false;
    }

    function spaceBetween(a, b, aUnary, beforeA) {
      if (!a) return false;
      const av = a.v;
      if (a.t === 'punct' && (av === '(' || av === '[' || av === '.')) return false;
      if (a.t === 'op' && av === '::') return false;
      if (aUnary) return false;
      if (b.t === 'punct' && (b.v === ')' || b.v === ']' || b.v === ',' || b.v === ';' || b.v === '.')) return false;
      if (b.t === 'op' && b.v === '::') return false;
      if (b.v === '(') {
        if (a.t === 'id' || a.t === 'qident') {
          // INSERT INTO t (a, b) / CREATE TABLE t (…)：表名后留空格
          return !!(beforeA && beforeA.t === 'kw' && SPACE_PAREN_PREV.has(beforeA.v));
        }
        if (av === ')' || av === ']') return false;              // (a).b 之类
        if (a.t === 'kw' && FUNC_LIKE.has(av)) return false;
        return true;                                             // IN (…) / EXISTS (…)
      }
      if (b.v === '[') {
        // ARRAY[1,2] / ROW[...] 紧贴，其它情况（含 x[1] 下标）也不留空格
        return !(a.t === 'id' || a.t === 'qident' || av === ')' || av === ']' ||
                 (a.t === 'kw' && (av === 'ARRAY' || av === 'ROW')));
      }
      if (b.t === 'str' && a.t === 'id' && /^(E|B|X|N|U&)$/i.test(av)) return false;
      return true;
    }

    function add(tok, glue) {
      const before = last;
      const wasUnary = prevUnary;
      const txt = tok.v;
      if (line === '') {
        line = txt;
      } else if (glue === 'none' || !spaceBetween(before, tok, wasUnary, prev2)) {
        line += txt;
      } else {
        line += ' ' + txt;
      }
      prevUnary = isUnary(tok, before);
      prev2 = before;
      last = tok;
    }

    for (let i = 0; i < toks.length; i++) {
      const tk = toks[i];

      if (tk.t === 'lcomment' || tk.t === 'bcomment') {
        if (line !== '') flush();
        // 块注释原样放（可能自带换行）；行注释缩进对齐当前子句
        const parts = tk.v.split('\n');
        for (const p of parts) lines.push(p === '' ? '' : new Array(indent + 1).join(' ') + p);
        continue;
      }

      if (tk.t === 'punct' && tk.v === ';') {
        add(tk, 'none');
        flush();
        indent = 0; depth = 0; blockDepth = 0; listAlign = -1; listDepth = -1;
        clause = ''; clauseDepth = 0; betweenAt = -1; sawJoin = false; pstack.length = 0;
        continue;
      }

      if (tk.t === 'punct' && tk.v === '(') {
        const sub = startsQuery(toks, i + 1);
        const info = sub ? null : parenInfo(toks, i);
        if (!sub && !info) return null; // 括号不配对
        const block = !sub && info.commas > 0 && info.len > MAX_INLINE;
        add(tk);
        depth++;
        if (sub || block) {
          pstack.push({
            kind: sub ? 'sub' : 'block',
            lineIndent: indent,
            bodyIndent: indent + 2,
            listAlign: listAlign,
            listDepth: listDepth,
            clause: clause,
            clauseDepth: clauseDepth,
            blockDepth: blockDepth,
          });
          flush();
          indent = indent + 2;
          clause = '';
          if (sub) {
            blockDepth = depth;
            listAlign = -1;
            listDepth = -1;
          } else {
            listAlign = indent;
            listDepth = depth;
          }
        } else {
          pstack.push({ kind: 'plain' });
        }
        continue;
      }

      if (tk.t === 'punct' && tk.v === ')') {
        depth--;
        const top = pstack.pop();
        if (top && top.kind !== 'plain' && line !== '') flush();
        if (top && top.kind !== 'plain') {
          indent = top.lineIndent;
          listAlign = top.listAlign;
          listDepth = top.listDepth;
          clause = top.clause;
          clauseDepth = top.clauseDepth;
          blockDepth = top.blockDepth;
          add(tk);
        } else {
          add(tk);
        }
        continue;
      }

      if (tk.t === 'punct' && tk.v === ',') {
        add(tk, 'none');
        if (listAlign >= 0 && depth === listDepth) {
          flush();
          indent = listAlign;
        }
        continue;
      }

      if (tk.t === 'kw' && CLAUSE.has(tk.v)) {
        const kw = tk.v;
        // EXPLAIN 后面紧跟的 ANALYZE / VERBOSE / SELECT 留在同一行，别拆成几行；
        // DELETE FROM 也要连在一起（拆开读着像个残缺语句）
        const stay = (clause === 'EXPLAIN' &&
          (kw === 'SELECT' || kw === 'ANALYZE' || kw === 'VERBOSE')) ||
          (clause === 'DELETE' && kw === 'FROM');

        if (stay) {
          add(tk);
          if (kw === 'SELECT') {
            clause = 'SELECT';
            clauseDepth = depth;
            listAlign = indent + line.length + 1;
            listDepth = depth;
          }
          continue;
        }

        // 只有「当前查询块」这一层的子句才换行：SUBSTRING(name FROM 1 FOR 3)
        // 里的 FROM 是参数分隔符，不能当子句处理
        if (depth !== blockDepth) {
          add(tk);
          continue;
        }
        // 句子已经开了头的 DDL 关键字（ALTER … DROP COLUMN）按普通词处理
        if (STMT_START.has(kw) && clause !== '') {
          add(tk);
          continue;
        }
        if (line !== '') flush();
        indent = baseIndent();
        add(tk);
        clause = kw;
        clauseDepth = depth;
        listAlign = -1;
        if (LIST_CLAUSE.has(kw)) {
          listAlign = indent + line.length + 1;
          listDepth = depth;
        }
        continue;
      }

      if (tk.t === 'kw' && JOIN_KW.has(tk.v)) {
        if (line !== '') flush();
        indent = baseIndent();
        add(tk);
        clause = 'JOIN';
        clauseDepth = depth;
        listAlign = -1;
        sawJoin = true;
        continue;
      }

      if (tk.t === 'kw' && tk.v === 'ON' && sawJoin && depth === blockDepth) {
        if (line !== '') flush();
        indent = baseIndent() + 2;
        add(tk);
        clause = 'ON';
        clauseDepth = depth;
        listAlign = -1;
        continue;
      }

      if (tk.t === 'kw' && tk.v === 'BETWEEN') {
        betweenAt = depth;
        add(tk);
        continue;
      }

      if (tk.t === 'kw' && (tk.v === 'AND' || tk.v === 'OR')) {
        // BETWEEN 1 AND 2 里的 AND 不是条件分隔符
        if (tk.v === 'AND' && betweenAt === depth) {
          betweenAt = -1;
          add(tk);
          continue;
        }
        if (COND_CLAUSE.has(clause) && depth === clauseDepth) {
          if (line !== '') flush();
          // 上面可能有注释整行，line 为空但缩进仍要对齐到条件续行
          indent = baseIndent() + 2;
        }
        add(tk);
        continue;
      }

      add(tk);
    }

    flush();
    while (lines.length && lines[lines.length - 1] === '') lines.pop();
    return lines.join('\n');
  }

  function format(sql) {
    const text = String(sql == null ? '' : sql);
    if (!text.trim()) return text;
    const toks = tokenize(text);
    if (!toks || !toks.length) return null;
    // 括号/引号不配对就别动：宁可原样，也不要吐出半截
    let d = 0;
    for (const t of toks) {
      if (t.t === 'punct' && t.v === '(') d++;
      else if (t.t === 'punct' && t.v === ')') { d--; if (d < 0) return null; }
    }
    if (d !== 0) return null;
    const out = layout(mergeTokens(toks));
    return out;
  }

  window.GodbFormat = { format: format };
})();
