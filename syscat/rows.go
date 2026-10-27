package syscat

import (
	"fmt"
	"sort"
	"strings"

	"godb/catalog"
	"godb/types"
)

// colMeta 是一列在系统目录里的投影。
type colMeta struct {
	idx      int
	atttypid int32
	attlen   int32
}

// userIndexOIDBase 是用户 CREATE INDEX 的 OID 起点，避开约束索引占用的区间。
const userIndexOIDBase = 60000

// idxMeta 描述一个索引（约束隐式创建的，或用户 CREATE INDEX 建的）。
type idxMeta struct {
	oid     int32
	name    string
	cols    []int // 复合主键会有多列
	colIdx  int   // 首列（只关心一列的旧代码用）
	primary bool
	uniq    bool // 唯一索引（主键/唯一约束/用户 CREATE UNIQUE INDEX）
	conOid  int32
	defOid  int32
}

// tblMeta 是一张表在系统目录里的投影。
type tblMeta struct {
	t    *catalog.Table
	oid  int32
	sys  bool
	cols []colMeta
	idx  []idxMeta
}

// meta 是整个库的目录快照。
type meta struct {
	tables []tblMeta
	byName map[string]*tblMeta
}

func buildMeta(src Source) *meta {
	m := &meta{byName: map[string]*tblMeta{}}
	// 先放系统表（这样 relnamespace=11 的过滤能正常工作）
	once.Do(build)
	for i, sp := range specs {
		t := byKey[sp.schema+"."+sp.name]
		if t == nil {
			continue
		}
		tm := &tblMeta{t: t, oid: int32(sysTableOIDBase + i), sys: true}
		for ci, col := range t.Columns {
			tm.cols = append(tm.cols, colMeta{idx: ci, atttypid: types.OIDOf(col.Type()), attlen: types.TypeLenByName(col.Type().String())})
		}
		m.tables = append(m.tables, *tm)
	}
	for i, t := range src.tables() {
		tm := &tblMeta{t: t, oid: int32(userTableOIDBase + i)}
		for ci, col := range t.Columns {
			tm.cols = append(tm.cols, colMeta{idx: ci, atttypid: types.OIDOf(col.Type()), attlen: types.TypeLenByName(col.Type().String())})
		}
		n := 0
		for _, grp := range t.UniqueGroups() {
			ci := grp[0]
			prim := t.Columns[ci].PrimaryKey
			name := fmt.Sprintf("%s_%s_key", t.Name, t.Columns[ci].Name)
			if prim {
				name = t.Name + "_pkey"
			}
			tm.idx = append(tm.idx, idxMeta{
				oid:     int32(indexOIDBase + n*len(src.tables()) + i),
				name:    name,
				cols:    grp,
				colIdx:  ci,
				primary: prim,
				uniq:    true,
				conOid:  int32(constraintOIDBase + n*len(src.tables()) + i),
				defOid:  int32(attrDefOIDBase + n*len(src.tables()) + i),
			})
			n++
		}
		// 用户 CREATE INDEX 建的索引：只登记与展示，不参与查询规划
		for j, ux := range t.Indexes {
			cols := make([]int, 0, len(ux.Columns))
			for _, cn := range ux.Columns {
				if ci, ok := t.ColumnIdx(cn); ok {
					cols = append(cols, ci)
				}
			}
			if len(cols) == 0 {
				continue
			}
			tm.idx = append(tm.idx, idxMeta{
				oid:     int32(userIndexOIDBase + i*100 + j),
				name:    ux.Name,
				cols:    cols,
				colIdx:  cols[0],
				primary: false,
				uniq:    ux.Unique,
			})
		}
		m.tables = append(m.tables, *tm)
	}
	for i := range m.tables {
		tm := &m.tables[i]
		m.byName[strings.ToLower(tm.t.Name)] = tm
	}
	return m
}

// RelationOID 返回一张表在 pg_class 里的 OID（与虚拟表 pg_class 的输出一致）。
// "xxx"::regclass 要用它：catalog.Table.ID 是数据文件编号，
// 和客户端看到的 OID 不是一回事。
func RelationOID(cat *catalog.Catalog, name string) (int32, bool) {
	if cat == nil {
		return 0, false
	}
	m := buildMeta(Source{Cat: cat})
	tm, ok := m.byName[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return 0, false
	}
	return tm.oid, true
}

// userTables 返回业务表（不含系统表）。
// colType 返回第 idx 列（从 0 开始）的类型 OID，找不到给 0。
func (tm *tblMeta) colType(idx int) int32 {
	for _, c := range tm.cols {
		if c.idx == idx {
			return c.atttypid
		}
	}
	return 0
}

func (m *meta) userTables() []*tblMeta {
	var out []*tblMeta
	for i := range m.tables {
		if !m.tables[i].sys {
			out = append(out, &m.tables[i])
		}
	}
	return out
}

func (m *meta) get(name string) *tblMeta {
	return m.byName[strings.ToLower(name)]
}

// mk 按列名填一行，未出现的列一律 NULL。
func mk(def *catalog.Table, vals map[string]types.Value) types.Row {
	r := make(types.Row, len(def.Columns))
	for i := range r {
		r[i] = types.Null
	}
	for k, v := range vals {
		if i, ok := def.ColumnIdx(k); ok {
			r[i] = v
		}
	}
	return r
}

func iv(n int) types.Value    { return types.Int(int64(n)) }
func i64(n int64) types.Value { return types.Int(n) }
func sv(s string) types.Value { return types.Text(s) }
func bv(b bool) types.Value   { return types.Bool(b) }

