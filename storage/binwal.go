package storage

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"godb/types"
)

// 二进制行编码。WAL 用它代替 JSON：体积小一个数量级，解析不需要反射/字符串分配。
//
// 值编码（单字节 tag + 定长/变长负载）：
//
//	0 NULL
//	1 int64    8 字节
//	2 float64  8 字节
//	3 bool     1 字节
//	4 text     uint32 长度 + UTF-8 字节
//	5 time     int64 UnixNano + int32 时区偏移（秒）
//	6 decimal  uint32 长度 + 规范十进制文本（NUMERIC 精确值）
const (
	tagNull    byte = 0
	tagInt     byte = 1
	tagFloat   byte = 2
	tagBool    byte = 3
	tagText    byte = 4
	tagTime    byte = 5
	tagDecimal byte = 6
)

func appendValue(b []byte, v types.Value) []byte {
	switch v.K {
	case types.KNull:
		return append(b, tagNull)
	case types.KInt:
		b = append(b, tagInt)
		return binary.LittleEndian.AppendUint64(b, uint64(v.I))
	case types.KFloat:
		b = append(b, tagFloat)
		return binary.LittleEndian.AppendUint64(b, math.Float64bits(v.Float()))
	case types.KBool:
		b = append(b, tagBool)
		if v.Bool() {
			return append(b, 1)
		}
		return append(b, 0)
	case types.KText:
		b = append(b, tagText)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(v.S)))
		return append(b, v.S...)
	case types.KDecimal:
		b = append(b, tagDecimal)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(v.S)))
		return append(b, v.S...)
	case types.KTime:
		b = append(b, tagTime)
		b = binary.LittleEndian.AppendUint64(b, uint64(v.I))
		_, off := v.Time().Zone()
		return binary.LittleEndian.AppendUint32(b, uint32(int32(off)))
	}
	return append(b, tagNull)
}

// readValue 解一个值，返回 (值, 新的偏移, 错误)。
func readValue(b []byte, off int) (types.Value, int, error) {
	if off >= len(b) {
		return types.Null, off, fmt.Errorf("值编码越界")
	}
	tag := b[off]
	off++
	switch tag {
	case tagNull:
		return types.Null, off, nil
	case tagInt:
		if off+8 > len(b) {
			return types.Null, off, fmt.Errorf("int64 越界")
		}
		v := int64(binary.LittleEndian.Uint64(b[off:]))
		return types.Int(v), off + 8, nil
	case tagFloat:
		if off+8 > len(b) {
			return types.Null, off, fmt.Errorf("float64 越界")
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(b[off:]))
		return types.Float(v), off + 8, nil
	case tagBool:
		if off >= len(b) {
			return types.Null, off, fmt.Errorf("bool 越界")
		}
		v := b[off] != 0
		return types.Bool(v), off + 1, nil
	case tagText:
		if off+4 > len(b) {
			return types.Null, off, fmt.Errorf("text 长度越界")
		}
		n := int(binary.LittleEndian.Uint32(b[off:]))
		off += 4
		if off+n > len(b) {
			return types.Null, off, fmt.Errorf("text 内容越界")
		}
		return types.Text(string(b[off : off+n])), off + n, nil
	case tagDecimal:
		if off+4 > len(b) {
			return types.Null, off, fmt.Errorf("decimal 长度越界")
		}
		n := int(binary.LittleEndian.Uint32(b[off:]))
		off += 4
		if off+n > len(b) {
			return types.Null, off, fmt.Errorf("decimal 内容越界")
		}
		return types.Decimal(string(b[off : off+n])), off + n, nil
	case tagTime:
		if off+12 > len(b) {
			return types.Null, off, fmt.Errorf("time 越界")
		}
		nanos := int64(binary.LittleEndian.Uint64(b[off:]))
		off += 8
		offSec := int32(binary.LittleEndian.Uint32(b[off:]))
		off += 4
		loc := time.FixedZone("", int(offSec))
		return types.Time(time.Unix(0, nanos).In(loc)), off, nil
	}
	return types.Null, off, fmt.Errorf("未知的值 tag %d", tag)
}

// appendRow 编码一行：uint32 列数 + 每列的值。
func appendRow(b []byte, row types.Row) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(row)))
	for _, v := range row {
		b = appendValue(b, v)
	}
	return b
}

// readRowValues 解一行，返回 (值列表, 新偏移, 错误)。列数与 catalog 的
// 一致性由 castRow 负责（补齐 / 截断），这里只按行内自带的列数读。
func readRowValues(b []byte, off int) ([]types.Value, int, error) {
	if off+4 > len(b) {
		return nil, off, fmt.Errorf("行头越界")
	}
	n := int(binary.LittleEndian.Uint32(b[off:]))
	off += 4
	out := make([]types.Value, 0, n)
	for i := 0; i < n; i++ {
		v, next, err := readValue(b, off)
		if err != nil {
			return nil, off, err
		}
		off = next
		out = append(out, v)
	}
	return out, off, nil
}
