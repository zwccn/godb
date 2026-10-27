// Package syscat 用虚拟表实现 PostgreSQL 的系统目录（pg_catalog 与
// information_schema），让 psql、Navicat、DBeaver 这类客户端能浏览对象。
//
// 这些表不落盘、不可写，行数据在查询时由当前 catalog 实时生成。
package syscat

import (
	"strings"
	"sync"

	"godb/catalog"
	"godb/types"
)

// 固定的对象编号，保证同一次运行内引用一致（客户端会拿 OID 互相关联）。
const (
	OwnerOID = 10
	DbOID    = 16384
	// LastSystemOID 是 pg_database.datlastsysoid 的取值（= FirstNormalObjectId-1）。
	// 客户端靠 oid <= datlastsysoid 判断「系统对象」，Navicat 表设计器就用它
	// 把内置类型和用户类型分开；填 0 会让它在类型下拉里看不到 int4/text 这些。
	LastSystemOID    = 16383
	NamespaceCatalog = 11
	NamespacePublic  = 2200
	NamespaceInfo    = 13207
	TablespaceDef    = 1663
	TablespaceGlobal = 1664

	userTableOIDBase  = 16385
	sysTableOIDBase   = 10000
	indexOIDBase      = 30000
	constraintOIDBase = 40000
	attrDefOIDBase    = 50000
)

// OwnerName 是本库唯一的角色名，与 pgwire 返回的 current_user 保持一致。
const OwnerName = "godb"

// DatabaseName 是唯一的数据库名。
const DatabaseName = "godb"

// ServerVersion / ServerVersionNum 是汇报给客户端的版本号，三处必须一致：
// 这里、pgwire 的握手参数、exec 里 current_setting('server_version')。
//
// 报 10.0 而不是 13.0：目录表本来就是 PG 11 之前的形状（relhasoids、consrc、
// proisagg 都还在），老客户端（Navicat 12）还会按版本挑元数据路径。
const (
	ServerVersion    = "10.0"
	ServerVersionNum = "100000"
	GodbVersion      = "0.1.0"
)

// ServerVersionLine 是 version() 的返回值。
const ServerVersionLine = "PostgreSQL " + ServerVersion + " (godb " + GodbVersion + ") on x86_64-pc-windows, compiled by Go"

// colSpec 是虚拟表的一列。
type colSpec struct{ name, typ string }

// tableSpec 是一张虚拟表。
type tableSpec struct {
	schema string // pg_catalog / information_schema
	name   string
	cols   []colSpec
}

// 常用类型名缩写，纯粹为了让下面的表定义短一点。
const (
	tOid      = "oid"
	tName     = "name"
	tText     = "text"
	tInt2     = "int2"
	tInt4     = "int4"
	tInt8     = "int8"
	tBool     = "bool"
	tChar     = `"char"`
	tFloat4   = "float4"
	tXid      = "xid"
	tOidArr   = "oid[]"
	tTextArr  = "text[]"
	tCharArr  = "\"char\"[]"
	tInt2Arr  = "int2[]"
	tRegproc  = "regproc"
	tAclArr   = "aclitem[]"
	tNodeTree = "pg_node_tree"
	tXidArr   = "xid[]"
	tVec      = "int2vector"
	tOidVec   = "oidvector"
	tTS       = "timestamptz"
)

func c(name, typ string) colSpec { return colSpec{name, typ} }