// Rows 生成一张虚拟表的全部行。
func Rows(def *catalog.Table, src Source) []types.Row {
	m := buildMeta(src)
	switch def.Name {
	case "pg_database":
		return []types.Row{mk(def, map[string]types.Value{
			"oid": iv(DbOID), "datname": sv(DatabaseName), "datdba": iv(OwnerOID),
			"encoding": iv(6), "datcollate": sv("C"), "datctype": sv("C"),
			"datistemplate": bv(false), "datallowconn": bv(true), "datconnlimit": iv(-1),
			"datlastsysoid": iv(LastSystemOID), "datfrozenxid": iv(0), "datminmxid": iv(1),
			"dattablespace": iv(TablespaceDef),
		})}

	case "pg_namespace":
		return []types.Row{
			mk(def, map[string]types.Value{"oid": iv(NamespaceCatalog), "nspname": sv("pg_catalog"), "nspowner": iv(OwnerOID)}),
			mk(def, map[string]types.Value{"oid": iv(NamespaceInfo), "nspname": sv("information_schema"), "nspowner": iv(OwnerOID)}),
			mk(def, map[string]types.Value{"oid": iv(NamespacePublic), "nspname": sv("public"), "nspowner": iv(OwnerOID)}),
		}

	case "pg_class":
		var out []types.Row
		for _, tm := range m.tables {
			ns := int32(NamespacePublic)
			if tm.sys {
				ns = NamespaceCatalog
			}
			out = append(out, mk(def, map[string]types.Value{
				"oid": iv(int(tm.oid)), "relname": sv(tm.t.Name), "relnamespace": iv(int(ns)),
				"reltype": iv(0), "reloftype": iv(0), "relowner": iv(OwnerOID),
				"relam": iv(2), "relfilenode": iv(int(tm.oid)), "reltablespace": iv(0),
				"relpages": iv(1), "reltuples": types.Float(float64(src.rowsOf(tm.t.Name))),
				"relallvisible": iv(0), "reltoastrelid": iv(0),
				"relhasindex": bv(len(tm.idx) > 0), "relisshared": bv(false),
				"relhasoids":     bv(false),
				"relpersistence": sv("p"), "relkind": sv("r"), "relnatts": iv(len(tm.t.Columns)),
				"relchecks": iv(0), "relhasrules": bv(false), "relhastriggers": bv(false),
				"relhassubclass": bv(false), "relrowsecurity": bv(false),
				"relforcerowsecurity": bv(false), "relispopulated": bv(true),
				"relreplident": sv("d"), "relispartition": bv(false),
				"relfrozenxid": iv(0), "relminmxid": iv(1),
			}))
			// 索引也是 pg_class 里的对象（relkind='i'）
			for _, ix := range tm.idx {
				out = append(out, mk(def, map[string]types.Value{
					"oid": iv(int(ix.oid)), "relname": sv(ix.name), "relnamespace": iv(int(ns)),
					"relowner": iv(OwnerOID), "relam": iv(403), "relfilenode": iv(int(ix.oid)),
					"relpages": iv(1), "reltuples": types.Float(0), "relhasindex": bv(false),
					"relhasoids": bv(false), "relisshared": bv(false),
					"relpersistence": sv("p"), "relkind": sv("i"), "relnatts": iv(1),
					"relispopulated": bv(true), "relreplident": sv("n"),
					"relfrozenxid": iv(0), "relminmxid": iv(1),
				}))
			}
		}
		return out

	case "pg_attribute":
		var out []types.Row
		for _, tm := range m.tables {
			for num, cm := range tm.cols {
				col := tm.t.Columns[cm.idx]
				hasDef := col.Default != "" || col.Serial || col.PrimaryKey && col.Serial
				if col.Serial && col.Default == "" {
					hasDef = true
				}
				out = append(out, mk(def, map[string]types.Value{
					"attrelid": iv(int(tm.oid)), "attname": sv(col.Name),
					"atttypid": iv(int(cm.atttypid)), "attstattarget": iv(-1),
					"attlen": iv(int(cm.attlen)), "attnum": iv(num + 1),
					"attndims": iv(0), "attcacheoff": iv(-1), "atttypmod": iv(typmodOf(col)),
					"attbyval":   bv(byvalOf(col.Type().Name)),
					"attstorage": sv(storageOf(col.Type().Name)),
					"attalign":   sv(alignOf(col.Type().Name)),
					"attnotnull": bv(col.NotNull || col.PrimaryKey), "atthasdef": bv(hasDef),
					"atthasmissing": bv(false), "attidentity": sv(""), "attgenerated": sv(""),
					"attisdropped": bv(false), "attislocal": bv(true), "attinhcount": iv(0),
					"attcollation": iv(attCollationOf(col.Type().Name)),
				}))
			}
		}
		return out

	case "pg_type":
		return typeRows(def)

	case "pg_index":
		var out []types.Row
		for _, tm := range m.tables {
			for _, ix := range tm.idx {
				out = append(out, mk(def, map[string]types.Value{
					"indexrelid": iv(int(ix.oid)), "indrelid": iv(int(tm.oid)),
					"indnatts": iv(1), "indnkeyatts": iv(1),
					"indisunique": bv(ix.uniq), "indisprimary": bv(ix.primary),
					"indisexclusion": bv(false), "indimmediate": bv(true),
					"indisclustered": bv(false), "indisvalid": bv(true),
					"indcheckxmin": bv(false), "indisready": bv(true), "indislive": bv(true),
					"indisreplident": bv(false),
					"indkey":         sv(indexColList(ix)),
					"indcollation":   sv(indexZeros(ix)), "indoption": sv(indexZeros(ix)),
					"indclass": sv(indexOpclasses(&tm, ix)),
				}))
			}
		}
		return out

	case "pg_constraint":
		var out []types.Row
		for _, tm := range m.tables {
			for _, ix := range tm.idx {
				// 用户索引没有对应约束（conOid 为 0），不要凭空造一条
				if ix.conOid == 0 {
					continue
				}
				typ := "u"
				if ix.primary {
					typ = "p"
				}
				out = append(out, mk(def, map[string]types.Value{
					"oid": iv(int(ix.conOid)), "conname": sv(ix.name),
					"connamespace": iv(NamespacePublic), "contype": sv(typ),
					"condeferrable": bv(false), "condeferred": bv(false), "convalidated": bv(true),
					"conrelid": iv(int(tm.oid)), "contypid": iv(0), "conindid": iv(int(ix.oid)),
					"conparentid": iv(0), "confrelid": iv(0),
					"confupdtype": sv(" "), "confdeltype": sv(" "), "confmatchtype": sv(" "),
					"conislocal": bv(true), "coninhcount": iv(0), "connoinherit": bv(false),
					"conkey": sv(constraintColList(ix)),
				}))
			}
			// 外键：contype = 'f'，confrelid 指向父表，conkey/confkey 是两侧列号
			for fi, fk := range tm.t.ForeignKeys {
				parent, ok := m.byName[strings.ToLower(fk.RefTable)]
				if !ok {
					continue
				}
				ckey, err1 := refColNums(tm.t, fk.Columns)
				fkey, err2 := refColNums(parent.t, fk.RefColumns)
				if err1 != nil || err2 != nil {
					continue
				}
				out = append(out, mk(def, map[string]types.Value{
					"oid":          iv(fkOIDBase + int(tm.oid)*10 + fi),
					"conname":      sv(fk.Name),
					"connamespace": iv(NamespacePublic), "contype": sv("f"),
					"condeferrable": bv(false), "condeferred": bv(false), "convalidated": bv(true),
					"conrelid": iv(int(tm.oid)), "contypid": iv(0), "conindid": iv(0),
					"conparentid": iv(0), "confrelid": iv(int(parent.oid)),
					"confupdtype": sv(fkActionCode(fk.OnUpdate)), "confdeltype": sv(fkActionCode(fk.OnDelete)),
					"confmatchtype": sv("s"), "conislocal": bv(true), "coninhcount": iv(0),
					"connoinherit": bv(false), "conkey": sv(ckey), "confkey": sv(fkey),
				}))
			}
		}
		return out

	case "pg_attrdef":
		var out []types.Row
		for _, tm := range m.tables {
			for num, cm := range tm.cols {
				col := tm.t.Columns[cm.idx]
				expr := defaultExpr(tm.t, col)
				if expr == "" {
					continue
				}
				out = append(out, mk(def, map[string]types.Value{
					"oid":     iv(attrDefOIDBase + int(tm.oid)*10 + num),
					"adrelid": iv(int(tm.oid)), "adnum": iv(num + 1), "adbin": sv(expr),
				}))
			}
		}
		return out

	case "pg_am":
		return []types.Row{
			mk(def, map[string]types.Value{"oid": iv(2), "amname": sv("heap"), "amhandler": sv("heap_tableam_handler"), "amtype": sv("t")}),
			mk(def, map[string]types.Value{"oid": iv(403), "amname": sv("btree"), "amhandler": sv("bthandler"), "amtype": sv("i")}),
			mk(def, map[string]types.Value{"oid": iv(405), "amname": sv("hash"), "amhandler": sv("hashhandler"), "amtype": sv("i")}),
		}

	case "pg_opclass":
		// btree 的默认操作符类。客户端「索引」页要拿 indclass 去 join 这张表
		// 显示操作符类；一条都没有的话那个下拉是空的。
		rows := make([]types.Row, 0, len(btreeOpclasses))
		for _, oc := range btreeOpclasses {
			rows = append(rows, mk(def, map[string]types.Value{
				"oid": iv(int(oc.oid)), "opcmethod": iv(BtreeAmOID),
				"opcname": sv(oc.name), "opcnamespace": iv(NamespaceCatalog),
				"opcowner": iv(OwnerOID), "opcfamily": iv(0),
				"opcintype": iv(int(oc.intype)), "opcdefault": bv(true), "opckeytype": iv(0),
			}))
		}
		return rows

	case "pg_tablespace":
		return []types.Row{
			mk(def, map[string]types.Value{"oid": iv(TablespaceDef), "spcname": sv("pg_default"), "spcowner": iv(OwnerOID)}),
			mk(def, map[string]types.Value{"oid": iv(TablespaceGlobal), "spcname": sv("pg_global"), "spcowner": iv(OwnerOID)}),
		}

	case "pg_roles", "pg_authid":
		return []types.Row{mk(def, map[string]types.Value{
			"oid": iv(OwnerOID), "rolname": sv(OwnerName), "rolsuper": bv(true),
			"rolinherit": bv(true), "rolcreaterole": bv(true), "rolcreatedb": bv(true),
			"rolcanlogin": bv(true), "rolreplication": bv(false), "rolbypassrls": bv(false),
			"rolconnlimit": iv(-1), "rolpassword": sv("********"),
		})}

	case "pg_user", "pg_shadow":
		return []types.Row{mk(def, map[string]types.Value{
			"usename": sv(OwnerName), "usesysid": iv(OwnerOID), "usecreatedb": bv(true),
			"usesuper": bv(true), "userepl": bv(false), "usebypassrls": bv(false),
			"passwd": sv("********"),
		})}

	case "pg_group":
		return nil

	case "pg_settings":
		return settingRows(def)

	case "pg_description", "pg_shdescription":
		return []types.Row{mk(def, map[string]types.Value{
			"objoid": iv(DbOID), "classoid": iv(1262), "objsubid": iv(0),
			"description": sv("godb - a from-scratch SQL database in Go"),
		})}

	case "pg_collation":
		return []types.Row{
			mk(def, map[string]types.Value{"oid": iv(100), "collname": sv("default"), "collnamespace": iv(NamespaceCatalog), "collowner": iv(OwnerOID), "collprovider": sv("d"), "collisdeterministic": bv(true), "collencoding": iv(-1)}),
			mk(def, map[string]types.Value{"oid": iv(950), "collname": sv("C"), "collnamespace": iv(NamespaceCatalog), "collowner": iv(OwnerOID), "collprovider": sv("c"), "collisdeterministic": bv(true), "collencoding": iv(-1), "collcollate": sv("C"), "collctype": sv("C")}),
			mk(def, map[string]types.Value{"oid": iv(951), "collname": sv("POSIX"), "collnamespace": iv(NamespaceCatalog), "collowner": iv(OwnerOID), "collprovider": sv("c"), "collisdeterministic": bv(true), "collencoding": iv(-1), "collcollate": sv("POSIX"), "collctype": sv("POSIX")}),
		}

	case "pg_proc":
		return procRows(def)

	case "pg_language":
		return []types.Row{
			mk(def, map[string]types.Value{"oid": iv(12), "lanname": sv("internal"), "lanispl": bv(false), "lanpltrusted": bv(false), "lanplcallfoid": iv(0), "laninline": iv(0), "lanvalidator": iv(2246)}),
			mk(def, map[string]types.Value{"oid": iv(13), "lanname": sv("c"), "lanispl": bv(false), "lanpltrusted": bv(false), "lanplcallfoid": iv(0), "laninline": iv(0), "lanvalidator": iv(2247)}),
			mk(def, map[string]types.Value{"oid": iv(14), "lanname": sv("sql"), "lanispl": bv(true), "lanpltrusted": bv(true), "lanplcallfoid": iv(0), "laninline": iv(0), "lanvalidator": iv(2248)}),
		}

	// ---------- 视图 ----------
	case "pg_tables":
		var out []types.Row
		for _, tm := range m.userTables() {
			out = append(out, mk(def, map[string]types.Value{
				"schemaname": sv("public"), "tablename": sv(tm.t.Name), "tableowner": sv(OwnerName),
				"tablespace": types.Null, "hasindexes": bv(len(tm.idx) > 0),
				"hasrules": bv(false), "hastriggers": bv(false), "rowsecurity": bv(false),
			}))
		}
		return out

	case "pg_indexes":
		var out []types.Row
		for _, tm := range m.userTables() {
			for _, ix := range tm.idx {
				out = append(out, mk(def, map[string]types.Value{
					"schemaname": sv("public"), "tablename": sv(tm.t.Name),
					"indexname": sv(ix.name), "tablespace": types.Null,
					"indexdef": sv(indexDef(tm, ix)),
				}))
			}
		}
		return out

	case "pg_sequences", "information_schema.sequences":
		var out []types.Row
		for _, tm := range m.userTables() {
			for _, col := range tm.t.Columns {
				if !col.Serial {
					continue
				}
				seq := seqName(tm.t, col)
				if def.Name == "pg_sequences" {
					out = append(out, mk(def, map[string]types.Value{
						"schemaname": sv("public"), "sequencename": sv(seq), "sequenceowner": sv(OwnerName),
						"data_type": sv("bigint"), "start_value": i64(1), "min_value": i64(1),
						"max_value": i64(9223372036854775807), "increment_by": i64(1),
						"cycle": bv(false), "cache_size": i64(1), "last_value": i64(src.rowsOf(tm.t.Name)),
					}))
				} else {
					out = append(out, mk(def, map[string]types.Value{
						"sequence_catalog": sv(DatabaseName), "sequence_schema": sv("public"),
						"sequence_name": sv(seq), "data_type": sv("bigint"),
						"numeric_precision": iv(64), "numeric_precision_radix": iv(2),
						"numeric_scale": iv(0), "start_value": sv("1"),
						"minimum_value": sv("1"), "maximum_value": sv("9223372036854775807"),
						"increment": sv("1"), "cycle_option": sv("NO"),
					}))
				}
			}
		}
		return out

	case "pg_operator":
		// 常用类型的比较操作符（OID 与 oprcode 都取自真实 PostgreSQL）。
		// 客户端的索引页/操作符页会拿它做 join，空表时那些面板是空的。
		rows := make([]types.Row, 0, len(comparisonOperators))
		for _, op := range comparisonOperators {
			rows = append(rows, mk(def, map[string]types.Value{
				"oid": iv(op.oid), "oprname": sv(op.name), "oprnamespace": iv(NamespaceCatalog),
				"oprowner": iv(OwnerOID), "oprkind": sv("b"), "oprcanmerge": bv(false),
				"oprcanhash": bv(false), "oprleft": iv(op.typ), "oprright": iv(op.typ),
				"oprresult": iv(16), "oprcom": iv(0), "oprnegate": iv(0),
				"oprcode": sv(op.code), "oprrest": sv("-"), "oprjoin": sv("-"),
			}))
		}
		return rows

	case "pg_stat_activity":
		// 真实连接列表（协议层登记，见 activity.go）。没有连接时返回空集，
		// 和 PostgreSQL 一致：它不给自己伪造一行。
		snap := ActivitySnapshot()
		out := make([]types.Row, 0, len(snap))
		for _, a := range snap {
			usr := a.User
			if usr == "" {
				usr = OwnerName
			}
			state := a.State
			if state == "" {
				state = "idle"
			}
			out = append(out, mk(def, map[string]types.Value{
				"datid": iv(DbOID), "datname": sv(DatabaseName), "pid": iv(int(a.PID)),
				"usesysid": iv(OwnerOID), "usename": sv(usr),
				"application_name": sv(a.AppName), "client_addr": sv(a.Addr),
				"client_port": iv(int(a.Port)), "backend_start": types.Time(a.Started),
				"query_start": types.Time(a.QueryTime), "state": sv(state),
				"query": sv(a.Query), "backend_type": sv("client backend"),
			}))
		}
		return out

	case "pg_stat_database":
		return []types.Row{mk(def, map[string]types.Value{
			"datid": iv(DbOID), "datname": sv(DatabaseName), "numbackends": iv(1),
		})}

	case "pg_stat_user_tables", "pg_stat_all_tables":
		var out []types.Row
		for _, tm := range m.userTables() {
			out = append(out, mk(def, map[string]types.Value{
				"relid": iv(int(tm.oid)), "schemaname": sv("public"), "relname": sv(tm.t.Name),
				"n_live_tup": i64(src.rowsOf(tm.t.Name)), "n_dead_tup": i64(0),
			}))
		}
		return out

	case "pg_stat_user_indexes":
		var out []types.Row
		for _, tm := range m.userTables() {
			for _, ix := range tm.idx {
				out = append(out, mk(def, map[string]types.Value{
					"relid": iv(int(tm.oid)), "indexrelid": iv(int(ix.oid)),
					"schemaname": sv("public"), "relname": sv(tm.t.Name),
					"indexrelname": sv(ix.name),
				}))
			}
		}
		return out

	case "pg_available_extensions":
		return []types.Row{
			mk(def, map[string]types.Value{"name": sv("plpgsql"), "default_version": sv("1.0"), "installed_version": sv("1.0"), "comment": sv("PL/pgSQL procedural language")}),
		}

	case "pg_timezone_names":
		var out []types.Row
		for _, z := range []struct{ name, abbrev, offset string }{
			{"UTC", "UTC", "00:00:00"},
			{"Asia/Shanghai", "CST", "08:00:00"},
			{"Asia/Tokyo", "JST", "09:00:00"},
			{"Europe/London", "GMT", "00:00:00"},
			{"America/New_York", "EST", "-05:00:00"},
		} {
			out = append(out, mk(def, map[string]types.Value{
				"name": sv(z.name), "abbrev": sv(z.abbrev),
				"utc_offset": sv(z.offset), "is_dst": bv(false),
			}))
		}
		return out

	// ---------- information_schema ----------
	case "schemata":
		var out []types.Row
		for _, ns := range []string{"pg_catalog", "information_schema", "public"} {
			out = append(out, mk(def, map[string]types.Value{
				"catalog_name": sv(DatabaseName), "schema_name": sv(ns), "schema_owner": sv(OwnerName),
			}))
		}
		return out

	case "tables":
		var out []types.Row
		for _, tm := range m.userTables() {
			out = append(out, mk(def, map[string]types.Value{
				"table_catalog": sv(DatabaseName), "table_schema": sv("public"),
				"table_name": sv(tm.t.Name), "table_type": sv("BASE TABLE"),
				"is_insertable_into": sv("YES"), "is_typed": sv("NO"),
			}))
		}
		return out

	case "columns":
		var out []types.Row
		for _, tm := range m.userTables() {
			for num, col := range tm.t.Columns {
				collSchema, collName := types.Null, types.Null
				if collatable(col.Type().Name) {
					collSchema, collName = sv("pg_catalog"), sv("default")
				}
				out = append(out, mk(def, map[string]types.Value{
					"table_catalog": sv(DatabaseName), "table_schema": sv("public"),
					"table_name": sv(tm.t.Name), "column_name": sv(col.Name),
					"ordinal_position":         iv(num + 1),
					"column_default":           nullableText(defaultExpr(tm.t, col)),
					"is_nullable":              yesNo(!(col.NotNull || col.PrimaryKey)),
					"data_type":                sv(infoDataType(col.Type())),
					"character_maximum_length": lenOrNull(col),
					"character_octet_length":   octetLenOrNull(col),
					"numeric_precision":        numericPrecOrNull(col),
					"numeric_precision_radix":  numericRadixOrNull(col),
					"numeric_scale":            numericScaleOrNull(col),
					"datetime_precision":       datetimePrecOrNull(col),
					"collation_catalog":        types.Null,
					"collation_schema":         collSchema,
					"collation_name":           collName,
					"udt_catalog":              sv(DatabaseName),
					"udt_schema":               sv("pg_catalog"),
					"udt_name":                 sv(udtName(col.Type())),
					"dtd_identifier":           sv(fmt.Sprintf("%d", num+1)),
					"is_identity":              sv(identityOf(col)),
					"is_generated":             sv("NEVER"),
					"is_updatable":             sv("YES"),
				}))
			}
		}
		return out

	case "table_constraints":
		var out []types.Row
		for _, tm := range m.userTables() {
			for _, ix := range tm.idx {
				typ := "UNIQUE"
				if ix.primary {
					typ = "PRIMARY KEY"
				}
				out = append(out, mk(def, map[string]types.Value{
					"constraint_catalog": sv(DatabaseName), "constraint_schema": sv("public"),
					"constraint_name": sv(ix.name), "table_catalog": sv(DatabaseName),
					"table_schema": sv("public"), "table_name": sv(tm.t.Name),
					"constraint_type": sv(typ), "is_deferrable": sv("NO"),
					"initially_deferred": sv("NO"), "enforced": sv("YES"),
				}))
			}
		}
		return out

	case "key_column_usage", "constraint_column_usage":
		var out []types.Row
		for _, tm := range m.userTables() {
			for _, ix := range tm.idx {
				col := tm.t.Columns[ix.colIdx]
				if def.Name == "key_column_usage" {
					// 复合主键的每一列各占一行，ordinal_position 是它在键里的次序
					for ord, ci := range ix.cols {
						out = append(out, mk(def, map[string]types.Value{
							"constraint_catalog": sv(DatabaseName), "constraint_schema": sv("public"),
							"constraint_name": sv(ix.name), "table_catalog": sv(DatabaseName),
							"table_schema": sv("public"), "table_name": sv(tm.t.Name),
							"column_name": sv(tm.t.Columns[ci].Name), "ordinal_position": iv(ord + 1),
						}))
					}
				} else {
					out = append(out, mk(def, map[string]types.Value{
						"table_catalog": sv(DatabaseName), "table_schema": sv("public"),
						"table_name": sv(tm.t.Name), "column_name": sv(col.Name),
						"constraint_catalog": sv(DatabaseName), "constraint_schema": sv("public"),
						"constraint_name": sv(ix.name),
					}))
				}
			}
		}
		return out

	case "character_sets":
		return []types.Row{mk(def, map[string]types.Value{
			"character_set_catalog": sv(DatabaseName), "character_set_schema": sv("pg_catalog"),
			"character_set_name": sv("UTF8"), "character_repertoire": sv("UCS"),
			"form_of_use": sv("UTF8"),
		})}

	case "collations":
		return []types.Row{
			mk(def, map[string]types.Value{"collation_catalog": sv(DatabaseName), "collation_schema": sv("pg_catalog"), "collation_name": sv("default"), "pad_attribute": sv("NO PAD")}),
			mk(def, map[string]types.Value{"collation_catalog": sv(DatabaseName), "collation_schema": sv("pg_catalog"), "collation_name": sv("C"), "pad_attribute": sv("NO PAD")}),
		}
	}
	return nil
}

