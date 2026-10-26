package sql

import (
	"fmt"
	"strings"
)

// TokKind 是词法单元类型。
type TokKind uint8

const (
	EOF       TokKind = iota
	Word              // 裸词：标识符或关键字
	QIdent            // 双引号标识符
	StringVal         // 单引号字符串
	NumberVal         // 数字字面量
	Punct             // 标点/运算符
)

// Token 是一个词法单元。
type Token struct {
	Kind TokKind
	Text string // Word: 原始写法；QIdent/StringVal: 反转义后的内容
	Line int
	Col  int
}

// Is 判断是否为某个关键字（大小写不敏感，仅对裸词有效）。
func (t Token) Is(kw string) bool {
	return t.Kind == Word && strings.EqualFold(t.Text, kw)
}

// IsPunct 判断是否为某个标点。
func (t Token) IsPunct(p string) bool {
	return t.Kind == Punct && t.Text == p
}

func (t Token) String() string {
	if t.Kind == EOF {
		return "end of input"
	}
	if t.Text == "" {
		return "?"
	}
	return t.Text
}

// Error 是解析期错误，带行号列号，便于前端定位。
type Error struct {
	Msg  string
	Line int
	Col  int
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s (line %d, column %d)", e.Msg, e.Line, e.Col)
	}
	return e.Msg
}

// 保留字：不能作为裸标识符使用。
var reserved = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
	select from where group by having order limit offset as distinct all
	fetch for
	join inner outer left right full cross on using natural
	and or not null is in between like ilike similar
	case when then else end
	insert into values update set delete
	create table drop alter add column rename to truncate
	begin commit rollback transaction start work
	primary key unique default check references foreign constraint
	true false asc desc nulls first last cast explain analyze
	if exists cascade restrict returning
	union intersect except
	`) {
		reserved[w] = true
	}
}

// IsReserved 判断裸词是否为保留字。
func IsReserved(w string) bool {
	return reserved[strings.ToLower(w)]
}
