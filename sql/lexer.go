package sql

import (
	"fmt"
	"strings"
)

// Lex 把手写 SQL 文本切成词法单元。遇到非法字符会返回 *Error。
func Lex(src string) ([]Token, error) {
	l := &lexer{src: []rune(src), line: 1, col: 1}
	var toks []Token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		if t.Kind == EOF {
			toks = append(toks, t)
			return toks, nil
		}
		toks = append(toks, t)
	}
}

type lexer struct {
	src  []rune
	pos  int
	line int
	col  int
}

func (l *lexer) peekAt(n int) rune {
	if l.pos+n >= len(l.src) {
		return 0
	}
	return l.src[l.pos+n]
}

func (l *lexer) advance() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	c := l.src[l.pos]
	l.pos++
	if c == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return c
}

func (l *lexer) errf(format string, args ...interface{}) error {
	return &Error{Msg: fmt.Sprintf(format, args...), Line: l.line, Col: l.col}
}

func isIdentStart(c rune) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c > 127
}

func isIdentPart(c rune) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}

func isDigit(c rune) bool { return c >= '0' && c <= '9' }

func (l *lexer) next() (Token, error) {
	for {
		if l.pos >= len(l.src) {
			return Token{Kind: EOF, Line: l.line, Col: l.col}, nil
		}
		c := l.peekAt(0)
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v' {
			l.advance()
			continue
		}
		// 行注释
		if c == '-' && l.peekAt(1) == '-' {
			for l.pos < len(l.src) && l.peekAt(0) != '\n' {
				l.advance()
			}
			continue
		}
		// 块注释
		if c == '/' && l.peekAt(1) == '*' {
			l.advance()
			l.advance()
			closed := false
			for l.pos < len(l.src) {
				if l.peekAt(0) == '*' && l.peekAt(1) == '/' {
					l.advance()
					l.advance()
					closed = true
					break
				}
				l.advance()
			}
			if !closed {
				return Token{}, l.errf("unterminated block comment")
			}
			continue
		}
		break
	}

	startLine, startCol := l.line, l.col
	c := l.peekAt(0)

	switch {
	case isIdentStart(c):
		var b strings.Builder
		for l.pos < len(l.src) && isIdentPart(l.peekAt(0)) {
			b.WriteRune(l.advance())
		}
		return Token{Kind: Word, Text: b.String(), Line: startLine, Col: startCol}, nil

	case isDigit(c) || (c == '.' && isDigit(l.peekAt(1))):
		var b strings.Builder
		hasDot := false
		hasExp := false
		for l.pos < len(l.src) {
			ch := l.peekAt(0)
			if isDigit(ch) {
				b.WriteRune(l.advance())
				continue
			}
			if ch == '.' && !hasDot && !hasExp {
				hasDot = true
				b.WriteRune(l.advance())
				continue
			}
			if (ch == 'e' || ch == 'E') && !hasExp {
				nxt := l.peekAt(1)
				if isDigit(nxt) || ((nxt == '+' || nxt == '-') && isDigit(l.peekAt(2))) {
					hasExp = true
					b.WriteRune(l.advance())
					if l.peekAt(0) == '+' || l.peekAt(0) == '-' {
						b.WriteRune(l.advance())
					}
					continue
				}
			}
			break
		}
		return Token{Kind: NumberVal, Text: b.String(), Line: startLine, Col: startCol}, nil

	case c == '\'':
		l.advance()
		var b strings.Builder
		for {
			if l.pos >= len(l.src) {
				return Token{}, &Error{Msg: "unterminated string literal", Line: startLine, Col: startCol}
			}
			ch := l.advance()
			if ch == '\'' {
				if l.peekAt(0) == '\'' {
					l.advance()
					b.WriteRune('\'')
					continue
				}
				break
			}
			b.WriteRune(ch)
		}
		return Token{Kind: StringVal, Text: b.String(), Line: startLine, Col: startCol}, nil

	case c == '"':
		l.advance()
		var b strings.Builder
		for {
			if l.pos >= len(l.src) {
				return Token{}, &Error{Msg: "unterminated quoted identifier", Line: startLine, Col: startCol}
			}
			ch := l.advance()
			if ch == '"' {
				if l.peekAt(0) == '"' {
					l.advance()
					b.WriteRune('"')
					continue
				}
				break
			}
			b.WriteRune(ch)
		}
		return Token{Kind: QIdent, Text: b.String(), Line: startLine, Col: startCol}, nil
	}

	// 标点与运算符（含多字符）
	two := ""
	if l.pos+1 < len(l.src) {
		two = string([]rune{l.peekAt(0), l.peekAt(1)})
	}
	switch two {
	case "<=", ">=", "<>", "!=", "||", "::":
		l.advance()
		l.advance()
		return Token{Kind: Punct, Text: two, Line: startLine, Col: startCol}, nil
	}
	single := string(l.advance())
	switch single {
	case "=", "<", ">", "+", "-", "*", "/", "%", "(", ")", ",", ".", ";", ":":
		return Token{Kind: Punct, Text: single, Line: startLine, Col: startCol}, nil
	}
	return Token{}, &Error{Msg: fmt.Sprintf("syntax error at or near %q", single), Line: startLine, Col: startCol}
}