// ---------- 类型表 ----------

type typeDef struct {
	name  string
	oid   int32
	typ   string // 内部类型名
	categ string
	lem   int32
	array int32
	align string
	store string
}

var builtinTypes = []typeDef{
	{"bool", types.OIDBool, "bool", "B", 0, 1000, "c", "p"},
	{"bytea", types.OIDBytea, "bytea", "U", 0, 1001, "i", "x"},
	{"char", types.OIDChar, `"char"`, "Z", 0, 1002, "c", "p"},
	{"name", types.OIDName, "name", "S", 0, 1003, "c", "p"},
	{"int8", types.OIDInt8, "int8", "N", 0, 1016, "d", "p"},
	{"int2", types.OIDInt2, "int2", "N", 0, 1005, "s", "p"},
	{"int2vector", types.OIDInt2Vector, "int2vector", "A", 21, 1006, "i", "p"},
	{"int4", types.OIDInt4, "int4", "N", 0, 1007, "i", "p"},
	{"regproc", types.OIDRegproc, "regproc", "N", 0, 1008, "i", "p"},
	{"text", types.OIDText, "text", "S", 0, 1009, "i", "x"},
	{"oid", types.OIDOid, "oid", "N", 0, 1028, "i", "p"},
	{"tid", types.OIDTid, "tid", "U", 0, 1010, "s", "p"},
	{"xid", types.OIDXid, "xid", "U", 0, 1011, "i", "p"},
	{"cid", types.OIDCid, "cid", "U", 0, 1012, "i", "p"},
	{"oidvector", types.OIDOidVector, "oidvector", "A", 26, 1013, "i", "p"},
	{"json", types.OIDJson, "json", "U", 0, 199, "i", "x"},
	{"xml", types.OIDXml, "xml", "U", 0, 143, "i", "x"},
	{"point", types.OIDPoint, "point", "G", 701, 1017, "d", "p"},
	{"float4", types.OIDFloat4, "float4", "N", 0, 1021, "i", "p"},
	{"float8", types.OIDFloat8, "float8", "N", 0, 1022, "d", "p"},
	{"unknown", types.OIDUnknown, "unknown", "X", 0, 0, "c", "p"},
	{"money", types.OIDMoney, "money", "N", 0, 791, "d", "p"},
	{"macaddr", types.OIDMacaddr, "macaddr", "U", 0, 1040, "i", "p"},
	{"inet", types.OIDInet, "inet", "I", 0, 1041, "i", "x"},
	{"cidr", types.OIDCidr, "cidr", "I", 0, 651, "i", "x"},
	{"bpchar", types.OIDBpchar, "bpchar", "S", 0, 1014, "i", "x"},
	{"varchar", types.OIDVarchar, "varchar", "S", 0, 1015, "i", "x"},
	{"date", types.OIDDate, "date", "D", 0, 1182, "i", "p"},
	{"time", types.OIDTime, "time", "D", 0, 1183, "d", "p"},
	{"timestamp", types.OIDTimestamp, "timestamp", "D", 0, 1115, "d", "p"},
	{"timestamptz", types.OIDTimestamptz, "timestamptz", "D", 0, 1185, "d", "p"},
	{"interval", types.OIDInterval, "interval", "T", 0, 1187, "d", "p"},
	{"timetz", types.OIDTimetz, "timetz", "D", 0, 1270, "d", "p"},
	{"bit", types.OIDBit, "bit", "V", 0, 1561, "i", "x"},
	{"varbit", types.OIDVarbit, "varbit", "V", 0, 1563, "i", "x"},
	{"numeric", types.OIDNumeric, "numeric", "N", 0, 1231, "i", "m"},
	{"uuid", types.OIDUuid, "uuid", "U", 0, 2951, "c", "p"},
	{"jsonb", types.OIDJsonb, "jsonb", "U", 0, 3807, "i", "x"},
	{"record", types.OIDRecord, "record", "P", 0, 2287, "d", "x"},
	{"regclass", types.OIDRegclass, "regclass", "N", 0, 2210, "i", "p"},
	{"regtype", types.OIDRegtype, "regtype", "N", 0, 2211, "i", "p"},
	{"anyarray", types.OIDAnyArray, "anyarray", "P", 0, 0, "d", "x"},
	{"anyelement", types.OIDAnyElement, "anyelement", "P", 0, 0, "d", "x"},
	{"aclitem", types.OIDAclitem, "aclitem", "U", 0, 1034, "i", "p"},
	{"pg_node_tree", types.OIDNodeTree, "pg_node_tree", "S", 0, 0, "i", "x"},
	{"tsvector", types.OIDTsvector, "tsvector", "U", 0, 3643, "i", "x"},
	{"tsquery", types.OIDTsquery, "tsquery", "U", 0, 3645, "i", "x"},
}

