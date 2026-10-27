package exec

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"godb/catalog"
	"godb/syscat"
	"godb/types"
)

// ---------- 虚拟系统表 ----------

// virtualOp 把 syscat 生成的行包装成一个算子树叶子节点。
// 行数据延迟到 Open 时生成，Describe（只编译不执行）不会白算一遍。
type virtualOp struct {
	name  string
	def   *catalog.Table
	src   syscat.Source
	rows  []types.Row
	built bool
	sc    Schema
	i     int
}

func (o *virtualOp) Schema() Schema { return o.sc }

func (o *virtualOp) Open() error {
	if !o.built {
		o.rows = syscat.Rows(o.def, o.src)
		o.built = true
	}
	o.i = 0
	return nil
}

func (o *virtualOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.i]
	o.i++
	return r, true, nil
}

// Close 必须把 built 一起复位：否则下一次 Open 会以为行还在，
// 但 rows 已经被清空 —— 表现为「同一个子查询第一次有值、之后全是 NULL」
// （标量子查询每处理一行外层数据都要重新 Open 一次，正好踩中）。
func (o *virtualOp) Close() error {
	o.rows = nil
	o.built = false
	return nil
}
func (o *virtualOp) Explain() string {
	return fmt.Sprintf("SystemScan(%s, %d rows)", o.name, len(o.rows))
}

// virtualTable 在本地 catalog 里找不到表时，尝试当作系统目录的虚拟表。
func (p *Planner) virtualTable(schema, name, qual string) (Operator, Schema, bool) {
	def, ok := syscat.Lookup(schema, name)
	if !ok {
		return nil, nil, false
	}
	src := syscat.Source{Cat: p.Cat}
	if p.Info != nil {
		src.Current = p.Info.Name
		if p.Info.All != nil {
			src.All = p.Info.All()
		}
	}
	if p.Tx != nil {
		if st := p.Tx.Store(); st != nil {
			src.Count = func(t string) int { return st.RowCount(t) }
		}
	}
	if qual == "" {
		qual = def.Name
	}
	sc := make(Schema, len(def.Columns))
	for i, c := range def.Columns {
		sc[i] = ColInfo{Qualifier: strings.ToLower(qual), Name: strings.ToLower(c.Name), Type: c.Type()}
	}
	return &virtualOp{name: def.Name, def: def, src: src, sc: sc}, sc, true
}

// ---------- 客户端兼容函数 ----------

// compatTypeName 记录兼容函数的返回类型关键字。
var compatFuncs = map[string]string{
	// 文本
	"version": "text", "current_database": "text", "current_schema": "name",
	// 不带括号写也能被解析成函数调用，这里给出真实返回
	"current_date": "date", "current_time": "timetz", "current_timestamp": "timestamptz",
	"localtime": "time", "localtimestamp": "timestamp", "current_role": "name",
	"current_catalog": "name",
	"current_user":    "name", "session_user": "name", "user": "name",
	"current_schemas": "text", "pg_get_userbyid": "name", "format_type": "text",
	"pg_get_expr": "text", "pg_get_indexdef": "text", "pg_get_constraintdef": "text",
	"pg_get_serial_sequence": "text", "obj_description": "text", "col_description": "text",
	"shobj_description": "text", "quote_ident": "text", "quote_literal": "text",
	"quote_nullable": "text", "array_to_string": "text", "pg_typeof": "text",
	"current_setting": "text", "set_config": "text", "pg_encoding_to_char": "text",
	"pg_client_encoding": "text", "pg_get_viewdef": "text", "pg_get_ruledef": "text",
	"pg_get_triggerdef": "text", "pg_get_function_arguments": "text",
	"pg_get_function_result": "text", "pg_get_functiondef": "text",
	"pg_get_partkeydef": "text", "pg_tablespace_location": "text",
	"pg_collation_for": "text", "pg_get_statisticsobjdef": "text",
	"current_query": "text", "inet_server_addr": "text", "inet_client_addr": "text",
	"pg_get_statisticsobjdef_columns": "text", "concat_ws": "text",
	"regexp_replace": "text", "md5": "text", "encode": "text", "decode": "text",
	"pg_size_pretty": "text", "pg_ls_dir": "text", "pg_read_file": "text",

	// 布尔
	"pg_table_is_visible": "bool", "pg_type_is_visible": "bool",
	"pg_function_is_visible": "bool", "pg_operator_is_visible": "bool",
	"has_table_privilege": "bool", "has_schema_privilege": "bool",
	"has_database_privilege": "bool", "has_column_privilege": "bool",
	"has_function_privilege": "bool", "pg_has_role": "bool",
	"pg_is_in_recovery": "bool", "pg_is_other_temp_schema": "bool",
	"pg_jit_available": "bool", "pg_relation_is_publishable": "bool",

	// 整数
	"pg_backend_pid": "int", "txid_current": "int", "pg_relation_size": "int",
	"pg_total_relation_size": "int", "pg_table_size": "int", "pg_indexes_size": "int",
	"pg_database_size": "int", "pg_column_size": "int", "pg_my_temp_schema": "int",
	"pg_trigger_depth": "int", "pg_encoding_max_length": "int",
	"pg_char_to_encoding": "int", "pg_size_bytes": "int", "array_length": "int",
	"inet_server_port": "int", "inet_client_port": "int", "pg_version_num": "int",
	"txid_current_if_assigned": "int",

	// 时间
	"pg_postmaster_start_time": "timestamp", "pg_conf_load_time": "timestamp",
	"transaction_timestamp": "timestamp", "statement_timestamp": "timestamp",
	"clock_timestamp": "timestamp",

	// 其它（一律 NULL）
	"pg_sleep": "text", "pg_notify": "text", "pg_advisory_lock": "text",
	"pg_advisory_unlock": "text", "pg_cancel_backend": "bool",
	"pg_terminate_backend": "bool", "setseed": "text", "pg_reload_conf": "bool",
}

