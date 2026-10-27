"""把 pg-queries.log 里某一批次的语句逐条发给 /api/query，打印服务端耗时。

用途：客户端「感觉慢」时，先分清是网络往返、还是某条 SQL 本身慢。
"""

import json
import sys
import urllib.request

URL = "http://127.0.0.1:8080/api/query"
LOG = "pg-queries.log"


def load(prefix):
    txt = open(LOG, encoding="utf-8", errors="replace").read().replace("\r", "")
    out = []
    for part in txt.split("\n-- "):
        line, _, body = part.partition("\n")
        if line.startswith(prefix):
            out.append((line.strip(), body.strip()))
    return out


def main():
    prefix = sys.argv[1] if len(sys.argv) > 1 else ""
    items = load(prefix)
    print("批次 %s：%d 条" % (prefix, len(items)))

    total = 0
    for ts, sql in items:
        req = urllib.request.Request(
            URL,
            data=json.dumps({"sql": sql}).encode(),
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                data = json.load(r)
        except Exception as e:  # noqa: BLE001
            print("[ERR ] %s %-60s %s" % (ts, " ".join(sql.split())[:60], e))
            continue

        res = data["results"][0]
        ms = res["tookNs"] / 1e6
        total += ms
        rows = res.get("rowCount", 0)
        err = res.get("error")
        flag = "ERR " if err else "ok  "
        print("%s %s %7.1f ms  rows=%-4d %s" % (flag, ts, ms, rows, " ".join(sql.split())[:58]))
        if err:
            print("        ->", err)

    print("服务端合计 %.1f ms" % total)


if __name__ == "__main__":
    main()