// specs 是全部虚拟表定义。
var specs = []tableSpec{
	// ---------------- pg_catalog ----------------
	{"pg_catalog", "pg_database", []colSpec{
		c("oid", tOid), c("datname", tName), c("datdba", tOid), c("encoding", tInt4),
		c("datcollate", tText), c("datctype", tText), c("datistemplate", tBool),
		c("datallowconn", tBool), c("datconnlimit", tInt4), c("datlastsysoid", tOid),
		c("datfrozenxid", tXid), c("datminmxid", tXid), c("dattablespace", tOid), c("datacl", tAclArr),
	}},
	{"pg_catalog", "pg_namespace", []colSpec{
		c("oid", tOid), c("nspname", tName), c("nspowner", tOid), c("nspacl", tAclArr),
	}},
	{"pg_catalog", "pg_class", []colSpec{
		c("oid", tOid), c("relname", tName), c("relnamespace", tOid), c("reltype", tOid),
		c("reloftype", tOid), c("relowner", tOid), c("relam", tOid), c("relfilenode", tOid),
		c("reltablespace", tOid), c("relpages", tInt4), c("reltuples", tFloat4),
		c("relallvisible", tInt4), c("reltoastrelid", tOid), c("relhasindex", tBool),
		// relhasoids 在 PG 12 已经移除，但老客户端（Navicat、旧版 JDBC）
		// 仍会 SELECT 它，缺列会直接报错，所以保留并恒为 false。
		c("relhasoids", tBool),
		c("relisshared", tBool), c("relpersistence", tChar), c("relkind", tChar),
		c("relnatts", tInt2), c("relchecks", tInt2), c("relhasrules", tBool),
		c("relhastriggers", tBool), c("relhassubclass", tBool), c("relrowsecurity", tBool),
		c("relforcerowsecurity", tBool), c("relispopulated", tBool), c("relreplident", tChar),
		c("relispartition", tBool), c("relrewrite", tOid), c("relfrozenxid", tXid),
		c("relminmxid", tXid), c("relacl", tAclArr), c("reloptions", tTextArr),
		c("relpartbound", tNodeTree),
	}},
	{"pg_catalog", "pg_attribute", []colSpec{
		c("attrelid", tOid), c("attname", tName), c("atttypid", tOid), c("attstattarget", tInt4),
		c("attlen", tInt2), c("attnum", tInt2), c("attndims", tInt4), c("attcacheoff", tInt4),
		c("atttypmod", tInt4), c("attbyval", tBool), c("attstorage", tChar), c("attalign", tChar),
		c("attnotnull", tBool), c("atthasdef", tBool), c("atthasmissing", tBool),
		c("attidentity", tChar), c("attgenerated", tChar), c("attisdropped", tBool),
		c("attislocal", tBool), c("attinhcount", tInt4), c("attcollation", tOid),
		c("attacl", tAclArr), c("attoptions", tTextArr), c("attfdwoptions", tTextArr),
		c("attmissingval", tText),
	}},
	{"pg_catalog", "pg_type", []colSpec{
		c("oid", tOid), c("typname", tName), c("typnamespace", tOid), c("typowner", tOid),
		c("typlen", tInt2), c("typbyval", tBool), c("typtype", tChar), c("typcategory", tChar),
		c("typispreferred", tBool), c("typisdefined", tBool), c("typdelim", tChar),
		c("typrelid", tOid), c("typelem", tOid), c("typarray", tOid), c("typinput", tRegproc),
		c("typoutput", tRegproc), c("typreceive", tRegproc), c("typsend", tRegproc),
		c("typmodin", tRegproc), c("typmodout", tRegproc), c("typanalyze", tRegproc),
		c("typalign", tChar), c("typstorage", tChar), c("typnotnull", tBool),
		c("typbasetype", tOid), c("typtypmod", tInt4), c("typndims", tInt4),
		c("typcollation", tOid), c("typdefaultbin", tText), c("typdefault", tText),
		c("typacl", tAclArr),
	}},
	{"pg_catalog", "pg_index", []colSpec{
		c("indexrelid", tOid), c("indrelid", tOid), c("indnatts", tInt2), c("indnkeyatts", tInt2),
		c("indisunique", tBool), c("indisprimary", tBool), c("indisexclusion", tBool),
		c("indimmediate", tBool), c("indisclustered", tBool), c("indisvalid", tBool),
		c("indcheckxmin", tBool), c("indisready", tBool), c("indislive", tBool),
		c("indisreplident", tBool), c("indkey", tVec), c("indcollation", tOidVec),
		c("indclass", tOidVec), c("indoption", tVec), c("indexprs", tNodeTree),
		c("indpred", tNodeTree),
	}},
	{"pg_catalog", "pg_constraint", []colSpec{
		c("oid", tOid), c("conname", tName), c("connamespace", tOid), c("contype", tChar),
		c("condeferrable", tBool), c("condeferred", tBool), c("convalidated", tBool),
		c("conrelid", tOid), c("contypid", tOid), c("conindid", tOid), c("conparentid", tOid),
		c("confrelid", tOid), c("confupdtype", tChar), c("confdeltype", tChar),
		c("confmatchtype", tChar), c("conislocal", tBool), c("coninhcount", tInt4),
		c("connoinherit", tBool), c("conkey", tInt2Arr), c("confkey", tInt2Arr),
		c("conpfeqop", tOidArr), c("conppeqop", tOidArr), c("conffeqop", tOidArr),
		c("conexclop", tOidArr), c("conbin", tNodeTree),
		// consrc 在 PG 12 已经被 conbin + pg_get_constraintdef 取代，
		// 但 Navicat 的「设计表」仍在 SELECT 它，缺列会整条查询失败。
		// 保留并恒为 NULL。
		c("consrc", tText),
	}},
	{"pg_catalog", "pg_attrdef", []colSpec{
		c("oid", tOid), c("adrelid", tOid), c("adnum", tInt2), c("adbin", tText),
	}},
	{"pg_catalog", "pg_am", []colSpec{
		c("oid", tOid), c("amname", tName), c("amhandler", tRegproc), c("amtype", tChar),
	}},
	{"pg_catalog", "pg_tablespace", []colSpec{
		c("oid", tOid), c("spcname", tName), c("spcowner", tOid), c("spcacl", tAclArr),
		c("spcoptions", tTextArr),
	}},
	{"pg_catalog", "pg_roles", []colSpec{
		c("oid", tOid), c("rolname", tName), c("rolsuper", tBool), c("rolinherit", tBool),
		c("rolcreaterole", tBool), c("rolcreatedb", tBool), c("rolcanlogin", tBool),
		c("rolreplication", tBool), c("rolconnlimit", tInt4), c("rolpassword", tText),
		c("rolvaliduntil", tTS), c("rolbypassrls", tBool), c("rolconfig", tTextArr),
	}},
	{"pg_catalog", "pg_authid", []colSpec{
		c("oid", tOid), c("rolname", tName), c("rolsuper", tBool), c("rolinherit", tBool),
		c("rolcreaterole", tBool), c("rolcreatedb", tBool), c("rolcanlogin", tBool),
		c("rolreplication", tBool), c("rolbypassrls", tBool), c("rolconnlimit", tInt4),
		c("rolpassword", tText), c("rolvaliduntil", tTS),
	}},
	{"pg_catalog", "pg_user", []colSpec{
		c("usename", tName), c("usesysid", tOid), c("usecreatedb", tBool),
		c("usesuper", tBool), c("userepl", tBool), c("usebypassrls", tBool),
		c("passwd", tText), c("valuntil", tTS), c("useconfig", tTextArr),
	}},
	{"pg_catalog", "pg_shadow", []colSpec{
		c("usename", tName), c("usesysid", tOid), c("usecreatedb", tBool),
		c("usesuper", tBool), c("userepl", tBool), c("usebypassrls", tBool),
		c("passwd", tText), c("valuntil", tTS), c("useconfig", tTextArr),
	}},
	{"pg_catalog", "pg_group", []colSpec{
		c("groname", tName), c("grosysid", tOid), c("grolist", tOidArr),
	}},
	{"pg_catalog", "pg_settings", []colSpec{
		c("name", tText), c("setting", tText), c("unit", tText), c("category", tText),
		c("short_desc", tText), c("extra_desc", tText), c("context", tText),
		c("vartype", tText), c("source", tText), c("min_val", tText), c("max_val", tText),
		c("enumvals", tTextArr), c("boot_val", tText), c("reset_val", tText),
		c("sourcefile", tText), c("sourceline", tInt4), c("pending_restart", tBool),
	}},
	{"pg_catalog", "pg_description", []colSpec{
		c("objoid", tOid), c("classoid", tOid), c("objsubid", tInt4), c("description", tText),
	}},
	{"pg_catalog", "pg_shdescription", []colSpec{
		c("objoid", tOid), c("classoid", tOid), c("description", tText),
	}},
	{"pg_catalog", "pg_inherits", []colSpec{
		c("inhrelid", tOid), c("inhparent", tOid), c("inhseqno", tInt4),
	}},
	{"pg_catalog", "pg_enum", []colSpec{
		c("oid", tOid), c("enumtypid", tOid), c("enumsortorder", tFloat4),
		c("enumlabel", tName),
	}},
	{"pg_catalog", "pg_range", []colSpec{
		c("rngtypid", tOid), c("rngsubtype", tOid), c("rngcollation", tOid),
		c("rngsubopc", tOid), c("rngcanonical", tRegproc), c("rngsubdiff", tRegproc),
	}},
	{"pg_catalog", "pg_collation", []colSpec{
		c("oid", tOid), c("collname", tName), c("collnamespace", tOid),
		c("collowner", tOid), c("collprovider", tChar), c("collisdeterministic", tBool),
		c("collencoding", tInt4), c("collcollate", tText), c("collctype", tText),
		c("collversion", tText),
	}},
	{"pg_catalog", "pg_extension", []colSpec{
		c("oid", tOid), c("extname", tName), c("extowner", tOid), c("extnamespace", tOid),
		c("extrelocatable", tBool), c("extversion", tText), c("extconfig", tOidArr),
		c("extcondition", tTextArr),
	}},
	{"pg_catalog", "pg_proc", []colSpec{
		c("oid", tOid), c("proname", tName), c("pronamespace", tOid), c("proowner", tOid),
		c("prolang", tOid), c("procost", tFloat4), c("prorows", tFloat4),
		c("provariadic", tOid), c("prosupport", tRegproc), c("prokind", tChar),
		// proisagg / proiswindow 在 PG 11 被 prokind 取代，但老客户端
		// （Navicat 的函数列表）仍在 SELECT，缺列整条查询就失败。
		c("proisagg", tBool), c("proiswindow", tBool),
		c("prosecdef", tBool), c("proleakproof", tBool), c("proisstrict", tBool),
		c("proretset", tBool), c("provolatile", tChar), c("proparallel", tChar),
		c("pronargs", tInt2), c("pronargdefaults", tInt2), c("prorettype", tOid),
		c("proargtypes", tOidArr), c("proallargtypes", tOidArr), c("proargmodes", tCharArr),
		c("proargnames", tTextArr), c("proargdefaults", tNodeTree), c("prosrc", tText),
		c("probin", tText), c("proconfig", tTextArr), c("proacl", tAclArr),
	}},
	{"pg_catalog", "pg_language", []colSpec{
		c("oid", tOid), c("lanname", tName), c("lanispl", tBool), c("lanpltrusted", tBool),
		c("lanplcallfoid", tOid), c("laninline", tOid), c("lanvalidator", tOid),
		c("lanacl", tAclArr),
	}},
	{"pg_catalog", "pg_trigger", []colSpec{
		c("oid", tOid), c("tgrelid", tOid), c("tgparentid", tOid), c("tgname", tName),
		c("tgfoid", tOid), c("tgtype", tInt2), c("tgenabled", tChar),
		c("tgisinternal", tBool), c("tgconstrrelid", tOid), c("tgconstrindid", tOid),
		c("tgconstraint", tOid), c("tgdeferrable", tBool), c("tginitdeferred", tBool),
		c("tgnargs", tInt2), c("tgattr", tVec), c("tgargs", "bytea"),
		c("tgqual", tNodeTree), c("tgoldtable", tName), c("tgnewtable", tName),
	}},
	{"pg_catalog", "pg_rewrite", []colSpec{
		c("oid", tOid), c("rulename", tName), c("ev_class", tOid), c("ev_type", tChar),
		c("ev_enabled", tChar), c("is_instead", tBool), c("ev_qual", tNodeTree),
		c("ev_action", tNodeTree),
	}},
	{"pg_catalog", "pg_depend", []colSpec{
		c("classid", tOid), c("objid", tOid), c("objsubid", tInt4),
		c("refclassid", tOid), c("refobjid", tOid), c("refobjsubid", tInt4),
		c("deptype", tChar),
	}},
	{"pg_catalog", "pg_operator", []colSpec{
		c("oid", tOid), c("oprname", tName), c("oprnamespace", tOid), c("oprowner", tOid),
		c("oprkind", tChar), c("oprcanmerge", tBool), c("oprcanhash", tBool),
		c("oprleft", tOid), c("oprright", tOid), c("oprresult", tOid),
		c("oprcom", tOid), c("oprnegate", tOid), c("oprcode", tRegproc),
		c("oprrest", tRegproc), c("oprjoin", tRegproc),
	}},
	{"pg_catalog", "pg_cast", []colSpec{
		c("oid", tOid), c("castsource", tOid), c("casttarget", tOid),
		c("castfunc", tOid), c("castcontext", tChar), c("castmethod", tChar),
	}},
	{"pg_catalog", "pg_opclass", []colSpec{
		c("oid", tOid), c("opcmethod", tOid), c("opcname", tName),
		c("opcnamespace", tOid), c("opcowner", tOid), c("opcfamily", tOid),
		c("opcintype", tOid), c("opcdefault", tBool), c("opckeytype", tOid),
	}},
	{"pg_catalog", "pg_opfamily", []colSpec{
		c("oid", tOid), c("opfmethod", tOid), c("opfname", tName),
		c("opfnamespace", tOid), c("opfowner", tOid),
	}},
	{"pg_catalog", "pg_conversion", []colSpec{
		c("oid", tOid), c("conname", tName), c("connamespace", tOid), c("conowner", tOid),
		c("conforencoding", tInt4), c("contoencoding", tInt4),
		c("conproc", tRegproc), c("condefault", tBool),
	}},
	{"pg_catalog", "pg_policy", []colSpec{
		c("oid", tOid), c("polname", tName), c("polrelid", tOid), c("polcmd", tChar),
		c("polpermissive", tBool), c("polroles", tOidArr), c("polqual", tNodeTree),
		c("polwithcheck", tNodeTree),
	}},
	{"pg_catalog", "pg_publication", []colSpec{
		c("oid", tOid), c("pubname", tName), c("pubowner", tOid), c("puballtables", tBool),
		c("pubinsert", tBool), c("pubupdate", tBool), c("pubdelete", tBool),
		c("pubtruncate", tBool), c("pubviaroot", tBool),
	}},
	{"pg_catalog", "pg_sequence", []colSpec{
		c("seqrelid", tOid), c("seqtypid", tOid), c("seqstart", tInt8),
		c("seqincrement", tInt8), c("seqmax", tInt8), c("seqmin", tInt8),
		c("seqcache", tInt8), c("seqcycle", tBool),
	}},
	{"pg_catalog", "pg_partitioned_table", []colSpec{
		c("partrelid", tOid), c("partstrat", tChar), c("partnatts", tInt2),
		c("partdefid", tOid), c("partattrs", tVec), c("partexprs", tNodeTree),
	}},
	{"pg_catalog", "pg_statistic_ext", []colSpec{
		c("oid", tOid), c("stxrelid", tOid), c("stxname", tName),
		c("stxnamespace", tOid), c("stxowner", tOid), c("stxstattarget", tInt4),
		// PG 10 的扩展统计有这两列，psql 的 \d 会读它们判断有没有统计信息
		c("stxkeys", "int2vector"), c("stxkind", "char[]"),
	}},
	// 逻辑复制与统计扩展的伴生表：我们没有这些功能，但 psql 的 \d 会无条件
	// 查它们（LEFT JOIN），缺表会让整条查询失败。空表即可。
	{"pg_catalog", "pg_publication_rel", []colSpec{
		c("oid", tOid), c("prpubid", tOid), c("prrelid", tOid),
	}},
	{"pg_catalog", "pg_transform", []colSpec{
		c("oid", tOid), c("trftype", tOid), c("trflang", tOid),
		c("trffromsql", tRegproc), c("trftosql", tRegproc),
	}},
	{"pg_catalog", "pg_statistic_ext_data", []colSpec{
		c("stxoid", tOid), c("stxddistsqldata", tNodeTree), c("stxdndistinct", tNodeTree),
		c("stxddependencies", tNodeTree), c("stxdmcv", tNodeTree),
	}},
	{"pg_catalog", "pg_statistic", []colSpec{c("starelid", tOid), c("staattnum", tInt2), c("stainherit", tBool),
		c("stanullfrac", tFloat4), c("stawidth", tInt4), c("stadistinct", tFloat4),
	}},
	{"pg_catalog", "pg_foreign_data_wrapper", []colSpec{
		c("oid", tOid), c("fdwname", tName), c("fdwowner", tOid),
		c("fdwhandler", tOid), c("fdwvalidator", tOid), c("fdwacl", tAclArr),
		c("fdwoptions", tTextArr),
	}},
	{"pg_catalog", "pg_foreign_server", []colSpec{
		c("oid", tOid), c("srvname", tName), c("srvowner", tOid), c("srvfdw", tOid),
		c("srvtype", tText), c("srvversion", tText), c("srvacl", tAclArr),
		c("srvoptions", tTextArr),
	}},
	{"pg_catalog", "pg_foreign_table", []colSpec{
		c("ftrelid", tOid), c("ftserver", tOid), c("ftoptions", tTextArr),
	}},
	{"pg_catalog", "pg_user_mapping", []colSpec{
		c("oid", tOid), c("umuser", tOid), c("umserver", tOid), c("umoptions", tTextArr),
	}},
	{"pg_catalog", "pg_event_trigger", []colSpec{
		c("oid", tOid), c("evtname", tName), c("evtevent", tName), c("evtowner", tOid),
		c("evtfoid", tOid), c("evtenabled", tChar), c("evttags", tTextArr),
	}},
	{"pg_catalog", "pg_subscription", []colSpec{
		c("oid", tOid), c("subdbid", tOid), c("subname", tName), c("subowner", tOid),
		c("subenabled", tBool), c("subconninfo", tText), c("subslotname", tName),
		c("subsynccommit", tText), c("subpublications", tTextArr),
	}},
	{"pg_catalog", "pg_replication_origin", []colSpec{
		c("roident", tOid), c("roname", tText),
	}},
	{"pg_catalog", "pg_largeobject_metadata", []colSpec{
		c("oid", tOid), c("lomowner", tOid), c("lomacl", tAclArr),
	}},
	{"pg_catalog", "pg_seclabel", []colSpec{
		c("objoid", tOid), c("classoid", tOid), c("objsubid", tInt4),
		c("provider", tText), c("label", tText),
	}},
	{"pg_catalog", "pg_shdepend", []colSpec{
		c("dbid", tOid), c("classid", tOid), c("objid", tOid), c("objsubid", tInt4),
		c("refclassid", tOid), c("refobjid", tOid), c("deptype", tChar),
	}},
	{"pg_catalog", "pg_ts_config", []colSpec{
		c("oid", tOid), c("cfgname", tName), c("cfgnamespace", tOid), c("cfgowner", tOid),
		c("cfgparser", tOid),
	}},
	{"pg_catalog", "pg_ts_dict", []colSpec{
		c("oid", tOid), c("dictname", tName), c("dictnamespace", tOid),
		c("dictowner", tOid), c("dicttemplate", tOid), c("dictinitoption", tText),
	}},
	{"pg_catalog", "pg_ts_parser", []colSpec{
		c("oid", tOid), c("prsname", tName), c("prsnamespace", tOid),
		c("prsstart", tRegproc), c("prstoken", tRegproc), c("prsend", tRegproc),
		c("prsheadline", tRegproc), c("prslextype", tRegproc),
	}},
	{"pg_catalog", "pg_ts_template", []colSpec{
		c("oid", tOid), c("tmplname", tName), c("tmplnamespace", tOid),
		c("tmplinit", tRegproc), c("tmpllexize", tRegproc),
	}},

	// 视图类（由 catalog 派生）
	{"pg_catalog", "pg_tables", []colSpec{
		c("schemaname", tName), c("tablename", tName), c("tableowner", tName),
		c("tablespace", tName), c("hasindexes", tBool), c("hasrules", tBool),
		c("hastriggers", tBool), c("rowsecurity", tBool),
	}},
	{"pg_catalog", "pg_views", []colSpec{
		c("schemaname", tName), c("viewname", tName), c("viewowner", tName),
		c("definition", tText),
	}},
	{"pg_catalog", "pg_matviews", []colSpec{
		c("schemaname", tName), c("matviewname", tName), c("matviewowner", tName),
		c("tablespace", tName), c("hasindexes", tBool), c("ispopulated", tBool),
		c("definition", tText),
	}},
	{"pg_catalog", "pg_indexes", []colSpec{
		c("schemaname", tName), c("tablename", tName), c("indexname", tName),
		c("tablespace", tName), c("indexdef", tText),
	}},
	{"pg_catalog", "pg_sequences", []colSpec{
		c("schemaname", tName), c("sequencename", tName), c("sequenceowner", tName),
		c("data_type", "regtype"), c("start_value", tInt8), c("min_value", tInt8),
		c("max_value", tInt8), c("increment_by", tInt8), c("cycle", tBool),
		c("cache_size", tInt8), c("last_value", tInt8),
	}},
	{"pg_catalog", "pg_stat_activity", []colSpec{
		c("datid", tOid), c("datname", tName), c("pid", tInt4), c("leader_pid", tInt4),
		c("usesysid", tOid), c("usename", tName), c("application_name", tText),
		c("client_addr", "inet"), c("client_hostname", tText), c("client_port", tInt4),
		c("backend_start", tTS), c("xact_start", tTS), c("query_start", tTS),
		c("state_change", tTS), c("wait_event_type", tText), c("wait_event", tText),
		c("state", tText), c("backend_xid", tXid), c("backend_xmin", tXid),
		c("query", tText), c("backend_type", tText),
	}},
	{"pg_catalog", "pg_locks", []colSpec{
		c("locktype", tText), c("database", tOid), c("relation", tOid),
		c("page", tInt4), c("tuple", tInt2), c("virtualxid", tText),
		c("transactionid", tXid), c("classid", tOid), c("objid", tOid),
		c("objsubid", tInt2), c("virtualtransaction", tText), c("pid", tInt4),
		c("mode", tText), c("granted", tBool), c("fastpath", tBool),
	}},
	{"pg_catalog", "pg_prepared_statements", []colSpec{
		c("name", tText), c("statement", tText), c("prepare_time", tTS),
		c("parameter_types", "regtype[]"), c("from_sql", tBool), c("generic_plans", tInt8),
		c("custom_plans", tInt8),
	}},
	{"pg_catalog", "pg_prepared_xacts", []colSpec{
		c("transaction", tXid), c("gid", tText), c("prepared", tTS),
		c("owner", tName), c("database", tName),
	}},
	{"pg_catalog", "pg_cursors", []colSpec{
		c("name", tText), c("statement", tText), c("is_holdable", tBool),
		c("is_binary", tBool), c("is_scrollable", tBool), c("creation_time", tTS),
	}},
	{"pg_catalog", "pg_timezone_names", []colSpec{
		c("name", tText), c("abbrev", tText), c("utc_offset", "interval"), c("is_dst", tBool),
	}},
	{"pg_catalog", "pg_available_extensions", []colSpec{
		c("name", tName), c("default_version", tText), c("installed_version", tText),
		c("comment", tText),
	}},
	{"pg_catalog", "pg_available_extension_versions", []colSpec{
		c("name", tName), c("version", tText), c("installed", tBool),
		c("superuser", tBool), c("trusted", tBool), c("relocatable", tBool),
		c("schema", tName), c("requires", tTextArr), c("comment", tText),
	}},
	{"pg_catalog", "pg_stat_user_tables", []colSpec{
		c("relid", tOid), c("schemaname", tName), c("relname", tName),
		c("seq_scan", tInt8), c("seq_tup_read", tInt8), c("idx_scan", tInt8),
		c("idx_tup_fetch", tInt8), c("n_tup_ins", tInt8), c("n_tup_upd", tInt8),
		c("n_tup_del", tInt8), c("n_tup_hot_upd", tInt8), c("n_live_tup", tInt8),
		c("n_dead_tup", tInt8), c("last_vacuum", tTS), c("last_autovacuum", tTS),
		c("last_analyze", tTS), c("last_autoanalyze", tTS),
	}},
	{"pg_catalog", "pg_stat_all_tables", []colSpec{
		c("relid", tOid), c("schemaname", tName), c("relname", tName),
		c("seq_scan", tInt8), c("seq_tup_read", tInt8), c("idx_scan", tInt8),
		c("idx_tup_fetch", tInt8), c("n_tup_ins", tInt8), c("n_tup_upd", tInt8),
		c("n_tup_del", tInt8), c("n_live_tup", tInt8), c("n_dead_tup", tInt8),
	}},
	{"pg_catalog", "pg_stat_database", []colSpec{
		c("datid", tOid), c("datname", tName), c("numbackends", tInt4),
		c("xact_commit", tInt8), c("xact_rollback", tInt8), c("blks_read", tInt8),
		c("blks_hit", tInt8), c("tup_returned", tInt8), c("tup_fetched", tInt8),
		c("tup_inserted", tInt8), c("tup_updated", tInt8), c("tup_deleted", tInt8),
	}},
	{"pg_catalog", "pg_stat_user_indexes", []colSpec{
		c("relid", tOid), c("indexrelid", tOid), c("schemaname", tName),
		c("relname", tName), c("indexrelname", tName), c("idx_scan", tInt8),
		c("idx_tup_read", tInt8), c("idx_tup_fetch", tInt8),
	}},

	// ---------------- information_schema ----------------
	{"information_schema", "schemata", []colSpec{
		c("catalog_name", "sql_identifier"), c("schema_name", "sql_identifier"),
		c("schema_owner", "sql_identifier"),
		c("default_character_set_catalog", "sql_identifier"),
		c("default_character_set_schema", "sql_identifier"),
		c("default_character_set_name", "sql_identifier"),
		c("sql_path", "character_data"),
	}},
	{"information_schema", "tables", []colSpec{
		c("table_catalog", "sql_identifier"), c("table_schema", "sql_identifier"),
		c("table_name", "sql_identifier"), c("table_type", "character_data"),
		c("self_referencing_column_name", "sql_identifier"),
		c("reference_generation", "character_data"),
		c("user_defined_type_catalog", "sql_identifier"),
		c("user_defined_type_schema", "sql_identifier"),
		c("user_defined_type_name", "sql_identifier"),
		c("is_insertable_into", "yes_or_no"), c("is_typed", "yes_or_no"),
		c("commit_action", "character_data"),
	}},
	{"information_schema", "columns", []colSpec{
		c("table_catalog", "sql_identifier"), c("table_schema", "sql_identifier"),
		c("table_name", "sql_identifier"), c("column_name", "sql_identifier"),
		c("ordinal_position", "cardinal_number"), c("column_default", "character_data"),
		c("is_nullable", "yes_or_no"), c("data_type", "character_data"),
		c("character_maximum_length", "cardinal_number"),
		c("character_octet_length", "cardinal_number"),
		c("numeric_precision", "cardinal_number"),
		c("numeric_precision_radix", "cardinal_number"),
		c("numeric_scale", "cardinal_number"),
		c("datetime_precision", "cardinal_number"),
		c("interval_type", "character_data"), c("interval_precision", "cardinal_number"),
		c("character_set_catalog", "sql_identifier"),
		c("character_set_schema", "sql_identifier"),
		c("character_set_name", "sql_identifier"),
		c("collation_catalog", "sql_identifier"),
		c("collation_schema", "sql_identifier"),
		c("collation_name", "sql_identifier"),
		c("domain_catalog", "sql_identifier"), c("domain_schema", "sql_identifier"),
		c("domain_name", "sql_identifier"), c("udt_catalog", "sql_identifier"),
		c("udt_schema", "sql_identifier"), c("udt_name", "sql_identifier"),
		c("scope_catalog", "sql_identifier"), c("scope_schema", "sql_identifier"),
		c("scope_name", "sql_identifier"), c("maximum_cardinality", "cardinal_number"),
		c("dtd_identifier", "sql_identifier"), c("is_self_referencing", "yes_or_no"),
		c("is_identity", "yes_or_no"), c("identity_generation", "character_data"),
		c("identity_start", "character_data"), c("identity_increment", "character_data"),
		c("identity_maximum", "character_data"), c("identity_minimum", "character_data"),
		c("identity_cycle", "yes_or_no"), c("is_generated", "character_data"),
		c("generation_expression", "character_data"), c("is_updatable", "yes_or_no"),
	}},
	{"information_schema", "table_constraints", []colSpec{
		c("constraint_catalog", "sql_identifier"), c("constraint_schema", "sql_identifier"),
		c("constraint_name", "sql_identifier"), c("table_catalog", "sql_identifier"),
		c("table_schema", "sql_identifier"), c("table_name", "sql_identifier"),
		c("constraint_type", "character_data"), c("is_deferrable", "yes_or_no"),
		c("initially_deferred", "yes_or_no"), c("enforced", "yes_or_no"),
	}},
	{"information_schema", "key_column_usage", []colSpec{
		c("constraint_catalog", "sql_identifier"), c("constraint_schema", "sql_identifier"),
		c("constraint_name", "sql_identifier"), c("table_catalog", "sql_identifier"),
		c("table_schema", "sql_identifier"), c("table_name", "sql_identifier"),
		c("column_name", "sql_identifier"), c("ordinal_position", "cardinal_number"),
		c("position_in_unique_constraint", "cardinal_number"),
	}},
	{"information_schema", "constraint_column_usage", []colSpec{
		c("table_catalog", "sql_identifier"), c("table_schema", "sql_identifier"),
		c("table_name", "sql_identifier"), c("column_name", "sql_identifier"),
		c("constraint_catalog", "sql_identifier"), c("constraint_schema", "sql_identifier"),
		c("constraint_name", "sql_identifier"),
	}},
	{"information_schema", "referential_constraints", []colSpec{
		c("constraint_catalog", "sql_identifier"), c("constraint_schema", "sql_identifier"),
		c("constraint_name", "sql_identifier"),
		c("unique_constraint_catalog", "sql_identifier"),
		c("unique_constraint_schema", "sql_identifier"),
		c("unique_constraint_name", "sql_identifier"),
		c("match_option", "character_data"), c("update_rule", "character_data"),
		c("delete_rule", "character_data"),
	}},
	{"information_schema", "views", []colSpec{
		c("table_catalog", "sql_identifier"), c("table_schema", "sql_identifier"),
		c("table_name", "sql_identifier"), c("view_definition", "character_data"),
		c("check_option", "character_data"), c("is_updatable", "yes_or_no"),
		c("is_insertable_into", "yes_or_no"),
		c("is_trigger_updatable", "yes_or_no"), c("is_trigger_deletable", "yes_or_no"),
		c("is_trigger_insertable_into", "yes_or_no"),
	}},
	{"information_schema", "sequences", []colSpec{
		c("sequence_catalog", "sql_identifier"), c("sequence_schema", "sql_identifier"),
		c("sequence_name", "sql_identifier"), c("data_type", "character_data"),
		c("numeric_precision", "cardinal_number"),
		c("numeric_precision_radix", "cardinal_number"),
		c("numeric_scale", "cardinal_number"), c("start_value", "character_data"),
		c("minimum_value", "character_data"), c("maximum_value", "character_data"),
		c("increment", "character_data"), c("cycle_option", "yes_or_no"),
	}},
	{"information_schema", "routines", []colSpec{
		c("specific_catalog", "sql_identifier"), c("specific_schema", "sql_identifier"),
		c("specific_name", "sql_identifier"), c("routine_catalog", "sql_identifier"),
		c("routine_schema", "sql_identifier"), c("routine_name", "sql_identifier"),
		c("routine_type", "character_data"), c("data_type", "character_data"),
		c("routine_body", "character_data"), c("routine_definition", "character_data"),
	}},
	{"information_schema", "parameters", []colSpec{
		c("specific_catalog", "sql_identifier"), c("specific_schema", "sql_identifier"),
		c("specific_name", "sql_identifier"), c("ordinal_position", "cardinal_number"),
		c("parameter_mode", "character_data"), c("parameter_name", "sql_identifier"),
		c("data_type", "character_data"),
	}},
	{"information_schema", "triggers", []colSpec{
		c("trigger_catalog", "sql_identifier"), c("trigger_schema", "sql_identifier"),
		c("trigger_name", "sql_identifier"), c("event_manipulation", "character_data"),
		c("event_object_catalog", "sql_identifier"),
		c("event_object_schema", "sql_identifier"),
		c("event_object_table", "sql_identifier"),
		c("action_order", "cardinal_number"), c("action_condition", "character_data"),
		c("action_statement", "character_data"), c("action_orientation", "character_data"),
		c("action_timing", "character_data"),
	}},
	// 客户端的触发器查询会 LEFT JOIN 这张视图（我们没有任何触发器，恒空）。
	{"information_schema", "triggered_update_columns", []colSpec{
		c("trigger_catalog", "sql_identifier"), c("trigger_schema", "sql_identifier"),
		c("trigger_name", "sql_identifier"),
		c("event_object_catalog", "sql_identifier"),
		c("event_object_schema", "sql_identifier"),
		c("event_object_table", "sql_identifier"),
		c("event_object_column", "sql_identifier"),
	}},
	{"information_schema", "domains", []colSpec{
		c("domain_catalog", "sql_identifier"), c("domain_schema", "sql_identifier"),
		c("domain_name", "sql_identifier"), c("data_type", "character_data"),
	}},
	{"information_schema", "table_privileges", []colSpec{
		c("grantor", "sql_identifier"), c("grantee", "sql_identifier"),
		c("table_catalog", "sql_identifier"), c("table_schema", "sql_identifier"),
		c("table_name", "sql_identifier"), c("privilege_type", "character_data"),
		c("is_grantable", "yes_or_no"), c("with_hierarchy", "yes_or_no"),
	}},
	{"information_schema", "column_privileges", []colSpec{
		c("grantor", "sql_identifier"), c("grantee", "sql_identifier"),
		c("table_catalog", "sql_identifier"), c("table_schema", "sql_identifier"),
		c("table_name", "sql_identifier"), c("column_name", "sql_identifier"),
		c("privilege_type", "character_data"), c("is_grantable", "yes_or_no"),
	}},
	{"information_schema", "character_sets", []colSpec{
		c("character_set_catalog", "sql_identifier"),
		c("character_set_schema", "sql_identifier"),
		c("character_set_name", "sql_identifier"),
		c("character_repertoire", "sql_identifier"),
		c("form_of_use", "sql_identifier"),
		c("default_collate_catalog", "sql_identifier"),
		c("default_collate_schema", "sql_identifier"),
		c("default_collate_name", "sql_identifier"),
	}},
	{"information_schema", "collations", []colSpec{
		c("collation_catalog", "sql_identifier"), c("collation_schema", "sql_identifier"),
		c("collation_name", "sql_identifier"),
		c("pad_attribute", "character_data"),
	}},
	{"information_schema", "check_constraints", []colSpec{
		c("constraint_catalog", "sql_identifier"), c("constraint_schema", "sql_identifier"),
		c("constraint_name", "sql_identifier"), c("check_clause", "character_data"),
	}},
	{"information_schema", "element_types", []colSpec{
		c("object_catalog", "sql_identifier"), c("object_schema", "sql_identifier"),
		c("object_name", "sql_identifier"), c("object_type", "character_data"),
		c("collection_type_identifier", "sql_identifier"),
		c("data_type", "character_data"),
	}},
	{"information_schema", "sql_features", []colSpec{
		c("feature_id", "character_data"), c("feature_name", "character_data"),
		c("sub_feature_id", "character_data"), c("sub_feature_name", "character_data"),
		c("is_supported", "yes_or_no"), c("is_verified_by", "character_data"),
		c("comments", "character_data"),
	}},
}

