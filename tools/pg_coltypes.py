"""比较两台 PG 服务器对同一条查询返回的列类型 OID。

客户端（Navicat）解析结果集依赖 RowDescription 里的类型 OID，不是值本身。
如果某个数值列被声明成 text，客户端做类型匹配就会失败。

用法：
    python tools/pg_coltypes.py <端口> <文件.sql> [用户] [库]
"""

import struct
import sys
import socket

OID_NAMES = {
    16: "bool", 17: "bytea", 18: "char", 19: "name", 20: "int8", 21: "int2",
    23: "int4", 25: "text", 26: "oid", 114: "json", 700: "float4", 701: "float8",
    1042: "bpchar", 1043: "varchar", 1082: "date", 1114: "timestamp",
    1184: "timestamptz", 1700: "numeric", 1009: "_text", 1007: "_int4",
    1034: "_aclitem", 1003: "_name", 2205: "regclass", 2950: "uuid",
}


def recv_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise RuntimeError("连接被关闭")
        buf += chunk
    return buf


def connect(port, user, db):
    sock = socket.create_connection(("127.0.0.1", port), timeout=15)
    body = struct.pack("!i", 196608)
    body += b"user\x00" + user.encode() + b"\x00"
    body += b"database\x00" + db.encode() + b"\x00\x00"
    sock.sendall(struct.pack("!i", len(body) + 4) + body)
    while True:
        typ = recv_exact(sock, 1)
        (ln,) = struct.unpack("!i", recv_exact(sock, 4))
        if ln > 4:
            recv_exact(sock, ln - 4)
        if typ == b"Z":
            break
    return sock


def columns(port, user, db, sql):
    sock = connect(port, user, db)
    payload = sql.encode() + b"\x00"
    sock.sendall(b"Q" + struct.pack("!i", len(payload) + 4) + payload)

    cols = []
    err = None
    while True:
        typ = recv_exact(sock, 1)
        (ln,) = struct.unpack("!i", recv_exact(sock, 4))
        body = recv_exact(sock, ln - 4) if ln > 4 else b""
        if typ == b"T":  # RowDescription
            n = struct.unpack("!h", body[:2])[0]
            off = 2
            for _ in range(n):
                end = body.index(b"\x00", off)
                name = body[off:end].decode("utf-8", "replace")
                off = end + 1
                off += 4 + 2  # tableOID, attnum
                oid = struct.unpack("!i", body[off:off + 4])[0]
                off += 4
                off += 2 + 4 + 2  # typlen, typmod, format
                cols.append((name, oid))
        if typ == b"E":
            err = body[:160].decode("utf-8", "replace")
        if typ == b"Z":
            break
    sock.close()
    return cols, err


def main():
    port = int(sys.argv[1])
    path = sys.argv[2]
    user = sys.argv[3] if len(sys.argv) > 3 else "zwccn"
    db = sys.argv[4] if len(sys.argv) > 4 else "godb"
    sql = open(path, encoding="utf-8").read().strip()

    cols, err = columns(port, user, db, sql)
    if err:
        print("ERROR:", err)
    for name, oid in cols:
        print("%-26s %6d %s" % (name, oid, OID_NAMES.get(oid, "?")))


if __name__ == "__main__":
    main()
