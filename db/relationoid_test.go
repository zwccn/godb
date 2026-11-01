package db

import (
	"fmt"
	"testing"
)

// RelationOID 现在有一条不建 meta 的快速路径（OID = 基准 + 表在 catalog 里的下标）。
// 它必须和 pg_class 的输出永远一致 —— 否则 `'t'::regclass`、外键的 conrelid、
// 客户端的元数据查询会各自看到不同的 OID。
//
// 这里特意覆盖「多张表」和「中间删一张表」两种情况：删表后后面表的下标会前移，
// 两套算法都得跟着变（既有行为如此，不是这次引入的）。
func TestRelationOIDMatchesPgClass(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	for _, q := range []string{
		`CREATE TABLE ra (id int4 PRIMARY KEY)`,
		`CREATE TABLE rb (id int4 PRIMARY KEY)`,
		`CREATE TABLE rc (id int4 PRIMARY KEY)`,
	} {
		run(t, d, s, q)
	}

	check := func(phase string, names ...string) {
		t.Helper()
		for _, name := range names {
			res := run(t, d, s, fmt.Sprintf(`
				SELECT '%[1]s'::regclass = (SELECT oid FROM pg_class WHERE relname = '%[1]s') AS same`, name))
			if got := fmt.Sprint(res[0].Rows[0][0]); got != "true" {
				t.Fatalf("%s：%s 的 regclass 与 pg_class.oid 不一致（%s）", phase, name, got)
			}
		}
	}
	check("三张表都在", "ra", "rb", "rc")

	// 删表后后面表的下标会前移，两套算法都得跟着变
	run(t, d, s, `DROP TABLE rb`)
	check("删掉中间一张表之后", "ra", "rc")
}