// typeRows 生成 pg_type。数组类型不单独列，按基类型的 typarray 全自动生成：
// 名字是 "_"+基名（真 PG 的写法是 _int4 / _text，不是 int4_array），
// 这样 typarray 指过去的 OID 一定存在，客户端反查数组元素类型才不会落空。
func typeRows(def *catalog.Table) []types.Row {
	out := make([]types.Row, 0, len(builtinTypes)*2)
	for _, td := range builtinTypes {
		out = append(out, mk(def, map[string]types.Value{
			"oid": iv(int(td.oid)), "typname": sv(td.name), "typnamespace": iv(NamespaceCatalog),
			"typowner": iv(OwnerOID), "typlen": iv(int(types.TypeLenByName(td.typ))),
			"typbyval": bv(byvalOf(td.typ)),
			"typtype":  sv("b"), "typcategory": sv(td.categ),
			"typispreferred": bv(td.name == "text" || td.name == "int4" || td.name == "float8"),
			"typisdefined":   bv(true), "typdelim": sv(","), "typrelid": iv(0),
			"typelem": iv(int(td.lem)), "typarray": iv(int(td.array)),
			"typinput": sv(td.name + "in"), "typoutput": sv(td.name + "out"),
			"typreceive": sv(td.name + "recv"), "typsend": sv(td.name + "send"),
			"typalign": sv(td.align), "typstorage": sv(td.store),
			"typnotnull": bv(false), "typbasetype": iv(0), "typtypmod": iv(-1),
			"typndims": iv(0), "typcollation": iv(attCollationOf(td.typ)),
		}))
		if td.array == 0 {
			continue
		}
		arr := "_" + td.typ
		out = append(out, mk(def, map[string]types.Value{
			"oid": iv(int(td.array)), "typname": sv(arr), "typnamespace": iv(NamespaceCatalog),
			"typowner": iv(OwnerOID), "typlen": iv(-1),
			"typbyval": bv(false), "typtype": sv("b"), "typcategory": sv("A"),
			"typispreferred": bv(false), "typisdefined": bv(true), "typdelim": sv(","),
			"typrelid": iv(0), "typelem": iv(int(td.oid)), "typarray": iv(0),
			"typinput": sv("array_in"), "typoutput": sv("array_out"),
			"typreceive": sv("array_recv"), "typsend": sv("array_send"),
			"typalign": sv("i"), "typstorage": sv("x"),
			"typnotnull": bv(false), "typbasetype": iv(0), "typtypmod": iv(-1),
			"typndims": iv(0), "typcollation": iv(0),
		}))
	}
	return out
}

