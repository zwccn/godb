#!/usr/bin/env python3
"""向量能力冒烟测试（T5~T8 的功能面）。

    python tools/vec_smoke.py [http://127.0.0.1:8080]

只依赖标准库，直接打控制台的 /api/query。跑完会留下一张 t_vec 表（方便手工
继续看），下称重跑会先 DROP 掉。数据量刻意只有 11 行：这里验的是「行为对不对」，
性能数字在 go test -bench Vec 里量。
"""
import json
import sys
import urllib.request
import http.cookiejar

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"

cj = http.cookiejar.CookieJar()
# 显式禁用代理：本机 127.0.0.1 走系统代理会连不上（和 curl 要 --noproxy 同理）
op = urllib.request.build_opener(
    urllib.request.ProxyHandler({}),
    urllib.request.HTTPCookieProcessor(cj),
)

fails = []


def q(sql):
    req = urllib.request.Request(
        BASE + "/api/query",
        data=json.dumps({"sql": sql}).encode(),
        headers={"Content-Type": "application/json"},
    )
    return json.loads(op.open(req).read())


def rows(sql):
    r = q(sql)
    if r.get("error"):
        raise RuntimeError("SQL 错误 %r: %s" % (sql, r["error"]))
    res = r["results"][0]
    return res.get("rows") or []


def schema_columns(table):
    raw = json.loads(op.open(BASE + "/api/schema").read())
    for t in raw["tables"]:
        if t["name"] == table:
            return {c["name"]: c["type"] for c in t["columns"]}
    return {}


def check(name, ok, detail=""):
    print(("  PASS  " if ok else "  FAIL  ") + name + ("" if ok else "  <- " + str(detail)))
    if not ok:
        fails.append(name)


print("== 准备：4 维小表 10 行 + 1 行 NULL")
q("DROP TABLE IF EXISTS t_vec")
q("CREATE TABLE t_vec (id INT4 PRIMARY KEY, e VECTOR(4), note TEXT)")
check("CREATE TABLE ... VECTOR(4)", q("SELECT count(*) FROM t_vec").get("error") is None)

pts = [[1, 0, 0, 0], [0, 1, 0, 0], [0, 0, 1, 0], [0, 0, 0, 1], [1, 1, 0, 0],
       [1, 0, 1, 0], [1, 0, 0, 1], [2, 0, 0, 0], [0, 2, 0, 0], [3, 0, 0, 0]]
vals = ["(%d,'[%s]','n%d')" % (i, ",".join(str(x) for x in p), i) for i, p in enumerate(pts)]
q("INSERT INTO t_vec VALUES " + ",".join(vals))
check("插入 10 行", len(rows("SELECT id FROM t_vec")) == 10)

print("== T5 DDL")
check("schema 里 e 列是 vector(4)", schema_columns("t_vec").get("e") == "vector(4)",
      schema_columns("t_vec"))
q("ALTER TABLE t_vec ADD COLUMN f VECTOR(2)")
check("ADD COLUMN VECTOR(2)", schema_columns("t_vec").get("f") == "vector(2)",
      schema_columns("t_vec"))
q("UPDATE t_vec SET f = '[1,1]' WHERE id = 0")
check("新列可写可读", rows("SELECT f FROM t_vec WHERE id = 0")[0][0] == "[1,1]")
check("没写过的行新列为 NULL", rows("SELECT f FROM t_vec WHERE id = 5")[0][0] is None)

print("== T6 Top-K")
full = rows("SELECT id, e <-> '[1,0,0,0]' AS d FROM t_vec ORDER BY d")
top3 = rows("SELECT id, e <-> '[1,0,0,0]' AS d FROM t_vec ORDER BY d LIMIT 3")
# 距离并列时先返回哪一行不保证（PG 也不保证），所以比「集合 + 距离序列」，
# 而不是比 id 的先后顺序。
check("Top-3 = 全量排序前 3（忽略并列顺序）",
      sorted(r[0] for r in top3) == sorted(r[0] for r in full[:3]),
      (top3, full[:3]))
check("Top-3 距离一致", [round(r[1], 5) for r in top3] == [round(r[1], 5) for r in full[:3]])
cos = rows("SELECT id, e <=> '[1,0,0,0]' AS d FROM t_vec ORDER BY d LIMIT 2")
# [1,0,0,0] [2,0,0,0] [3,0,0,0] 三个向量方向相同，余弦距离都是 0
check("余弦距离 <=>", len(cos) == 2 and all(abs(r[1]) < 1e-6 for r in cos), cos)
negip = rows("SELECT id, e <#> '[1,0,0,0]' AS d FROM t_vec ORDER BY d LIMIT 2")
check("负内积 <#>", len(negip) == 2 and negip[0][1] <= negip[1][1], negip)
ex = rows("EXPLAIN SELECT id FROM t_vec ORDER BY e <-> '[1,0,0,0]' LIMIT 3")
check("EXPLAIN 走 Top-N Sort", any("Top-N" in str(x) for x in ex), ex)

print("== T7 过滤与边界")
fv = rows("SELECT id FROM t_vec WHERE id < 5 ORDER BY e <-> '[1,0,0,0]' LIMIT 2")
check("WHERE + 向量排序", len(fv) == 2 and all(r[0] < 5 for r in fv), fv)
q("INSERT INTO t_vec (id, e) VALUES (100, NULL)")
allr = rows("SELECT id FROM t_vec ORDER BY e <-> '[1,0,0,0]' LIMIT 11")
check("NULL 向量不报错并排在最后", allr[-1][0] == 100, allr)
check("NULL 行距离是 NULL",
      rows("SELECT e <-> '[1,0,0,0]' FROM t_vec WHERE id = 100")[0][0] is None)
check("LIMIT 0", rows("SELECT id FROM t_vec ORDER BY e <-> '[1,0,0,0]' LIMIT 0") == [])
off = rows("SELECT id FROM t_vec ORDER BY e <-> '[1,0,0,0]' LIMIT 2 OFFSET 1")
check("OFFSET 生效（忽略并列顺序）",
      sorted(r[0] for r in off) == sorted(r[0] for r in full[1:3]), (off, full[1:3]))
err = (q("SELECT e <-> '[1,2]' FROM t_vec").get("error") or {}).get("message", "")
check("维度不匹配在编译期报错", "dimension mismatch" in err, err)
check("过滤后没有行也不报错",
      q("SELECT id FROM t_vec WHERE id > 9999 ORDER BY e <-> '[1,0,0,0]' LIMIT 3").get("error") is None)

print("== T8 输出与 DDL 收尾")
got = rows("SELECT e FROM t_vec WHERE id = 0")[0][0]
check("向量按 [..] 文本输出", got.startswith("[1,") and got.endswith("]"), got)
q("UPDATE t_vec SET e = '[9,9,9,9]' WHERE id = 1")
check("UPDATE 向量生效", rows("SELECT e FROM t_vec WHERE id = 1")[0][0] == "[9,9,9,9]")
q("DELETE FROM t_vec WHERE id = 1")
check("DELETE 行生效", len(rows("SELECT id FROM t_vec")) == 10)
q("ALTER TABLE t_vec DROP COLUMN f")
check("DROP COLUMN VECTOR", "f" not in schema_columns("t_vec"), schema_columns("t_vec"))
check("删列后其余列仍可查", len(rows("SELECT id, e FROM t_vec ORDER BY id")) == 10)

print()
if fails:
    print("失败 %d 项: %s" % (len(fails), fails))
    sys.exit(1)
print("全部通过")
