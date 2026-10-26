package syscat

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"godb/catalog"
	"godb/types"
)

// colMeta 是一列在系统目录里的投影。
type colMeta struct {
	idx      int
	atttypid int32
	attlen   int32
}

// idxMeta 描述一个由约束隐式创建的索引。
type idxMeta struct {
	oid     int32
	name    string
	colIdx  int
	primary bool
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
		for _, ci := range t.UniqueCols() {
			prim := t.Columns[ci].PrimaryKey
			name := fmt.Sprintf("%s_%s_key", t.Name, t.Columns[ci].Name)
			if prim {
				name = t.Name + "_pkey"
			}
			tm.idx = append(tm.idx, idxMeta{
				oid:     int32(indexOIDBase + n*len(src.tables()) + i),
				name:    name,
				colIdx:  ci,
				primary: prim,
				conOid:  int32(constraintOIDBase + n*len(src.tables()) + i),
				defOid:  int32(attrDefOIDBase + n*len(src.tables()) + i),
			})
			n++
		}
		m.tables = append(m.tables, *tm)
	}
	for i := range m.tables {
		tm := &m.tables[i]
		m.byName[strings.ToLower(tm.t.Name)] = tm
	}
	return m
}

// userTables 返回业务表（不含系统表）。
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
			"datlastsysoid": iv(0), "datfrozenxid": iv(0), "datminmxid": iv(1),
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
					"relowner": iv(OwnerOID), "relam": iv(4031), "relfilenode": iv(int(ix.oid)),
					"relpages": iv(1), "reltuples": types.Float(0), "relhasindex": bv(false),
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
					"attbyval":   bv(cm.attlen > 0 && cm.attlen <= 8),
					"attstorage": sv("x"), "attalign": sv("i"),
					"attnotnull": bv(col.NotNull || col.PrimaryKey), "atthasdef": bv(hasDef),
					"atthasmissing": bv(false), "attidentity": sv(""), "attgenerated": sv(""),
					"attisdropped": bv(false), "attislocal": bv(true), "attinhcount": iv(0),
					"attcollation": iv(0),
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
					"indisunique": bv(true), "indisprimary": bv(ix.primary),
					"indisexclusion": bv(false), "indimmediate": bv(true),
					"indisclustered": bv(false), "indisvalid": bv(true),
					"indcheckxmin": bv(false), "indisready": bv(true), "indislive": bv(true),
					"indisreplident": bv(false),
					"indkey":         sv(fmt.Sprintf("%d", ix.colIdx+1)),
					"indcollation":   sv("0"), "indclass": sv("0"), "indoption": sv("0"),
				}))
			}
		}
		return out

	case "pg_constraint":
		var out []types.Row
		for _, tm := range m.tables {
			for _, ix := range tm.idx {
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
					"conkey": sv(fmt.Sprintf("%d", ix.colIdx+1)),
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
			mk(def, map[string]types.Value{"oid": iv(4031), "amname": sv("btree"), "amhandler": sv("bthandler"), "amtype": sv("i")}),
			mk(def, map[string]types.Value{"oid": iv(4053), "amname": sv("hash"), "amhandler": sv("hashhandler"), "amtype": sv("i")}),
		}

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

	case "pg_stat_activity":
		return []types.Row{mk(def, map[string]types.Value{
			"datid": iv(DbOID), "datname": sv(DatabaseName), "pid": iv(1),
			"usesysid": iv(OwnerOID), "usename": sv(OwnerName),
			"backend_start": types.Time(time.Now()), "state": sv("active"),
			"query": sv(""), "backend_type": sv("client backend"),
		})}

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
				out = append(out, mk(def, map[string]types.Value{
					"table_catalog": sv(DatabaseName), "table_schema": sv("public"),
					"table_name": sv(tm.t.Name), "column_name": sv(col.Name),
					"ordinal_position":         iv(num + 1),
					"column_default":           nullableText(defaultExpr(tm.t, col)),
					"is_nullable":              yesNo(!(col.NotNull || col.PrimaryKey)),
					"data_type":                sv(infoDataType(col.Type())),
					"character_maximum_length": lenOrNull(col),
					"numeric_precision":        precOrNull(col),
					"numeric_scale":            scaleOrNull(col),
					"datetime_precision":       datetimePrecOrNull(col),
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
					out = append(out, mk(def, map[string]types.Value{
						"constraint_catalog": sv(DatabaseName), "constraint_schema": sv("public"),
						"constraint_name": sv(ix.name), "table_catalog": sv(DatabaseName),
						"table_schema": sv("public"), "table_name": sv(tm.t.Name),
						"column_name": sv(col.Name), "ordinal_position": iv(1),
					}))
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
	{"bool_array", 1000, "_bool", "A", 16, 0, "i", "x"},
	{"text_array", 1009, "_text", "A", 25, 0, "i", "x"},
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

func typeRows(def *catalog.Table) []types.Row {
	out := make([]types.Row, 0, len(builtinTypes))
	for _, td := range builtinTypes {
		out = append(out, mk(def, map[string]types.Value{
			"oid": iv(int(td.oid)), "typname": sv(td.name), "typnamespace": iv(NamespaceCatalog),
			"typowner": iv(OwnerOID), "typlen": iv(int(types.TypeLenByName(td.typ))),
			"typbyval": bv(types.TypeLenByName(td.typ) > 0 && types.TypeLenByName(td.typ) <= 8),
			"typtype":  sv("b"), "typcategory": sv(td.categ),
			"typispreferred": bv(td.name == "text" || td.name == "int4" || td.name == "float8"),
			"typisdefined":   bv(true), "typdelim": sv(","), "typrelid": iv(0),
			"typelem": iv(int(td.lem)), "typarray": iv(int(td.array)),
			"typinput": sv(td.name + "in"), "typoutput": sv(td.name + "out"),
			"typreceive": sv(td.name + "recv"), "typsend": sv(td.name + "send"),
			"typalign": sv(td.align), "typstorage": sv(td.store),
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
			"prorows": types.Float(0), "prokind": sv("f"), "prosecdef": bv(false),
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
		{"quote_all_identifiers", "off"}, {"search_path", `"$user", public`},
		{"server_encoding", "UTF8"}, {"server_version", "13.0"},
		{"server_version_num", "130000"}, {"session_authorization", OwnerName},
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
	col := tm.t.Columns[ix.colIdx]
	uniq := "UNIQUE "
	if ix.primary {
		uniq = "UNIQUE "
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON public.%s USING btree (%s)", uniq, ix.name, tm.t.Name, col.Name)
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

func precOrNull(col *catalog.Column) types.Value {
	if col.Type().Kind == types.KFloat {
		return iv(col.Type().Prec)
	}
	return types.Null
}

func scaleOrNull(col *catalog.Column) types.Value {
	if col.Type().Kind == types.KFloat {
		return iv(col.Type().Scale)
	}
	return types.Null
}

func datetimePrecOrNull(col *catalog.Column) types.Value {
	if col.Type().Kind == types.KTime {
		return iv(6)
	}
	return types.Null
}

func identityOf(col *catalog.Column) string {
	if col.Serial {
		return "YES"
	}
	return "NO"
}

// infoDataType 把内部类型映射成 information_schema.data_type 的写法。
func infoDataType(t types.Type) string {
	switch t.Kind {
	case types.KInt:
		return "bigint"
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
	switch t.Kind {
	case types.KInt:
		return "int8"
	case types.KFloat:
		return "float8"
	case types.KBool:
		return "bool"
	case types.KTime:
		return "timestamp"
	}
	if l := strings.ToLower(t.String()); strings.HasPrefix(l, "varchar") {
		return "varchar"
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