// procRows 只列出客户端会探测的几个函数。
type procRow struct {
	oid     int32
	name    string
	nargs   int
	rettype int32
}

var builtinProcs = []procRow{
	{1247, "boolin", 1, 16}, {1248, "boolout", 1, 2275},
	{1250, "int4in", 1, 23}, {1251, "int4out", 1, 2275},
	{1312, "version", 0, 25}, {1313, "current_schema", 0, 19},
	{1314, "current_database", 0, 19}, {1315, "current_user", 0, 19},
	{1685, "format_type", 2, 25}, {1717, "pg_get_expr", 3, 25},
	{1703, "pg_get_serial_sequence", 2, 25}, {1641, "pg_get_userbyid", 1, 19},
	{1642, "pg_get_indexdef", 3, 25}, {1643, "pg_get_triggerdef", 2, 25},
	{1644, "pg_get_constraintdef", 2, 25}, {2236, "pg_get_viewdef", 2, 25},
	{1693, "pg_encoding_to_char", 1, 19}, {1701, "pg_recvlogical", 0, 25},
}

func procRows(def *catalog.Table) []types.Row {
	out := make([]types.Row, 0, len(builtinProcs))
	for _, p := range builtinProcs {
		out = append(out, mk(def, map[string]types.Value{
			"oid": iv(int(p.oid)), "proname": sv(p.name), "pronamespace": iv(NamespaceCatalog),
			"proowner": iv(OwnerOID), "prolang": iv(12), "procost": types.Float(1),
			"prorows": types.Float(0), "prokind": sv("f"),
			"proisagg": bv(false), "proiswindow": bv(false), "prosecdef": bv(false),
			"proleakproof": bv(false), "proisstrict": bv(true), "proretset": bv(false),
			"provolatile": sv("s"), "proparallel": sv("s"), "pronargs": iv(p.nargs),
			"pronargdefaults": iv(0), "prorettype": iv(int(p.rettype)),
			"proargtypes": sv(""), "prosrc": sv(p.name), "probin": types.Null,
		}))
	}
	return out
}

