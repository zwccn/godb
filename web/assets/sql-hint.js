// SQL 智能提示：PostgreSQL 10 的关键字 / 函数 / 类型，加上当前库的真实表和列。
//
// 为什么自己写而不是用 CodeMirror 自带的 addon/hint/sql-hint.js：
// 那个版本的目的是「通用 SQL 补全」，词表是 MySQL-ish 的混合体，也不认识
// 我们的 /api/schema。这里只服务 godb 控制台，所以可以把 PG 10 的关键字表、
// 常用函数、列类型，以及当前库的表结构一起喂进去，还能按上下文排序
// （输入 FROM 时先给表名，输入 SELECT/WHERE 时先给列名）。
//
// 依赖：CodeMirror 5 + addon/hint/show-hint（都在 vendor/ 里，不走 CDN）。
(function (global) {
  'use strict';

  // PG 10 的关键字（附录 C 的保留字 + 常用非保留关键字）。
  var KEYWORDS = (
    'ABORT ABSOLUTE ACCESS ACTION ADD ADMIN AFTER AGGREGATE ALL ALSO ALTER ALWAYS ANALYSE ANALYZE AND ANY ARRAY AS ASC ' +
    'ASSERTION ASSIGNMENT ASYMMETRIC AT ATTACH ATTRIBUTE AUTHORIZATION BACKWARD BEFORE BEGIN BETWEEN BIGINT BINARY BIT ' +
    'BOOLEAN BOTH BY CACHE CALLED CASCADE CASCADED CASE CAST CATALOG CHAIN CHAR CHARACTER CHARACTERISTICS CHECK ' +
    'CHECKPOINT CLASS CLOSE CLUSTER COALESCE COLLATE COLLATION COLUMN COLUMNS COMMENT COMMENTS COMMIT COMMITTED ' +
    'CONCURRENTLY CONFIGURATION CONFLICT CONNECTION CONSTRAINT CONSTRAINTS CONTENT CONTINUE CONVERSION COPY CREATE CROSS ' +
    'CSV CUBE CURRENT CURRENT_CATALOG CURRENT_DATE CURRENT_ROLE CURRENT_SCHEMA CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER ' +
    'CURSOR CYCLE DATA DATABASE DAY DEALLOCATE DEC DECIMAL DECLARE DEFAULT DEFAULTS DEFERRABLE DEFERRED DEFINER DELETE ' +
    'DELIMITER DELIMITERS DESC DETACH DICTIONARY DISABLE DISCARD DISTINCT DO DOCUMENT DOMAIN DOUBLE DROP EACH ELSE ENABLE ' +
    'ENCODING ENCRYPTED END ENUM ESCAPE EVENT EXCEPT EXCLUDE EXCLUDING EXCLUSIVE EXECUTE EXISTS EXPLAIN EXTENSION ' +
    'EXTERNAL EXTRACT FALSE FAMILY FETCH FILTER FIRST FLOAT FOLLOWING FOR FORCE FOREIGN FORWARD FREEZE FROM FULL FUNCTION ' +
    'FUNCTIONS GENERATED GLOBAL GRANT GRANTED GREATEST GROUP GROUPING HANDLER HAVING HEADER HOLD HOUR IDENTITY IF ILIKE ' +
    'IMMEDIATE IMMUTABLE IMPLICIT IMPORT IN INCLUDING INCREMENT INDEX INDEXES INHERIT INHERITS INITIALLY INLINE INNER ' +
    'INOUT INPUT INSENSITIVE INSERT INSTEAD INT INTEGER INTERSECT INTERVAL INTO INVOKER IS ISNULL ISOLATION JOIN KEY ' +
    'LABEL LANGUAGE LARGE LAST LATERAL LEADING LEAKPROOF LEAST LEFT LEVEL LIKE LIMIT LISTEN LOAD LOCAL LOCALTIME ' +
    'LOCALTIMESTAMP LOCATION LOCK LOCKED LOGGED MAPPING MATCH MATERIALIZED MAXVALUE METHOD MINUTE MINVALUE MODE MONTH ' +
    'MOVE NAME NAMES NATIONAL NATURAL NCHAR NEXT NO NONE NOT NOTHING NOTIFY NOTNULL NOWAIT NULL NULLIF NULLS NUMERIC ' +
    'OBJECT OF OFF OFFSET OIDS ON ONLY OPERATOR OPTION OPTIONS OR ORDER ORDINALITY OUT OUTER OVER OVERLAPS OVERLAY ' +
    'OWNED OWNER PARALLEL PARSER PARTIAL PARTITION PASSING PASSWORD PLACING PLANS POLICY POSITION PRECEDING PRECISION ' +
    'PREPARE PREPARED PRESERVE PRIMARY PRIOR PRIVILEGES PROCEDURAL PROCEDURE PROGRAM PUBLICATION QUOTE RANGE READ REAL ' +
    'REASSIGN RECHECK RECURSIVE REF REFERENCES REFRESH REINDEX RELATIVE RELEASE RENAME REPEATABLE REPLACE REPLICA RESET ' +
    'RESTART RESTRICT RETURNING RETURNS REVOKE RIGHT ROLE ROLLBACK ROLLUP ROW ROWS RULE SAVEPOINT SCHEMA SCROLL SEARCH ' +
    'SECOND SECURITY SELECT SEQUENCE SEQUENCES SERIALIZABLE SERVER SESSION SESSION_USER SET SETOF SETS SHARE SHOW SIMILAR ' +
    'SIMPLE SKIP SMALLINT SNAPSHOT SOME SQL STABLE STANDALONE START STATEMENT STATISTICS STDIN STDOUT STORAGE STRICT ' +
    'STRIP SUBSTRING SYMMETRIC SYSID SYSTEM TABLE TABLES TABLESAMPLE TABLESPACE TEMP TEMPLATE TEMPORARY TEXT THEN TIES ' +
    'TIME TIMESTAMP TO TRAILING TRANSACTION TRANSFORM TREAT TRIGGER TRIM TRUE TRUNCATE TRUSTED TYPE TYPES UNBOUNDED ' +
    'UNCOMMITTED UNENCRYPTED UNION UNIQUE UNKNOWN UNLISTEN UNLOGGED UNTIL UPDATE USER USING VACUUM VALID VALIDATE ' +
    'VALIDATOR VALUE VALUES VARCHAR VARIADIC VARYING VERBOSE VERSION VIEW VIEWS VOLATILE WHEN WHERE WHITESPACE WINDOW ' +
    'WITH WITHIN WITHOUT WORK WRAPPER WRITE XML YEAR YES ZONE'
  ).split(' ');

  // 常用函数：hint 里带上签名，省得记参数顺序。
  var FUNCTIONS = [
    ['count', 'count(expr)'], ['count(*)', 'count(*)'],
    ['sum', 'sum(expr)'], ['avg', 'avg(expr)'], ['min', 'min(expr)'], ['max', 'max(expr)'],
    ['string_agg', 'string_agg(expr, delim)'], ['array_agg', 'array_agg(expr)'],
    ['coalesce', 'coalesce(a, b, ...)'], ['nullif', 'nullif(a, b)'],
    ['greatest', 'greatest(a, b)'], ['least', 'least(a, b)'],
    ['now', 'now()'], ['age', 'age(ts)'], ['date_trunc', 'date_trunc(field, ts)'],
    ['date_part', 'date_part(field, ts)'], ['extract', 'extract(field FROM ts)'],
    ['to_char', 'to_char(ts, fmt)'], ['to_date', 'to_date(text, fmt)'],
    ['to_timestamp', 'to_timestamp(text, fmt)'], ['to_number', 'to_number(text, fmt)'],
    ['length', 'length(text)'], ['char_length', 'char_length(text)'],
    ['lower', 'lower(text)'], ['upper', 'upper(text)'], ['initcap', 'initcap(text)'],
    ['trim', 'trim(text)'], ['ltrim', 'ltrim(text)'], ['rtrim', 'rtrim(text)'],
    ['lpad', 'lpad(text, len, fill)'], ['rpad', 'rpad(text, len, fill)'],
    ['substring', 'substring(text FROM start FOR len)'], ['position', 'position(sub IN text)'],
    ['strpos', 'strpos(text, sub)'], ['replace', 'replace(text, from, to)'],
    ['split_part', 'split_part(text, delim, n)'],
    ['concat', 'concat(a, b, ...)'], ['concat_ws', 'concat_ws(sep, a, b, ...)'],
    ['format', 'format(fmt, ...)'], ['left', 'left(text, n)'], ['right', 'right(text, n)'],
    ['reverse', 'reverse(text)'], ['ascii', 'ascii(text)'], ['chr', 'chr(code)'],
    ['repeat', 'repeat(text, n)'], ['md5', 'md5(text)'],
    ['regexp_matches', 'regexp_matches(text, pattern)'],
    ['regexp_replace', 'regexp_replace(text, pattern, repl)'],
    ['array_length', 'array_length(arr, dim)'], ['array_to_string', 'array_to_string(arr, delim)'],
    ['string_to_array', 'string_to_array(text, delim)'],
    ['unnest', 'unnest(array)'], ['generate_series', 'generate_series(start, stop[, step])'],
    ['abs', 'abs(x)'], ['ceil', 'ceil(x)'], ['floor', 'floor(x)'], ['round', 'round(x[, n])'],
    ['power', 'power(a, b)'], ['sqrt', 'sqrt(x)'], ['mod', 'mod(a, b)'],
    ['random', 'random()'], ['sign', 'sign(x)'], ['trunc', 'trunc(x)'],
    ['version', 'version()'], ['current_database', 'current_database()'],
    ['current_schema', 'current_schema()'], ['current_setting', 'current_setting(name)'],
    ['pg_typeof', 'pg_typeof(expr)'], ['pg_backend_pid', 'pg_backend_pid()'],
    ['pg_size_pretty', 'pg_size_pretty(bytes)'], ['pg_database_size', 'pg_database_size(name)'],
    ['pg_get_indexdef', 'pg_get_indexdef(index_oid)'],
    ['pg_get_constraintdef', 'pg_get_constraintdef(oid)'],
    ['pg_get_expr', 'pg_get_expr(expr, relid)'],
    ['pg_get_userbyid', 'pg_get_userbyid(oid)'],
    ['pg_table_is_visible', 'pg_table_is_visible(oid)'],
    ['obj_description', 'obj_description(oid, catalog)'],
    ['col_description', 'col_description(oid, colnum)'],
    ['quote_ident', 'quote_ident(text)'], ['quote_literal', 'quote_literal(text)'],
    ['nextval', 'nextval(seq)'], ['currval', 'currval(seq)'], ['setval', 'setval(seq, n)'],
    ['txid_current', 'txid_current()'], ['cast', 'cast(expr AS type)']
  ];

  var TYPES = (
    'int2 int4 int8 smallint integer bigint serial bigserial real float4 float8 double ' +
    'numeric decimal money text varchar char character varying bytea boolean bool date time ' +
    'timestamp timestamptz interval json jsonb uuid inet cidr macaddr xml point line lseg box ' +
    'path polygon circle tsvector tsquery oid regclass'
  ).split(' ');

  // 这些词后面该跟表名（而不是列名）
  var TABLE_CTX = ['from', 'join', 'update', 'into', 'table', 'truncate', 'analyze', 'vacuum',
    'drop', 'alter', 'describe', 'on'];

  // 由 app.js 注入：() => [{name, columns:[{name, type}]}]
  var schemaProvider = function () { return []; };

  function setSchemaProvider(fn) {
    if (typeof fn === 'function') schemaProvider = fn;
  }

  function tables() {
    try {
      return schemaProvider() || [];
    } catch (e) {
      return [];
    }
  }

  function findTable(name) {
    if (!name) return null;
    var lower = name.toLowerCase();
    var list = tables();
    for (var i = 0; i < list.length; i++) {
      if (list[i].name.toLowerCase() === lower) return list[i];
    }
    return null;
  }

  // statementAt 取光标**所在**的那条语句（前后两个分号之间）。
  //
  // 不能只取光标之前的部分：`SELECT e.| FROM employees e` 这种写法里，
  // 决定 `e` 是哪张表的 FROM 子句在光标**后面**，只往前看就永远解析不到别名
  // （现象是 `e.` 之后弹出来的是表名和关键字，而不是列）。
  function statementAt(cm, cur) {
    var doc = cm.getValue();
    var idx = cm.indexFromPos(cur);
    var start = doc.lastIndexOf(';', idx - 1) + 1;
    var end = doc.indexOf(';', idx);
    if (end < 0) end = doc.length;
    return doc.slice(start, end);
  }

  // collectScope 找出当前语句里 FROM / JOIN / UPDATE / INTO 后的表和别名。
  // 返回 [{table, alias}]，alias 可能是空串。
  function collectScope(stmt) {
    var out = [];
    var re = /\b(from|join|update|into)\s+([A-Za-z_][\w$]*)(?:\s+(?:as\s+)?([A-Za-z_][\w$]*))?/gi;
    var m;
    while ((m = re.exec(stmt)) !== null) {
      var name = m[2];
      var alias = m[3] || '';
      // `FROM SELECT` / `INTO VALUES` 这种不是表名
      if (/^(select|values|set|where|group|order|limit|returning)$/i.test(name)) continue;
      out.push({ table: name, alias: alias });
    }
    return out;
  }

  // resolveQualifier 把 `x.` 里的 x 解析成表：可以是表名，也可以是别名。
  function resolveQualifier(qual, scope) {
    for (var i = 0; i < scope.length; i++) {
      if (scope[i].alias && scope[i].alias.toLowerCase() === qual) {
        return findTable(scope[i].table);
      }
    }
    var direct = findTable(qual);
    if (direct) return direct;
    for (var j = 0; j < scope.length; j++) {
      if (scope[j].table.toLowerCase() === qual) return findTable(scope[j].table);
    }
    return null;
  }

  // 光标前的单词（补全前缀）
  function wordBefore(cm, cur) {
    var line = cm.getLine(cur.line) || '';
    var before = line.slice(0, cur.ch);
    var m = /[A-Za-z_][\w$]*$/.exec(before);
    return m ? m[0] : '';
  }

  function prevWord(cm, cur) {
    var line = cm.getLine(cur.line) || '';
    var before = line.slice(0, cur.ch);
    var m = /([A-Za-z_][\w$]*)\s*(?:\([^()]*\))?\s*$/.exec(before);
    return m ? m[1].toLowerCase() : '';
  }

  function keywordItems(name) {
    // 已经写出来的名字就不要再提示一遍（大小写不敏感）
    var lower = name.toLowerCase();
    var out = [];
    for (var i = 0; i < KEYWORDS.length; i++) {
      var k = KEYWORDS[i];
      if (k.toLowerCase() === lower) continue;
      out.push({ text: k, displayText: k, group: 3, note: '关键字' });
    }
    return out;
  }

  function functionItems(name) {
    var lower = name.toLowerCase();
    var out = [];
    for (var i = 0; i < FUNCTIONS.length; i++) {
      var f = FUNCTIONS[i];
      if (f[0].toLowerCase() === lower) continue;
      out.push({ text: f[0], displayText: f[0], group: 4, note: f[1] });
    }
    return out;
  }

  function typeItems(name) {
    var lower = name.toLowerCase();
    var out = [];
    for (var i = 0; i < TYPES.length; i++) {
      if (TYPES[i].toLowerCase() === lower) continue;
      out.push({ text: TYPES[i], displayText: TYPES[i], group: 5, note: '类型' });
    }
    return out;
  }

  function columnItems(t, qual) {
    if (!t) return [];
    var out = [];
    var cols = t.columns || [];
    for (var i = 0; i < cols.length; i++) {
      var c = cols[i];
      out.push({
        // `SELECT t.` 之后接着补全列名时保留限定符，方便连写
        text: qual ? qual + '.' + c.name : c.name,
        displayText: c.name,
        group: 1,
        note: (c.type || '') + (c.primaryKey ? ' · 主键' : '')
      });
    }
    return out;
  }

  function tableItems(name) {
    var lower = name.toLowerCase();
    var out = [];
    var list = tables();
    for (var i = 0; i < list.length; i++) {
      if (list[i].name.toLowerCase() === lower) continue;
      out.push({ text: list[i].name, displayText: list[i].name, group: 2, note: '表' });
    }
    return out;
  }

  // filterByWord 按已输入的前缀收窄候选。
  // show-hint 自己**不做**过滤，它把 list 原样铺出来；过滤是 hint 函数的责任。
  // 前缀命中排前面，其次才是「包含」命中（打 `emp` 也要能找到 `employees`）。
  function filterByWord(list, word) {
    var lower = (word || '').toLowerCase();
    if (!lower) return list;
    var starts = [];
    var contains = [];
    for (var i = 0; i < list.length; i++) {
      var text = (list[i].displayText || list[i].text || '').toLowerCase();
      if (text.lastIndexOf(lower, 0) === 0) {
        starts.push(list[i]);
      } else if (text.indexOf(lower) > 0) {
        contains.push(list[i]);
      }
    }
    return starts.concat(contains);
  }

  // build 是所有分支的统一收尾：按前缀收窄、限量、给每条挂上渲染函数。
  // 之前别名分支自己 return，漏了挂 render —— 表现为「列能补全，但右侧没有类型」。
  function build(list, word, from, cur) {
    list = filterByWord(list, word || '');
    if (!list.length) return null;
    if (!word && list.length > 60) list = list.slice(0, 60);
    for (var i = 0; i < list.length; i++) {
      // 注意字段叫 note 不叫 hint：show-hint 把 item.hint 当「插入回调函数」，
      // 同名会让它把字符串当函数调用（TypeError，表现为点了候选没反应）
      if (list[i].note) list[i].render = renderHint;
    }
    return { list: list, from: from, to: cur };
  }

  // complete 是交给 CodeMirror 的 hint 函数。
  function complete(cm) {
    var cur = cm.getCursor();
    var word = wordBefore(cm, cur);
    var from = { line: cur.line, ch: cur.ch - word.length };
    var stmt = statementAt(cm, cur);
    var scope = collectScope(stmt);

    // 1) `别名.` 或 `表名.` → 只给这张表的列
    var line = cm.getLine(cur.line) || '';
    var before = line.slice(0, cur.ch);
    // 末尾的列名可能还没开始写（`WHERE e.` 就要弹），所以最后一段是可选的
    var dot = /([A-Za-z_][\w$]*)\s*\.\s*(?:[A-Za-z_][\w$]*)?$/.exec(before);
    if (dot) {
      var t = resolveQualifier(dot[1].toLowerCase(), scope);
      // 别名已经打过了，插入时只插列名，否则会变成 `e.e.salary`
      var cols = columnItems(t, '');
      if (cols.length) return build(cols, word, from, cur);
    }

    // 2) 上下文：前一个词决定优先给什么
    var prev = prevWord(cm, cur);
    var wantTable = TABLE_CTX.indexOf(prev) >= 0;

    var items = [];
    if (wantTable) {
      items = items.concat(tableItems(word));
      items = items.concat(keywordItems(word).filter(function (k) {
        // 这里给表名，关键字只留少数几个真会出现在这儿的
        return ['select', 'values', 'lateral', 'only'].indexOf(k.text.toLowerCase()) >= 0;
      }));
    } else {
      // 默认：当前语句涉及的表，列优先（写 SELECT / WHERE / ORDER BY 时最常用）
      for (var i = 0; i < scope.length; i++) {
        var tbl = findTable(scope[i].table);
        if (!tbl) continue;
        // 多表时带上限定符，避免列名歧义
        var qual = scope.length > 1 ? (scope[i].alias || scope[i].table) : '';
        items = items.concat(columnItems(tbl, qual));
      }
      items = items.concat(tableItems(word));
      items = items.concat(functionItems(word));
      items = items.concat(keywordItems(word));
      items = items.concat(typeItems(word));
    }

    if (!items.length) return null;

    // 去重：同一 text 只留第一次出现的（列 > 表 > 函数 > 关键字）
    var seen = Object.create(null);
    var list = [];
    for (var k = 0; k < items.length; k++) {
      var it = items[k];
      var key = it.text.toLowerCase();
      if (seen[key]) continue;
      seen[key] = 1;
      list.push(it);
    }

    // 前缀完全匹配的排前面（CodeMirror 自己也会按前缀过滤，但我们要先排序）
    list.sort(function (a, b) {
      return (a.group - b.group) || a.displayText.localeCompare(b.displayText);
    });

    return build(list, word, from, cur);
  }

  // 给每条候选加右侧说明（列类型 / 函数签名 / 关键字标记）。
  // 注意 show-hint 是「每条自己 render」：cur.render(elt, data, cur)，
  // 所以这个函数要挂到每个 item 上，而不是写在 hintOptions 里。
  function renderHint(el, _data, cur) {
    el.textContent = '';
    var left = document.createElement('span');
    left.textContent = cur.displayText || cur.text;
    el.appendChild(left);
    if (cur && cur.note) {
      var right = document.createElement('span');
      right.className = 'cm-hint-note';
      right.textContent = cur.note;
      el.appendChild(right);
    }
  }

  global.GodbHint = {
    setSchemaProvider: setSchemaProvider,
    complete: complete,
    render: renderHint,
    keywords: KEYWORDS,
    functions: FUNCTIONS,
    types: TYPES
  };
})(window);
