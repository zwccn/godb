"""把某一批客户端语句逐条在两台 PG 服务器上跑，比对「列名 + 类型 OID」。

客户端解析结果集靠 RowDescription 的 OID，值一样但 OID 不同也会出问题。
用法：
    python tools/diff_batch_types.py 09:28:54
"""

import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
TMP = os.path.join(os.environ.get("TEMP", "."), "godb_diff")
OURS = ("6432", "zwccn", "godb")
THEIRS = ("5432", "postgres", "godb_cmp")


def load(prefix):
    txt = open("pg-queries.log", encoding="utf-8", errors="replace").read().replace("\r", "")
    out = []
    for part in txt.split("\n-- "):
        line, _, body = part.partition("\n")
        if line.startswith(prefix):
            out.append((line.strip(), body.strip()))
    return out


def coltypes(server, path):
    port, user, db = server
    p = subprocess.run(
        [sys.executable, os.path.join(HERE, "pg_coltypes.py"), port, path, user, db],
        capture_output=True, text=True, timeout=60,
    )
    lines = [l for l in (p.stdout + p.stderr).splitlines() if l.strip()]
    return lines


def main():
    prefix = sys.argv[1]
    items = load(prefix)
    os.makedirs(TMP, exist_ok=True)
    print("批次 %s：%d 条" % (prefix, len(items)))

    diffs = 0
    for ts, sql in items:
        path = os.path.join(TMP, "q.sql")
        with open(path, "w", encoding="utf-8") as f:
            f.write(sql)
        a = coltypes(OURS, path)
        b = coltypes(THEIRS, path)
        if a == b:
            continue
        diffs += 1
        print("\n--- %s %s" % (ts, " ".join(sql.split())[:80]))
        only_a = [x for x in a if x not in b]
        only_b = [x for x in b if x not in a]
        for x in only_a:
            print("   我们:", x)
        for x in only_b:
            print("    PG:", x)
    print("\n有差异的语句：%d / %d" % (diffs, len(items)))


if __name__ == "__main__":
    main()