func settingRows(def *catalog.Table) []types.Row {
	pairs := [][2]string{
		{"application_name", ""}, {"backslash_quote", "safe_encoding"},
		{"bytea_output", "hex"}, {"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"}, {"default_transaction_isolation", "read committed"},
		{"extra_float_digits", "1"}, {"integer_datetimes", "on"},
		{"IntervalStyle", "postgres"}, {"lc_collate", "C"}, {"lc_ctype", "C"},
		{"lc_monetary", "C"}, {"lc_numeric", "C"}, {"lc_time", "C"},
		{"max_connections", "100"}, {"max_index_keys", "32"},
		// Navicat 打开「设计表」时会问 block_size（用来算表和列的存储），
		// 少了它客户端会拿不到块大小，做不了容量估算。
		{"block_size", "8192"}, {"segment_size", "131072"},
		{"wal_block_size", "8192"}, {"wal_segment_size", "16MB"},
		{"max_function_args", "100"}, {"max_identifier_length", "63"},
		{"max_index_keys", "32"}, {"data_checksums", "off"},
		{"quote_all_identifiers", "off"}, {"search_path", `"$user", public`},
		{"server_encoding", "UTF8"}, {"server_version", ServerVersion},
		{"server_version_num", ServerVersionNum}, {"session_authorization", OwnerName},
		{"standard_conforming_strings", "on"}, {"TimeZone", "UTC"},
		{"TimeZone_abbreviations", "Default"}, {"transaction_isolation", "serializable"},
		{"is_superuser", "on"}, {"escape_string_warning", "on"},
	}
	out := make([]types.Row, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, mk(def, map[string]types.Value{
			"name": sv(p[0]), "setting": sv(p[1]), "category": sv("Client Connection Defaults"),
			"short_desc": sv(""), "context": sv("user"), "vartype": sv("string"),
			"source": sv("default"), "boot_val": sv(p[1]), "reset_val": sv(p[1]),
			"pending_restart": bv(false),
		}))
	}
	return out
}

// ---------- 辅助 ----------

func seqName(t *catalog.Table, col *catalog.Column) string {
	return t.Name + "_" + col.Name + "_seq"
}

// defaultExpr 返回列的默认值表达式（PG 的 adbin 文本形式）。
func defaultExpr(t *catalog.Table, col *catalog.Column) string {
	if col.Serial {
		return fmt.Sprintf("nextval('%s'::regclass)", seqName(t, col))
	}
	return col.Default
}

// typmodOf 把字符长度/数值精度编码成 PG 的 atttypmod。
func typmodOf(col *catalog.Column) int {
	t := col.Type()
	switch t.Kind {
	case types.KText:
		if t.Len > 0 {
			return t.Len + 4
		}
	case types.KFloat:
		if t.Prec > 0 {
			return (t.Prec << 16) | t.Scale + 4
		}
	}
	return -1
}

func indexDef(tm *tblMeta, ix idxMeta) string {
	names := make([]string, 0, len(ix.cols))
	for _, ci := range ix.cols {
		names = append(names, tm.t.Columns[ci].Name)
	}
	return fmt.Sprintf("CREATE UNIQUE INDEX %s ON public.%s USING btree (%s)",
		ix.name, tm.t.Name, strings.Join(names, ", "))
}

// indexColList 生成 indkey：列号空格分隔（int2vector 的文本形式，不带括号）。
func indexColList(ix idxMeta) string {
	parts := make([]string, len(ix.cols))
	for i, ci := range ix.cols {
		parts[i] = fmt.Sprintf("%d", ci+1)
	}
	return strings.Join(parts, " ")
}

// indexZeros 生成 indcollation / indoption：每列一个 0，空格分隔。
func indexZeros(ix idxMeta) string {
	parts := make([]string, len(ix.cols))
	for i := range ix.cols {
		parts[i] = "0"
	}
	return strings.Join(parts, " ")
}

// indexOpclasses 生成 indclass：每列的操作符类 oid，空格分隔。
func indexOpclasses(tm *tblMeta, ix idxMeta) string {
	parts := make([]string, len(ix.cols))
	for i, ci := range ix.cols {
		parts[i] = fmt.Sprintf("%d", opclassOf(tm.colType(ci)))
	}
	return strings.Join(parts, " ")
}