// schemaOf 判断一张虚拟表属于哪个 schema。
func tableKey(schema, name string) string {
	schema = strings.ToLower(strings.TrimSpace(schema))
	name = strings.ToLower(strings.TrimSpace(name))
	if schema == "" {
		return "" // 由 Lookup 依次尝试
	}
	// PostgreSQL 里 public 下也能看到 pg_catalog 之外的视图，这里只做两套目录
	return schema + "." + name
}

var (
	once   sync.Once
	byKey  map[string]*catalog.Table
	byName map[string][]*catalog.Table
)

func build() {
	byKey = make(map[string]*catalog.Table, len(specs))
	byName = make(map[string][]*catalog.Table, len(specs))
	for _, sp := range specs {
		t := &catalog.Table{Name: sp.name}
		for _, cs := range sp.cols {
			col := &catalog.Column{Name: cs.name, TypeName: cs.typ, Kind: int(kindOf(cs.typ))}
			t.Columns = append(t.Columns, col)
		}
		t.Rebuild()
		byKey[sp.schema+"."+sp.name] = t
		byName[sp.name] = append(byName[sp.name], t)
	}
}

// kindOf 把 PG 类型名映射到内部的运行时类型。
func kindOf(typ string) types.Kind {
	switch typ {
	case "oid", "xid", "cid", "int2", "int4", "int8", "regproc", "serial":
		return types.KInt
	case "float4", "float8", "numeric":
		return types.KFloat
	case "bool", "yes_or_no":
		return types.KBool
	case "timestamptz", "timestamp", "date":
		return types.KTime
	}
	return types.KText
}