func isCompatFunc(name string) bool {
	_, ok := compatFuncs[name]
	return ok
}

func compatFuncType(name string) (types.Type, bool) {
	k, ok := compatFuncs[name]
	if !ok {
		return types.Type{}, false
	}
	switch k {
	case "bool":
		return types.Type{Kind: types.KBool, Name: "boolean"}, true
	case "int":
		return types.Type{Kind: types.KInt, Name: "bigint"}, true
	case "timestamp":
		return types.Type{Kind: types.KTime, Name: "timestamp"}, true
	case "name":
		// PostgreSQL 里 current_user / pg_get_userbyid 这类返回 name（OID 19）
		return types.Type{Kind: types.KText, Name: "name"}, true
	}
	return types.Type{Kind: types.KText, Name: "text"}, true
}

// evalCompat 计算兼容函数。第二个返回值表示是否已处理。
// cat 只给需要反查系统目录的函数用（可 为 nil，此时那些函数返回空串）。
// dbName 是当前库名（多库模式下由连接决定）；空串表示单库模式，回退到默认库名。
func evalCompat(cat *catalog.Catalog, dbName, name string, vals []types.Value) (types.Value, bool, error) {
	arg := func(i int) types.Value {
		if i < len(vals) {
			return vals[i]
		}
		return types.Null
	}
	text := func(i int) string { return arg(i).TextForm() }
	integer := func(i int) int64 {
		v := arg(i)
		if v.IsNull() {
			return 0
		}
		if v.K == types.KInt {
			return v.I
		}
		return int64(v.Float())
	}

	switch name {
	case "version":
		return types.Text(syscat.ServerVersionLine), true, nil
	case "current_database":
		if dbName == "" {
			dbName = syscat.DefaultDatabaseName
		}
		return types.Text(dbName), true, nil
	case "current_schema":
		return types.Text("public"), true, nil
	case "current_user", "session_user", "user":
		return types.Text(syscat.OwnerName), true, nil
	case "current_schemas":
		return types.Text("{public}"), true, nil
	case "pg_get_userbyid":
		return types.Text(syscat.OwnerName), true, nil
	case "pg_backend_pid":
		return types.Int(1), true, nil
	case "txid_current", "pg_version_num":
		return types.Int(1), true, nil
	case "pg_is_in_recovery", "pg_jit_available", "pg_relation_is_publishable",
		"pg_is_other_temp_schema":
		return types.Bool(false), true, nil
	case "pg_table_is_visible", "pg_type_is_visible", "pg_function_is_visible",
		"pg_operator_is_visible":
		return types.Bool(true), true, nil
	case "has_table_privilege", "has_schema_privilege", "has_database_privilege",
		"has_column_privilege", "has_function_privilege", "pg_has_role",
		"pg_cancel_backend", "pg_terminate_backend", "pg_reload_conf":
		return types.Bool(true), true, nil
	case "pg_encoding_to_char":
		return types.Text("UTF8"), true, nil
	case "pg_client_encoding":
		return types.Text("UTF8"), true, nil
	case "pg_char_to_encoding", "pg_encoding_max_length":
		return types.Int(6), true, nil
	case "format_type":
		return types.Text(formatType(int32(integer(0)), int32(integer(1)))), true, nil
	case "pg_get_expr":
		// adbin 本身就存的是表达式文本，直接透出
		return types.Text(text(0)), true, nil
	case "pg_get_indexdef":
		// psql 的 \d 会从这个结果里截取第一个 '(' 之后的部分来显示索引定义，
		// 返回空串就会留下 "xxx PRIMARY KEY," 后面什么都没有（行尾多一个逗号）。
		if v, ok := syscat.IndexDefByOID(cat, int32(integer(0))); ok {
			return types.Text(v), true, nil
		}
		return types.Text(""), true, nil
	case "pg_get_constraintdef":
		if v, ok := syscat.ConstraintDefByOID(cat, int32(integer(0))); ok {
			return types.Text(v), true, nil
		}
		return types.Text(""), true, nil
	case "pg_get_viewdef", "pg_get_ruledef",
		"pg_get_triggerdef", "pg_get_functiondef", "pg_get_partkeydef",
		"pg_get_statisticsobjdef", "pg_get_statisticsobjdef_columns":
		return types.Text(""), true, nil
	case "pg_get_function_arguments", "pg_get_function_result":
		return types.Text(""), true, nil
	case "pg_get_serial_sequence":
		return types.Null, true, nil
	case "obj_description", "col_description", "shobj_description", "pg_collation_for",
		"pg_tablespace_location":
		return types.Null, true, nil
	case "current_date":
		now := time.Now()
		return types.Text(now.Format("2006-01-02")), true, nil
	case "current_time", "localtime":
		return types.Text(time.Now().Format("15:04:05")), true, nil
	case "current_timestamp", "localtimestamp":
		return types.Time(time.Now()), true, nil
	case "current_role", "current_catalog":
		return types.Text("godb"), true, nil
	case "current_setting":
		return types.Text(settingValue(text(0))), true, nil
	case "set_config":
		return types.Text(text(1)), true, nil
	case "quote_ident":
		return types.Text(quoteIdent(text(0))), true, nil
	case "quote_literal", "quote_nullable":
		return types.Text("'" + strings.ReplaceAll(text(0), "'", "''") + "'"), true, nil
	case "array_to_string":
		parts := splitArray(text(0))
		return types.Text(strings.Join(parts, text(1))), true, nil
	case "array_length":
		if len(splitArray(text(0))) == 0 {
			return types.Null, true, nil
		}
		return types.Int(1), true, nil
	case "pg_typeof":
		if arg(0).IsNull() {
			return types.Text("unknown"), true, nil
		}
		return types.Text(arg(0).K.String()), true, nil
	case "pg_relation_size", "pg_total_relation_size", "pg_table_size",
		"pg_indexes_size", "pg_database_size", "pg_column_size", "pg_size_bytes",
		"pg_my_temp_schema", "pg_trigger_depth":
		return types.Int(0), true, nil
	case "pg_size_pretty":
		return types.Text("8192 bytes"), true, nil
	case "pg_postmaster_start_time", "pg_conf_load_time", "transaction_timestamp",
		"statement_timestamp", "clock_timestamp":
		return types.Time(nowTime()), true, nil
	case "current_query":
		return types.Text(""), true, nil
	case "inet_server_addr", "inet_client_addr":
		return types.Null, true, nil
	case "inet_server_port", "inet_client_port":
		return types.Int(6432), true, nil
	case "concat_ws":
		if len(vals) == 0 {
			return types.Text(""), true, nil
		}
		sep := text(0)
		var parts []string
		for _, v := range vals[1:] {
			if v.IsNull() {
				continue
			}
			parts = append(parts, v.TextForm())
		}
		return types.Text(strings.Join(parts, sep)), true, nil
	case "regexp_replace":
		re, err := regexp.Compile(text(1))
		if err != nil {
			return types.Null, true, err
		}
		return types.Text(re.ReplaceAllString(text(0), text(2))), true, nil
	case "md5":
		return types.Text(md5Hex(text(0))), true, nil
	case "pg_sleep", "pg_notify", "pg_advisory_lock", "pg_advisory_unlock", "setseed":
		return types.Null, true, nil
	}
	return types.Null, false, nil
}

