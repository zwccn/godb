"""把 pg-queries.log 里记录的客户端语句原样重放一遍，逐条报 OK / FAIL。

排查客户端兼容性的标准动作：客户端只在弹窗里给一行错误，看不出是哪条
语句挂了。用 `-pg-log` 抓到原句后，这个脚本负责逐条送回去、把报错的那
条显出来，比靠猜快得多。

用法：
    python tools/replay_pglog.py              # 全部重放
    python tools/replay_pglog.py 18:47:57     # 只重放这个时间前缀的
"""

import re
import subprocess
import sys

PSQL = r"C:\Users\Administrator\Desktop\pgsql\bin\psql.exe"
LOG = "pg-queries.log"


def load(path=LOG):
    """按 "-- HH:MM:SS.mmm" 分隔符切出一条条语句。"""
    txt = open(path, encoding="utf-8", errors="replace").read().replace("\r", "")
    items = []
    for part in txt.split("\n-- "):
        line, _, body = part.partition("\n")
        line = line.strip()
        if re.match(r"^\d\d:\d\d:\d\d", line):
            items.append((line, body.strip()))
    return items


def main():
    only = sys.argv[1] if len(sys.argv) > 1 else None
    items = [it for it in load() if only is None or it[0].startswith(only)]
    print("重放 %d 条" % len(items))

    failed = 0
    for ts, body in items:
        p = subprocess.run(
            [PSQL, "-h", "127.0.0.1", "-p", "6432", "-U", "zwccn", "-d", "godb", "-w"],
            input=body,
            capture_output=True,
            text=True,
            timeout=60,
        )
        out = p.stdout + p.stderr
        errs = [l for l in out.splitlines() if "ERROR" in l]
        head = " ".join(body.split())[:70]
        if errs:
            failed += 1
            print("[FAIL] %s %s\n   -> %s" % (ts, head, errs[0][:200]))
        else:
            print("[ OK ] %s %s" % (ts, head))

    print("\n%d 条失败 / %d 条" % (failed, len(items)))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
