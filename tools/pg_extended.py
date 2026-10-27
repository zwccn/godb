"""走扩展协议（Parse/Describe/Bind/Execute）跑一条查询，打印各步响应。

用来确认预编译路径是通的：客户端有时只用扩展协议取元数据，
如果 Describe 返回 NoData，它就拿不到列信息。
"""

import socket
import struct
import sys


def recv_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise RuntimeError("连接被关闭")
        buf += chunk
    return buf


def msg(typ, payload):
    return typ + struct.pack("!i", len(payload) + 4) + payload


def read_msgs(sock, until=b"Z"):
    out = []
    while True:
        typ = recv_exact(sock, 1)
        (ln,) = struct.unpack("!i", recv_exact(sock, 4))
        body = recv_exact(sock, ln - 4) if ln > 4 else b""
        out.append((typ, body))
        if typ == until:
            return out


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 6432
    user = sys.argv[2] if len(sys.argv) > 2 else "zwccn"
    db = sys.argv[3] if len(sys.argv) > 3 else "godb"
    sql = sys.argv[4] if len(sys.argv) > 4 else "select 1 as a, 'x' as b"

    sock = socket.create_connection(("127.0.0.1", port), timeout=10)
    body = struct.pack("!i", 196608) + b"user\x00" + user.encode() + b"\x00"
    body += b"database\x00" + db.encode() + b"\x00\x00"
    sock.sendall(struct.pack("!i", len(body) + 4) + body)
    read_msgs(sock)

    # Parse + Describe(statement) + Sync
    parse = b"\x00" + sql.encode() + b"\x00" + struct.pack("!h", 0)
    sock.sendall(msg(b"P", parse) + msg(b"D", b"S\x00") + msg(b"S", b""))
    replies = read_msgs(sock)

    names = {b"1": "ParseComplete", b"t": "ParameterDescription",
             b"T": "RowDescription", b"n": "NoData", b"E": "ErrorResponse",
             b"Z": "ReadyForQuery"}
    for typ, body in replies:
        label = names.get(typ, typ.decode("latin1"))
        extra = ""
        if typ == b"T":
            n = struct.unpack("!h", body[:2])[0]
            fields = []
            off = 2
            for _ in range(n):
                end = body.index(b"\x00", off)
                fields.append(body[off:end].decode())
                off = end + 1 + 4 + 2 + 4 + 2 + 4 + 2
            extra = " -> " + ", ".join(fields)
        elif typ == b"E":
            extra = " -> " + body.decode("utf-8", "replace")[:200]
        print("%-18s%s" % (label, extra))
    sock.close()


if __name__ == "__main__":
    main()