// settingValue 回应 current_setting(name) 和 SHOW name，覆盖客户端常问的参数。
// 版本号等全局常量只从 syscat 取，避免两边写死不一致（之前这里还留着一份 13.0）。
func settingValue(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "server_version":
		return syscat.ServerVersion
	case "server_version_num":
		return syscat.ServerVersionNum
	case "server_encoding", "client_encoding":
		return "UTF8"
	case "search_path":
		return "public"
	case "datestyle":
		return "ISO, MDY"
	case "timezone":
		return "UTC"
	case "transaction_isolation", "default_transaction_isolation":
		return "serializable"
	case "standard_conforming_strings", "integer_datetimes", "is_superuser":
		return "on"
	case "application_name", "session_authorization":
		return syscat.OwnerName
	case "max_connections":
		return "100"
	case "block_size", "wal_block_size":
		return "8192"
	case "segment_size":
		return "131072"
	case "max_identifier_length":
		return "63"
	case "max_index_keys", "max_function_args":
		if strings.EqualFold(name, "max_function_args") {
			return "100"
		}
		return "32"
	}
	return ""
}

// ShowResult 回应 SHOW name：一行一列，列名就是参数名。
//
// 放在这里而不是各客户端接入层，是因为网页控制台和 PG 协议走的是同一条
// 执行路径 —— 之前只有 PG 层能 `SHOW`，网页里敲 `SHOW server_version` 会
// 直接报语法错。不认识的参数回空串（真 PG 会报 42704，但客户端只是拿它做
// 探测，回空串更宽容）。
func ShowResult(name string) *Result {
	col := strings.TrimSpace(name)
	return &Result{
		Command:  "SHOW",
		Tag:      "SHOW",
		Columns:  []Column{{Name: col, Type: "text"}},
		Rows:     [][]interface{}{{settingValue(col)}},
		RowCount: 1,
	}
}