// Lookup 按 schema + 名字查找虚拟表；schema 为空时依次尝试 pg_catalog 与
// information_schema。返回的 Table.Name 是**不带 schema 的裸名**，这样列引用的
// 限定符（pg_class.relname）能正确解析。
func Lookup(schema, name string) (*catalog.Table, bool) {
	once.Do(build)
	n := strings.ToLower(strings.TrimSpace(name))
	s := strings.ToLower(strings.TrimSpace(schema))
	if s != "" && s != "public" {
		t, ok := byKey[s+"."+n]
		return t, ok
	}
	if t, ok := byKey["pg_catalog."+n]; ok {
		return t, true
	}
	if t, ok := byKey["information_schema."+n]; ok {
		return t, true
	}
	// 裸名也允许直接命中（客户端偶尔省略 schema）
	if list := byName[n]; len(list) > 0 {
		return list[0], true
	}
	return nil, false
}

// IsSystem 判断一个名字是否是虚拟系统表（用于跳过表锁）。
func IsSystem(schema, name string) bool {
	_, ok := Lookup(schema, name)
	return ok
}

// Source 提供生成行数据所需的上下文。
type Source struct {
	Cat   *catalog.Catalog
	Count func(table string) int
}

func (s Source) tables() []*catalog.Table {
	if s.Cat == nil {
		return nil
	}
	return s.Cat.Tables
}

func (s Source) rowsOf(t string) int64 {
	if s.Count == nil {
		return 0
	}
	return int64(s.Count(t))
}
