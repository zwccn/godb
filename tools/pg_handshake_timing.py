"""量一下 PG 握手各阶段耗时：TCP 连接 / SSL 协商 / 启动到 ReadyForQuery。

用法：python tools/pg_handshake_timing.py [次数]
"""

import socket
import struct
import sys
import time

HOST, PORT = "127.0.0.1", 6432


def recv_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise RuntimeError("连接被关闭")
        buf += chunk
    return buf


def one_round(user="zwccn", db="godb"):
    t0 = time.perf_counter()
    sock = socket.create_connection((HOST, PORT), timeout=10)
    t1 = time.perf_counter()
    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)

    # SSLRequest
    sock.sendall(struct.pack("!ii", 8, 80877103))
    answer = recv_exact(sock, 1)
    t2 = time.perf_counter()

    # StartupMessage
    body = struct.pack("!i", 196608)
    body += b"user\x00" + user.encode() + b"\x00"
    body += b"database\x00" + db.encode() + b"\x00"
    body += b"client_encoding\x00UTF8\x00"
    body += b"application_name\x00navicat-test\x00\x00"
    sock.sendall(struct.pack("!i", len(body) + 4) + body)

    # 读到 ReadyForQuery（'Z'）为止
    phases = []
    while True:
        typ = recv_exact(sock, 1)
        (ln,) = struct.unpack("!i", recv_exact(sock, 4))
        payload = recv_exact(sock, ln - 4) if ln > 4 else b""
        phases.append(typ.decode("latin1"))
        if typ == b"Z":
            break
        if typ == b"E":
            print("  收到 ErrorResponse:", payload[:120])
            break
    t3 = time.perf_counter()

    # 一条简单查询的往返
    sql = b"select 1\x00"
    sock.sendall(b"Q" + struct.pack("!i", len(sql) + 4) + sql)
    while True:
        typ = recv_exact(sock, 1)
        (ln,) = struct.unpack("!i", recv_exact(sock, 4))
        _ = recv_exact(sock, ln - 4) if ln > 4 else b""
        if typ == b"Z":
            break
    t4 = time.perf_counter()
    sock.close()

    return {
        "tcp": (t1 - t0) * 1000,
        "ssl": (t2 - t1) * 1000,
        "startup": (t3 - t2) * 1000,
        "query": (t4 - t3) * 1000,
        "ssl_answer": answer.decode("latin1"),
        "msgs": "".join(phases),
    }


def main():
    rounds = int(sys.argv[1]) if len(sys.argv) > 1 else 5
    print("%-6s %-9s %-9s %-10s %-9s %s" % ("次", "TCP(ms)", "SSL(ms)", "启动(ms)", "查询(ms)", "服务端消息"))
    for i in range(rounds):
        r = one_round()
        print("%-6d %-9.1f %-9.1f %-10.1f %-9.1f ssl=%s %s" % (
            i + 1, r["tcp"], r["ssl"], r["startup"], r["query"],
            r["ssl_answer"], r["msgs"]))


if __name__ == "__main__":
    main()
