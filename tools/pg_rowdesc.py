"""临时：打印 RowDescription 每列的 tableOID/attnum/typeOID。"""
import io
import socket
import struct
import sys


def recv_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise EOFError("closed")
        buf += chunk
    return buf


def msg(sock):
    head = recv_exact(sock, 5)
    ln = struct.unpack("!i", head[1:5])[0]
    body = recv_exact(sock, ln - 4) if ln > 4 else b""
    return head[0:1], body


def main():
    sql = sys.argv[1]
    if sql.startswith("@"):
        sql = io.open(sql[1:], encoding="utf-8").read().strip()
    if sql.endswith(";"):
        sql = sql[:-1]
    port = int(sys.argv[2]) if len(sys.argv) > 2 else 6432
    user = sys.argv[3] if len(sys.argv) > 3 else "godb"
    db = sys.argv[4] if len(sys.argv) > 4 else "godb"
    s = socket.create_connection(("127.0.0.1", port), timeout=5)
    params = ("user\x00%s\x00database\x00%s\x00client_encoding\x00UTF8\x00\x00" % (user, db)).encode()
    s.sendall(struct.pack("!i", len(params) + 8) + struct.pack("!i", 196608) + params)
    while True:
        typ, body = msg(s)
        if typ == b"Z":
            break
        if typ == b"E":
            print("startup error:", body[:200])
            return
    s.sendall(b"Q" + struct.pack("!i", len(sql.encode()) + 5) + sql.encode() + b"\0")
    while True:
        typ, body = msg(s)
        if typ == b"T":
            n = struct.unpack("!h", body[0:2])[0]
            off = 2
            print("%-12s %8s %6s %8s %4s %6s %6s" % ("column", "tableOID", "attnum", "typeOID", "len", "mod", "fmt"))
            for _ in range(n):
                end = body.index(b"\0", off)
                name = body[off:end].decode("utf-8", "replace")
                off = end + 1
                oid, attnum, typoid = struct.unpack("!ihi", body[off:off + 10])
                off += 10
                typlen, typmod, fmt = struct.unpack("!hih", body[off:off + 8])
                off += 8
                print("%-12s %8d %6d %8d %4d %6d %6d" % (name, oid, attnum, typoid, typlen, typmod, fmt))
        elif typ == b"D":
            n = struct.unpack("!h", body[0:2])[0]
            off2 = 2
            vals = []
            for _ in range(n):
                (ln,) = struct.unpack("!i", body[off2:off2 + 4])
                off2 += 4
                if ln < 0:
                    vals.append("NULL")
                else:
                    vals.append(body[off2:off2 + ln].decode("utf-8", "replace"))
                    off2 += ln
            print("row  ", vals)
            break
        elif typ == b"E":
            print("query error:", body[:300])
            break
        elif typ == b"Z":
            break


if __name__ == "__main__":
    main()