// SetResult 是 SET 的确认结果（godb 不保存会话参数）。
func SetResult() *Result {
	return &Result{Command: "SET", Tag: "SET"}
}

// formatType 把 atttypid + atttypmod 还原成客户端看到的类型写法。
func formatType(oid, typmod int32) string {
	name := types.NameByOID(oid)
	if typmod < 4 {
		return name
	}
	m := typmod - 4
	switch oid {
	case types.OIDVarchar, types.OIDBpchar:
		if m > 0 {
			return fmt.Sprintf("%s(%d)", name, m)
		}
	case types.OIDNumeric:
		prec := (m >> 16) & 0xffff
		scale := m & 0xffff
		if prec > 0 {
			return fmt.Sprintf("numeric(%d,%d)", prec, scale)
		}
	case types.OIDTimestamp, types.OIDTimestamptz, types.OIDTime,
		types.OIDTimetz, types.OIDInterval:
		if m >= 0 && m <= 6 {
			return fmt.Sprintf("%s(%d)", name, m)
		}
	}
	return name
}

// splitArray 解析 PG 的数组文本形式 {a,b,c}。
func splitArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		s = s[1 : len(s)-1]
	}
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '"':
			inQuote = !inQuote
		case ch == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case ch == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	out = append(out, cur.String())
	return out
}

func quoteIdent(s string) string {
	need := false
	for i, r := range s {
		if r >= 'a' && r <= 'z' || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		need = true
		break
	}
	if !need && s != "" {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func nowTime() time.Time { return time.Now() }

// ---------- ANY / ALL ----------

// anyEval 处理 x <op> ANY (arr) / ALL (arr)。数组用 PG 的文本形式表示。
type anyEval struct {
	op  string
	all bool
	x   Eval
	arr Eval
}

func (e *anyEval) Type() types.Type { return types.Type{Kind: types.KBool, Name: "boolean"} }

func (e *anyEval) Eval(row types.Row) (types.Value, error) {
	lv, err := e.x.Eval(row)
	if err != nil {
		return types.Null, err
	}
	av, err := e.arr.Eval(row)
	if err != nil {
		return types.Null, err
	}
	if av.IsNull() {
		return types.Null, nil
	}
	items := splitArray(av.TextForm())
	if len(items) == 0 {
		return types.Bool(e.all), nil
	}
	if lv.IsNull() {
		return types.Null, nil
	}
	for _, it := range items {
		cmp, err := types.Compare(lv, types.Text(it))
		if err != nil {
			return types.Null, err
		}
		hit := cmp == 0
		if e.op == "<>" {
			hit = cmp != 0
		}
		if e.all {
			if !hit {
				return types.Bool(false), nil
			}
		} else if hit {
			return types.Bool(true), nil
		}
	}
	return types.Bool(e.all), nil
}
