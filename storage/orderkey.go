package storage

import (
	"encoding/binary"
	"math"

	"godb/types"
)

// orderKey 把一行（或一组值）编码成**保序**的字符串：逐字节比较等价于逐列比大小。
//
// 为什么不用 types.Key：它 int 用十进制字符串，`"10" < "9"`，只适合做哈希的等值键。
// 范围扫描、`BETWEEN`、`LIKE 'x%'`、`ORDER BY` 都必须靠「编码序 == 值序」，
// 所以另起一套：
//
//	每列：1 字节类型标签 + 值
//	  int  / time : 8 字节大端，符号位翻转（负数也能正确排序）
//	  float       : IEEE754 位模式，负数全取反、非负数只翻符号位
//	  bool        : 1 字节
//	  text        : 原字节 + 0x00 终止；内部 0x00 转义成 0x00 0xFF
//
// 文本用「转义 + 终止符」而不是长度前缀：长度前缀会让 "ab" 和 "a" 的比较取决于
// 长度而非内容（`\x02ab` vs `\x01a`），破坏保序性。
// 这几个标签是「排序用」的，和 binwal.go 里 WAL 序列化的 tag* 不是一回事：
// 那套标签只为编码无歧义，这套只为「同类型比较结果稳定」。同一列类型固定，
// 所以跨类型顺序无所谓，各占一个不同的字节即可。
const (
	ordInt   = 1
	ordFloat = 2
	ordText  = 3
	ordBool  = 4
	ordTime  = 5
)

// OrderKey 把一组值编码成保序键（同类型的多列直接拼接，各列自定界）。
func OrderKey(vals []types.Value) string {
	buf := make([]byte, 0, len(vals)*10)
	for _, v := range vals {
		buf = appendOrderKey(buf, v)
	}
	return string(buf)
}

// OrderKeyOfRow 按列位置从行里取键；含 NULL 或缺列时 ok=false（这种行不进索引）。
func OrderKeyOfRow(row types.Row, cols []int) (string, bool) {
	if len(cols) == 0 {
		return "", false
	}
	buf := make([]byte, 0, len(cols)*10)
	for _, ci := range cols {
		if ci >= len(row) || row[ci].IsNull() {
			return "", false
		}
		buf = appendOrderKey(buf, row[ci])
	}
	return string(buf), true
}

// AppendOrderKeyInto 复用外部缓冲追加（整表重建时避免每行一次分配）。
func AppendOrderKeyInto(buf []byte, row types.Row, cols []int) ([]byte, bool) {
	if len(cols) == 0 {
		return buf, false
	}
	for _, ci := range cols {
		if ci >= len(row) || row[ci].IsNull() {
			return buf, false
		}
		buf = appendOrderKey(buf, row[ci])
	}
	return buf, true
}

func appendOrderKey(dst []byte, v types.Value) []byte {
	switch v.K {
	case types.KInt:
		dst = append(dst, ordInt)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v.I)^(1<<63))
		return append(dst, b[:]...)
	case types.KFloat:
		f := v.Float()
		// -0.0 与 0.0 相等，编码必须一致，否则范围扫描会把它们当成两个键
		if f == 0 {
			f = 0
		}
		u := math.Float64bits(f)
		if u&(1<<63) != 0 {
			u = ^u // 负数：整体取反，越负越小
		} else {
			u |= 1 << 63 // 非负数：翻符号位，排在负数之后
		}
		dst = append(dst, ordFloat)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], u)
		return append(dst, b[:]...)
	case types.KBool:
		if v.Bool() {
			return append(dst, ordBool, 1)
		}
		return append(dst, ordBool, 0)
	case types.KTime:
		dst = append(dst, ordTime)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v.I)^(1<<63))
		return append(dst, b[:]...)
	case types.KText:
		dst = append(dst, ordText)
		s := v.S
		for i := 0; i < len(s); i++ {
			c := s[i]
			dst = append(dst, c)
			if c == 0 {
				dst = append(dst, 0xFF) // 转义，避免和终止符混淆
			}
		}
		return append(dst, 0x00)
	}
	// NULL 不该走到这里（调用方已过滤），兜底给一个最小标签
	return append(dst, 0)
}

// DecodeOrderKey 把保序键解码回值序列（最多取 want 列；want<=0 表示全部）。
// 用于覆盖索引扫描：命中键就能还原出索引列的值，不必回表。
func DecodeOrderKey(key string, want int) ([]types.Value, bool) {
	b := []byte(key)
	var out []types.Value
	for len(b) > 0 {
		if want > 0 && len(out) >= want {
			break
		}
		v, n, ok := decodeOrderValue(b)
		if !ok {
			return nil, false
		}
		out = append(out, v)
		b = b[n:]
	}
	return out, true
}

func decodeOrderValue(b []byte) (types.Value, int, bool) {
	if len(b) == 0 {
		return types.Null, 0, false
	}
	switch b[0] {
	case ordInt:
		if len(b) < 9 {
			return types.Null, 0, false
		}
		u := binary.BigEndian.Uint64(b[1:9])
		return types.Int(int64(u ^ (1 << 63))), 9, true
	case ordFloat:
		if len(b) < 9 {
			return types.Null, 0, false
		}
		u := binary.BigEndian.Uint64(b[1:9])
		if u&(1<<63) != 0 {
			u &^= 1 << 63
		} else {
			u = ^u
		}
		return types.Float(math.Float64frombits(u)), 9, true
	case ordBool:
		if len(b) < 2 {
			return types.Null, 0, false
		}
		return types.Bool(b[1] != 0), 2, true
	case ordTime:
		if len(b) < 9 {
			return types.Null, 0, false
		}
		u := binary.BigEndian.Uint64(b[1:9])
		return types.Value{K: types.KTime, I: int64(u ^ (1 << 63))}, 9, true
	case ordText:
		var s []byte
		i := 1
		for i < len(b) {
			c := b[i]
			if c == 0 {
				if i+1 < len(b) && b[i+1] == 0xFF {
					s = append(s, 0)
					i += 2
					continue
				}
				return types.Text(string(s)), i + 1, true
			}
			s = append(s, c)
			i++
		}
		return types.Null, 0, false
	}
	return types.Null, 0, false
}

// TextPrefixRange 返回 `LIKE 'prefix%'` 对应的键区间（左闭右开）。
//
// 文本编码是「类型标签 + 内容 + 0x00」，所以 "abc" 的编码一定小于任何以它开头
// 的长串（"abc\0" < "abcx\0"）。上界取「前缀编码去掉终止符 + 一串 0xFF」——
// UTF-8 里不会出现 0xFF，足够覆盖所有后续字节。
func TextPrefixRange(prefix string) (lo, hi string) {
	enc := OrderKey([]types.Value{types.Text(prefix)})
	base := enc[:len(enc)-1] // 去掉终止符 0x00
	return enc, base + "\xff\xff\xff\xff\xff\xff\xff\xff"
}
