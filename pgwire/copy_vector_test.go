package pgwire

import (
	"strings"
	"testing"
)

// COPY 走协议层时也要导出真实向量内容（[1,2,3]），而不是行里的槽位引用。
//
// db 层已有一条同样的单测（TestCopyVector），这里钉的是协议路径本身：
// CopyOutResponse / CopyData 的字节里到底是什么 —— 曾经是 `1\t[vector:0]`。
func TestCopyVectorOverWire(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	if msgs := c.query(`CREATE TABLE cv (id int PRIMARY KEY, v vector(3))`); errorMsg(msgs) != "" {
		t.Fatalf("建表失败: %s", errorMsg(msgs))
	}
	if msgs := c.query(`INSERT INTO cv VALUES (1,'[1,2,3]'), (2,'[4,5,6]')`); errorMsg(msgs) != "" {
		t.Fatalf("插入失败: %s", errorMsg(msgs))
	}

	msgs := c.query(`COPY cv TO STDOUT`)
	if e := errorMsg(msgs); e != "" {
		t.Fatalf("COPY TO 失败: %s", e)
	}
	var data strings.Builder
	sawOut := false
	for _, m := range msgs {
		switch m.typ {
		case 'H':
			sawOut = true
		case 'd':
			data.Write(m.body)
		}
	}
	if !sawOut {
		t.Fatal("没有收到 CopyOutResponse('H')")
	}
	got := data.String()
	if strings.Contains(got, "vector:") {
		t.Fatalf("导出了内部引用: %q", got)
	}
	if !strings.Contains(got, "1\t[1,2,3]") || !strings.Contains(got, "2\t[4,5,6]") {
		t.Fatalf("COPY 数据不对: %q", got)
	}

	// 清空后用同一份字节 COPY FROM 灌回去：行数与内容都要回来。
	if msgs := c.query(`DELETE FROM cv`); errorMsg(msgs) != "" {
		t.Fatalf("清空失败: %s", errorMsg(msgs))
	}
	p := append([]byte("COPY cv FROM STDIN"), 0)
	c.sendRaw(pgMsg('Q', p))
	c.readUntil('G') // CopyInResponse：可以发数据了
	c.sendRaw(pgMsg('d', []byte(got)))
	c.sendRaw(pgMsg('c', nil)) // CopyDone
	rest := c.readUntil('Z')
	if e := errorMsg(rest); e != "" {
		t.Fatalf("COPY FROM 失败: %s", e)
	}
	if tag := cmdTag(rest); tag != "COPY 2" {
		t.Fatalf("COPY FROM 结果 %q，期望 COPY 2", tag)
	}

	rows := dataRows(c.query(`SELECT id, v FROM cv ORDER BY id`))
	if len(rows) != 2 || string(rows[0][1]) != "[1,2,3]" || string(rows[1][1]) != "[4,5,6]" {
		t.Fatalf("导回后内容不对: %v", rows)
	}
}
