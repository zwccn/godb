// Package pgwire 实现 PostgreSQL 前端/后端协议（v3）的一个子集，
// 让 psql、Navicat、pgAdmin 之类的 PG 客户端可以直接连上来跑 SQL。
//
// 支持：StartupMessage、SSLRequest/GSSENCRequest（回 'N' 回落明文）、
// AuthenticationOk（trust，无密码）、简单查询（Q）、扩展查询（P/B/D/E/S/C/H）、Terminate。
// 不支持：密码认证、COPY、游标、二进制结果格式、$n 之外的参数类型推断。
package pgwire

import (
	"bytes"
	"encoding/binary"
)

// 消息类型（后端 → 前端）
const (
	msgAuthentication     = 'R'
	msgBackendKeyData     = 'K'
	msgParameterStatus    = 'S'
	msgReadyForQuery      = 'Z'
	msgRowDescription     = 'T'
	msgDataRow            = 'D'
	msgCommandComplete    = 'C'
	msgEmptyQueryResponse = 'I'
	msgErrorResponse      = 'E'
	msgNoticeResponse     = 'N'
	msgParameterDesc      = 't'
	msgNoData             = 'n'
	msgParseComplete      = '1'
	msgBindComplete       = '2'
	msgCloseComplete      = '3'
)

// writer 是后端消息编码器：先写 payload，最后一个字节的类型前缀 + 长度一次写出。
type writer struct {
	buf bytes.Buffer
}

func (w *writer) byte(b byte)     { w.buf.WriteByte(b) }
func (w *writer) bytes(p []byte)  { w.buf.Write(p) }
func (w *writer) int16(v int16)   { _ = binary.Write(&w.buf, binary.BigEndian, v) }
func (w *writer) int32(v int32)   { _ = binary.Write(&w.buf, binary.BigEndian, v) }
func (w *writer) uint32(v uint32) { _ = binary.Write(&w.buf, binary.BigEndian, v) }

func (w *writer) str(s string) {
	w.buf.WriteString(s)
	w.buf.WriteByte(0)
}

// message 产出一条带类型前缀和长度头的完整消息。
func (w *writer) message(typ byte) []byte {
	payload := w.buf.Bytes()
	out := make([]byte, 0, len(payload)+5)
	out = append(out, typ)
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)+4))
	out = append(out, payload...)
	return out
}

// ---------- 具体消息 ----------

func encodeAuthenticationOK() []byte {
	w := &writer{}
	w.int32(0) // AuthenticationOk
	return w.message(msgAuthentication)
}

// encodeAuthenticationCleartextPassword 要求客户端提交明文密码（AuthenticationCleartextPassword）。
// 我们没有 TLS，所以只支持这一种认证方式；客户端拿到的密码会在服务端
// 与 PBKDF2 哈希比对，不会落盘也不写日志。
func encodeAuthenticationCleartextPassword() []byte {
	w := &writer{}
	w.int32(3) // AuthenticationCleartextPassword
	return w.message(msgAuthentication)
}

func encodeParameterStatus(key, value string) []byte {
	w := &writer{}
	w.str(key)
	w.str(value)
	return w.message(msgParameterStatus)
}

func encodeBackendKeyData(pid, secret int32) []byte {
	w := &writer{}
	w.int32(pid)
	w.int32(secret)
	return w.message(msgBackendKeyData)
}

func encodeReadyForQuery(state byte) []byte {
	w := &writer{}
	w.byte(state) // 'I' idle / 'T' in transaction / 'E' failed transaction
	return w.message(msgReadyForQuery)
}

// field 是 RowDescription 里的一列。
type field struct {
	Name    string
	TypeOID int32
	TypeLen int16
	Format  int16 // 0 = text
}

func encodeRowDescription(fields []field) []byte {
	w := &writer{}
	w.int16(int16(len(fields)))
	for _, f := range fields {
		w.str(f.Name)
		w.int32(0) // 表 OID
		w.int16(0) // 列号
		w.int32(f.TypeOID)
		w.int16(f.TypeLen)
		w.int32(-1) // type modifier
		w.int16(f.Format)
	}
	return w.message(msgRowDescription)
}

func encodeDataRow(values [][]byte) []byte {
	w := &writer{}
	w.int16(int16(len(values)))
	for _, v := range values {
		if v == nil {
			w.int32(-1)
			continue
		}
		w.int32(int32(len(v)))
		w.bytes(v)
	}
	return w.message(msgDataRow)
}

func encodeCommandComplete(tag string) []byte {
	w := &writer{}
	w.str(tag)
	return w.message(msgCommandComplete)
}

func encodeEmptyQuery() []byte {
	return (&writer{}).message(msgEmptyQueryResponse)
}

func encodeParameterDescription(oids []int32) []byte {
	w := &writer{}
	w.int16(int16(len(oids)))
	for _, o := range oids {
		w.int32(o)
	}
	return w.message(msgParameterDesc)
}

func encodeNoData() []byte {
	return (&writer{}).message(msgNoData)
}

func encodeParseComplete() []byte { return (&writer{}).message(msgParseComplete) }
func encodeBindComplete() []byte  { return (&writer{}).message(msgBindComplete) }
func encodeCloseComplete() []byte { return (&writer{}).message(msgCloseComplete) }

// encodeError 组装 ErrorResponse。code 是 SQLSTATE。
func encodeError(severity, code, message string) []byte {
	w := &writer{}
	w.byte('S')
	w.str(severity)
	w.byte('V')
	w.str(severity)
	w.byte('C')
	w.str(code)
	w.byte('M')
	w.str(message)
	w.byte(0) // 字段列表结束
	return w.message(msgErrorResponse)
}

func encodeNotice(message string) []byte {
	w := &writer{}
	w.byte('S')
	w.str("NOTICE")
	w.byte('C')
	w.str("00000")
	w.byte('M')
	w.str(message)
	w.byte(0)
	return w.message(msgNoticeResponse)
}

// ---------- COPY 相关消息 ----------

const (
	msgCopyInResponse  = 'G'
	msgCopyOutResponse = 'H'
	msgCopyData        = 'd'
	msgCopyDone        = 'c'
	msgCopyFail        = 'f'
)

// encodeCopyResponse 生成 CopyInResponse('G') 或 CopyOutResponse('H')。
// 格式固定为 text（0），每列也用 text。
func encodeCopyResponse(typ byte, numCols int) []byte {
	w := &writer{}
	w.byte(0) // overall format: text
	w.int16(int16(numCols))
	for i := 0; i < numCols; i++ {
		w.int16(0) // per-column format: text
	}
	return w.message(typ)
}

func encodeCopyData(b []byte) []byte {
	w := &writer{}
	w.bytes(b)
	return w.message(msgCopyData)
}

func encodeCopyDone() []byte {
	w := &writer{}
	return w.message(msgCopyDone)
}