// constraintColList 生成 conkey：带花括号的 int2[] 字面量。
func constraintColList(ix idxMeta) string {
	parts := make([]string, len(ix.cols))
	for i, ci := range ix.cols {
		parts[i] = fmt.Sprintf("%d", ci+1)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func nullableText(s string) types.Value {
	if s == "" {
		return types.Null
	}
	return sv(s)
}

func yesNo(b bool) types.Value {
	if b {
		return sv("YES")
	}
	return sv("NO")
}

func lenOrNull(col *catalog.Column) types.Value {
	if col.Type().Len > 0 {
		return iv(col.Type().Len)
	}
	return types.Null
}

// octetLenOrNull 返回 character_octet_length。PG 里 text / 不限定长度的
// varchar 给的是 1073741824（1GB 上限），限定长度的给 len。
func octetLenOrNull(col *catalog.Column) types.Value {
	t := col.Type()
	switch baseTypeName(t.Name) {
	case "text", "character varying", "varchar", "name":
		if t.Len > 0 {
			return iv(t.Len)
		}
		return iv(1073741824)
	case "character", "char", "bpchar":
		if t.Len > 0 {
			return iv(t.Len)
		}
		return iv(1)
	}
	return types.Null
}

// numericPrecOrNull 返回 numeric_precision。必须和真 PG 一致：整数是位宽
// （int2=16 / int4=32 / int8=64），float4=24、float8=53，numeric 用声明的精度。
// 客户端（Navicat 的设计表）会拿这个值反推列类型，缺了会被当成未知类型。
func numericPrecOrNull(col *catalog.Column) types.Value {
	t := col.Type()
	switch baseTypeName(t.Name) {
	case "smallint", "int2":
		return iv(16)
	case "integer", "int", "int4", "serial", "serial4":
		return iv(32)
	case "bigint", "int8", "bigserial", "serial8":
		return iv(64)
	case "real", "float4":
		return iv(24)
	case "double precision", "float8", "double":
		return iv(53)
	case "numeric", "decimal":
		if t.Prec > 0 {
			return iv(t.Prec)
		}
	}
	return types.Null
}

// numericScaleOrNull 返回 numeric_scale。整数固定 0，浮点是 NULL，numeric
// 用声明的位数（没写修饰符时是 NULL）。
func numericScaleOrNull(col *catalog.Column) types.Value {
	t := col.Type()
	switch baseTypeName(t.Name) {
	case "smallint", "int2", "integer", "int", "int4", "serial", "serial4",
		"bigint", "int8", "bigserial", "serial8":
		return iv(0)
	case "numeric", "decimal":
		if t.Prec > 0 {
			return iv(t.Scale)
		}
	}
	return types.Null
}

// numericRadixOrNull 返回 numeric_precision_radix：整数/浮点按 2，numeric 按 10。
func numericRadixOrNull(col *catalog.Column) types.Value {
	switch baseTypeName(col.Type().Name) {
	case "smallint", "int2", "integer", "int", "int4", "serial", "serial4",
		"bigint", "int8", "bigserial", "serial8", "real", "float4",
		"double precision", "float8", "double":
		return iv(2)
	case "numeric", "decimal":
		return iv(10)
	}
	return types.Null
}

func datetimePrecOrNull(col *catalog.Column) types.Value {
	switch baseTypeName(col.Type().Name) {
	case "timestamp", "timestamp without time zone", "timestamptz",
		"timestamp with time zone", "time", "time without time zone",
		"timetz", "time with time zone":
		return iv(6)
	}
	return types.Null
}

// collatable 判断类型是否可排序（PG 里这些列的 attcollation 是 default 排序规则 100）。
func collatable(name string) bool {
	switch baseTypeName(name) {
	case "text", "character varying", "varchar", "character", "char", "bpchar":
		return true
	}
	return false
}

// attCollationOf 返回 pg_attribute.attcollation。文本类类型挂默认排序规则
// （OID 100），其它是 0。真 PG 里 text 列的排序规则不是空，客户端的
// 「设计表 → 排序规则」下拉就是从这里取的。
func attCollationOf(name string) int {
	if collatable(name) {
		return 100
	}
	return 0
}

// storageOf 返回 attstorage：p=plain（定长按值），x=extended（可压缩可外存），
// m=main（可压缩）。Navicat 等客户端会用它做列的存储估计。
func storageOf(name string) string {
	switch baseTypeName(name) {
	case "numeric", "decimal":
		return "m"
	case "text", "character varying", "varchar", "json", "jsonb", "bytea",
		"xml", "tsvector", "tsquery", "varbit", "bit":
		return "x"
	}
	return "p"
}

// alignOf 返回 attalign：1 字节按 c，2 字节按 s，4 字节按 i，8 字节按 d。
func alignOf(name string) string {
	switch baseTypeName(name) {
	case "boolean", "bool", "char", "character", "bpchar", "name", "uuid":
		return "c"
	case "smallint", "int2":
		return "s"
	case "integer", "int", "int4", "serial", "serial4", "real", "float4",
		"numeric", "decimal", "date", "oid", "text", "character varying",
		"varchar", "json", "jsonb", "bytea", "xml", "inet", "cidr",
		"macaddr", "bit", "varbit":
		return "i"
	}
	return "d"
}

// byvalOf 返回 attbyval：定长且长度 ≤ 8 的数值/日期类型按值传递，
// timestamp / interval / text 这些虽也是 8 字节但按引用传。
func byvalOf(name string) bool {
	switch baseTypeName(name) {
	case "boolean", "bool", "smallint", "int2", "integer", "int", "int4",
		"serial", "serial4", "bigint", "int8", "bigserial", "serial8",
		"real", "float4", "double precision", "float8", "double",
		"date", "time", "time without time zone", "timetz",
		"time with time zone", "oid":
		return true
	}
	return false
}

func identityOf(col *catalog.Column) string {
	if col.Serial {
		return "YES"
	}
	return "NO"
}

// baseTypeName 去掉类型修饰符，只留基名：numeric(12,2) → numeric，
// varchar(255) → varchar。
//
// 这一步不能省：catalog 里记的是用户写的完整写法（numeric(12,2)），而
// information_schema 的 data_type / udt_name 要的是基名。带括号时下面的
// switch 一个都匹配不上，numeric 列会被兜底成 double precision / float8，
// 客户端「设计表」拿到的类型自相矛盾（atttypid 是 numeric，data_type 说
// double precision），Navicat 就直接把列类型降级成 (Type) 对象类型。
func baseTypeName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// infoDataType 把内部类型映射成 information_schema.data_type 的写法。
// infoDataType 返回 information_schema.columns.data_type。
//
// 必须优先看列自己记录的类型名（t.Name），不能只按 Kind 猜：内部的整数
// 统一是 int64，但用户建表写的是 int4，客户端「设计表」看到 bigint 会显示错，
// 改表时还可能把类型改成 bigint。
func infoDataType(t types.Type) string {
	switch baseTypeName(t.Name) {
	case "smallint", "int2":
		return "smallint"
	case "integer", "int", "int4", "serial", "serial4":
		return "integer"
	case "bigint", "int8", "bigserial", "serial8":
		return "bigint"
	case "real", "float4":
		return "real"
	case "double precision", "float8", "double":
		return "double precision"
	case "numeric", "decimal":
		return "numeric"
	case "boolean", "bool":
		return "boolean"
	case "date":
		return "date"
	case "timestamp", "timestamp without time zone":
		return "timestamp without time zone"
	case "timestamptz", "timestamp with time zone":
		return "timestamp with time zone"
	case "time", "time without time zone":
		return "time without time zone"
	case "timetz", "time with time zone":
		return "time with time zone"
	case "interval":
		return "interval"
	case "char", "character", "bpchar":
		return "character"
	case "varchar", "character varying":
		return "character varying"
	case "json":
		return "json"
	case "jsonb":
		return "jsonb"
	case "uuid":
		return "uuid"
	case "bytea":
		return "bytea"
	case "money":
		return "money"
	case "inet":
		return "inet"
	case "cidr":
		return "cidr"
	case "macaddr":
		return "macaddr"
	case "xml":
		return "xml"
	case "point":
		return "point"
	case "bit":
		return "bit"
	case "varbit":
		return "bit varying"
	}
	// 兜底：只有拿不到具体名字时才按 Kind 猜
	switch t.Kind {
	case types.KInt:
		return "integer"
	case types.KFloat:
		return "double precision"
	case types.KBool:
		return "boolean"
	case types.KTime:
		return "timestamp without time zone"
	}
	return "text"
}

// udtName 映射成 information_schema.udt_name（PG 内部类型名）。
func udtName(t types.Type) string {
	switch baseTypeName(t.Name) {
	case "smallint", "int2":
		return "int2"
	case "integer", "int", "int4", "serial", "serial4":
		return "int4"
	case "bigint", "int8", "bigserial", "serial8":
		return "int8"
	case "real", "float4":
		return "float4"
	case "double precision", "float8", "double":
		return "float8"
	case "numeric", "decimal":
		return "numeric"
	case "boolean", "bool":
		return "bool"
	case "date":
		return "date"
	case "timestamp", "timestamp without time zone":
		return "timestamp"
	case "timestamptz", "timestamp with time zone":
		return "timestamptz"
	case "time", "time without time zone":
		return "time"
	case "timetz", "time with time zone":
		return "timetz"
	case "interval":
		return "interval"
	case "char", "character", "bpchar":
		return "bpchar"
	case "varchar", "character varying":
		return "varchar"
	case "json":
		return "json"
	case "jsonb":
		return "jsonb"
	case "uuid":
		return "uuid"
	case "bytea":
		return "bytea"
	case "money":
		return "money"
	case "inet":
		return "inet"
	case "cidr":
		return "cidr"
	case "macaddr":
		return "macaddr"
	case "xml":
		return "xml"
	case "point":
		return "point"
	case "bit":
		return "bit"
	case "varbit":
		return "varbit"
	}
	switch t.Kind {
	case types.KInt:
		return "int4"
	case types.KFloat:
		return "float8"
	case types.KBool:
		return "bool"
	case types.KTime:
		return "timestamp"
	}
	return "text"
}

// TypeNameByOID 把 PG 类型 OID 反查成显示名（format_type 用）。
func TypeNameByOID(oid int32) string { return types.NameByOID(oid) }

// sortedNames 仅用于调试输出。
func sortedNames(m *meta) []string {
	out := make([]string, 0, len(m.tables))
	for _, tm := range m.tables {
		out = append(out, tm.t.Name)
	}
	sort.Strings(out)
	return out
}

// BtreeAmOID 是 pg_am 里 btree 的 OID（和 PostgreSQL 一致）。
const BtreeAmOID = 403

// btreeOpclasses 是各类型在 btree 上的默认操作符类，
// OID 取自真实 PostgreSQL（pg_opclass WHERE opcdefault AND opcmethod=btree）。
var btreeOpclasses = []struct {
	oid    int32
	name   string
	intype int32 // 对应的类型 OID
}{
	{10011, "bool_ops", 16},
	{10036, "name_ops", 19},
	{3124, "int8_ops", 20},
	{1979, "int2_ops", 21},
	{1978, "int4_ops", 23},
	{3126, "text_ops", 25},
	{1981, "oid_ops", 26},
	{10020, "float4_ops", 700},
	{3123, "float8_ops", 701},
	{10012, "bpchar_ops", 1042},
	{3122, "date_ops", 1082},
	{10045, "time_ops", 1083},
	{3128, "timestamp_ops", 1114},
	{3127, "timestamptz_ops", 1184},
	{10030, "interval_ops", 1186},
	{3125, "numeric_ops", 1700},
}

// opclassOf 返回该类型在 btree 上的默认操作符类 OID；未知类型给 0。
func opclassOf(atttypid int32) int32 {
	for _, oc := range btreeOpclasses {
		if oc.intype == atttypid {
			return oc.oid
		}
	}
	return 0
}

// comparisonOperators 是常用类型上的比较操作符。
// oid 与 oprcode 都照抄真实 PostgreSQL（pg_operator）。
var comparisonOperators = []struct {
	oid  int
	name string
	typ  int // 左右操作数类型 OID
	code string
}{
	{91, "=", 16, "booleq"},
	{58, "<", 16, "boollt"},
	{59, ">", 16, "boolgt"},
	{1694, "<=", 16, "boolle"},
	{1695, ">=", 16, "boolge"},
	{85, "<>", 16, "boolne"},
	{410, "=", 20, "int8eq"},
	{412, "<", 20, "int8lt"},
	{413, ">", 20, "int8gt"},
	{414, "<=", 20, "int8le"},
	{415, ">=", 20, "int8ge"},
	{411, "<>", 20, "int8ne"},
	{96, "=", 23, "int4eq"},
	{97, "<", 23, "int4lt"},
	{521, ">", 23, "int4gt"},
	{523, "<=", 23, "int4le"},
	{525, ">=", 23, "int4ge"},
	{518, "<>", 23, "int4ne"},
	{98, "=", 25, "texteq"},
	{664, "<", 25, "text_lt"},
	{666, ">", 25, "text_gt"},
	{665, "<=", 25, "text_le"},
	{667, ">=", 25, "text_ge"},
	{531, "<>", 25, "textne"},
	{1093, "=", 1082, "date_eq"},
	{1095, "<", 1082, "date_lt"},
	{1097, ">", 1082, "date_gt"},
	{1096, "<=", 1082, "date_le"},
	{1098, ">=", 1082, "date_ge"},
	{1094, "<>", 1082, "date_ne"},
	{2060, "=", 1114, "timestamp_eq"},
	{2062, "<", 1114, "timestamp_lt"},
	{2064, ">", 1114, "timestamp_gt"},
	{2063, "<=", 1114, "timestamp_le"},
	{2065, ">=", 1114, "timestamp_ge"},
	{2061, "<>", 1114, "timestamp_ne"},
	{1320, "=", 1184, "timestamptz_eq"},
	{1322, "<", 1184, "timestamptz_lt"},
	{1324, ">", 1184, "timestamptz_gt"},
	{1323, "<=", 1184, "timestamptz_le"},
	{1325, ">=", 1184, "timestamptz_ge"},
	{1321, "<>", 1184, "timestamptz_ne"},
	{1752, "=", 1700, "numeric_eq"},
	{1754, "<", 1700, "numeric_lt"},
	{1756, ">", 1700, "numeric_gt"},
	{1755, "<=", 1700, "numeric_le"},
	{1757, ">=", 1700, "numeric_ge"},
	{1753, "<>", 1700, "numeric_ne"},
}

// fkOIDBase 是外键约束 OID 的起点，避开索引/唯一约束占用的区间。
const fkOIDBase = 21000

// refColNums 把列名列表转成 conkey/confkey 用的 "{1,2}" 形式。
func refColNums(t *catalog.Table, cols []string) (string, error) {
	parts := make([]string, 0, len(cols))
	for _, name := range cols {
		idx, ok := t.ColumnIdx(name)
		if !ok {
			return "", fmt.Errorf("column %q does not exist", name)
		}
		parts = append(parts, fmt.Sprintf("%d", idx+1))
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

// fkActionCode 把 ON DELETE / ON UPDATE 动作转成 pg_constraint 的单字符代码。
func fkActionCode(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "cascade":
		return "c"
	case "restrict":
		return "r"
	case "set null":
		return "n"
	case "set default":
		return "d"
	}
	return "a" // NO ACTION
}
