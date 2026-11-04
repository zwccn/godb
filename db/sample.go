package db

import (
	"fmt"
	"math/rand"
	"time"

	"godb/txn"
)

// SampleSQL 是演示数据集：部门/员工（JOIN、聚合、事务）、订单/订单行（三级外键链）、
// 文章 + 标签（8 维向量），让「按相似度取前 K 条」打开就能试。
//
// 外键在这里有两个用处：一是给帮助里的 JOIN 例子提供真实可跑的三级链，二是给
// 「库里有外键、模型图上一条线也没有」当对照物 —— 模型图（v3 起）只画人画的关系，
// 不读外键，所以载入示例之后打开模型图看到的是 6 张孤零零的卡片，需要自己拉线。
const SampleSQL = `
CREATE TABLE departments (
    id      SERIAL PRIMARY KEY,
    name    TEXT NOT NULL UNIQUE,
    budget  NUMERIC(12,2),
    city    TEXT
);

CREATE TABLE employees (
    id       SERIAL PRIMARY KEY,
    name     TEXT NOT NULL,
    dept_id  INTEGER REFERENCES departments(id),
    title    TEXT,
    salary   NUMERIC(10,2) NOT NULL DEFAULT 0,
    hired    TIMESTAMP,
    active   BOOLEAN DEFAULT TRUE
);

INSERT INTO departments (name, budget, city) VALUES
    ('研发部', 3200000, '深圳'),
    ('产品部', 1200000, '深圳'),
    ('市场部', 2400000, '上海'),
    ('财务部', 800000, '北京');

INSERT INTO employees (name, dept_id, title, salary, hired, active) VALUES
    ('张伟',   1, '架构师',   42000, '2019-03-11', TRUE),
    ('李娜',   1, '后端工程师', 28000, '2020-07-01', TRUE),
    ('王强',   1, '前端工程师', 26000, '2021-09-15', TRUE),
    ('刘洋',   2, '产品经理',  30000, '2018-05-20', TRUE),
    ('陈静',   2, '交互设计',  22000, '2022-02-14', FALSE),
    ('赵磊',   3, '市场总监',  38000, '2017-11-02', TRUE),
    ('孙悦',   3, '市场专员',  16000, '2023-03-06', TRUE),
    ('周涛',   3, '市场专员',  15500, '2023-08-21', TRUE),
    ('吴敏',   4, '财务经理',  26000, '2016-01-30', TRUE),
    ('郑凯',   4, '会计',     14000, '2021-06-18', TRUE),
    ('冯雪',   NULL, '实习生',  8000, '2024-07-01', TRUE),
    ('蒋楠',   NULL, '顾问',   50000, '2024-01-09', FALSE);

-- 订单 / 订单行：多一层「一对多再一对多」。
--
-- 原来示例只有「部门-员工」一条外键，关系图打开就是两张表一根线，
-- 看不出分组和层级；补上这条链之后，员工→订单→订单行是一条能看懂的
-- 三级结构，也顺带给帮助里的 JOIN 例子多一个现成的对象。
CREATE TABLE orders (
    id          SERIAL PRIMARY KEY,
    employee_id INTEGER REFERENCES employees(id),
    total       NUMERIC(12,2) NOT NULL DEFAULT 0,
    status      TEXT,
    created     DATE
);

INSERT INTO orders (employee_id, total, status, created) VALUES
    (1,  12800.00, 'paid',    '2026-09-02'),
    (2,   3400.00, 'paid',    '2026-09-05'),
    (3,    860.00, 'paid',    '2026-09-11'),
    (4,   3600.00, 'paid',    '2026-09-18'),
    (6,  21000.00, 'pending', '2026-10-02'),
    (7,   1250.00, 'paid',    '2026-10-09'),
    (9,   1200.00, 'paid',    '2026-10-15'),
    (10,  7600.00, 'pending', '2026-10-21');

CREATE TABLE order_items (
    id       SERIAL PRIMARY KEY,
    order_id INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    product  TEXT NOT NULL,
    qty      INTEGER NOT NULL DEFAULT 1,
    price    NUMERIC(10,2)
);

INSERT INTO order_items (order_id, product, qty, price) VALUES
    (1, '服务器托管',   1, 11800.00),
    (1, 'IP 地址',      4,   250.00),
    (2, '对象存储 1TB', 1,  3400.00),
    (3, '短信包',       1,   860.00),
    (4, '域名续费',     3,  1200.00),
    (5, '专线带宽',     1, 18000.00),
    (5, '机柜租用',     2,  1500.00),
    (6, '短信包',       1,  1250.00),
    (7, '域名续费',     1,  1200.00);

-- 向量示例：一小组「文档 + 8 维 embedding」，四个主题各占一个方向。
--
-- 为什么带这张表：向量那一节的所有例子（Top-K、按距离筛、1 - 距离 = 相似度）
-- 原来都要先 DROP/CREATE 一张 demo_vec 才有数据可试。示例库里直接放一份，
-- 打开就能照着帮助跑。8 维是刻意选的：小到能在屏幕上把每个数字读一遍，
-- 又足够让「同一主题排在一起」这件事看得出来。
--
-- 向量列不能建索引 / 主键 / 唯一约束（距离不是顺序，见帮助「向量检索」），
-- 所以这里只把 id 设成 SERIAL 主键。
CREATE TABLE articles (
    id        SERIAL PRIMARY KEY,
    title     TEXT NOT NULL,
    topic     TEXT,
    embedding VECTOR(8)
);

INSERT INTO articles (title, topic, embedding) VALUES
    ('向量索引的取舍：HNSW 还是 IVFFlat', 'tech',
     '[0.88,0.10,0.03,0.01,0.06,0.09,0.02,0.01]'),
    ('为什么我们暂时没用近似检索',       'tech',
     '[0.91,0.07,0.01,0.02,0.04,0.11,0.03,0.02]'),
    ('把 embedding 存进关系库的三个坑',   'tech',
     '[0.86,0.12,0.04,0.01,0.08,0.07,0.01,0.03]'),
    ('新版本需求评审纪要',               'prod',
     '[0.04,0.87,0.11,0.03,0.02,0.06,0.09,0.01]'),
    ('用户体验地图 v2',                  'prod',
     '[0.06,0.90,0.08,0.01,0.03,0.04,0.07,0.02]'),
    ('第三季度增长复盘',                 'mkt',
     '[0.02,0.05,0.85,0.13,0.04,0.03,0.06,0.02]'),
    ('渠道投放的边际收益',               'mkt',
     '[0.01,0.07,0.89,0.10,0.02,0.02,0.04,0.01]'),
    ('预算编制指引',                     'fin',
     '[0.03,0.02,0.09,0.88,0.01,0.02,0.05,0.04]'),
    ('应收账款周转分析',                 'fin',
     '[0.02,0.04,0.12,0.90,0.03,0.01,0.03,0.02]');

-- 注释：侧栏会在表名下面显示这一行。示例表尤其需要 —— 「articles」这名字
-- 本身说明不了里面存的是向量，也说明不了该怎么查。
COMMENT ON TABLE articles IS '示例：9 篇文档 + 8 维向量，演示按相似度取前 K 条';
COMMENT ON COLUMN articles.topic IS 'tech / prod / mkt / fin';
COMMENT ON COLUMN articles.embedding IS '8 维向量（四类主题各占一个方向），用 <=> 算余弦距离';

-- 标签：让 articles 也有一个子表。关系图里「一个分组至少要有两个节点」，
-- 否则每张表各成一组，看不出分组本身的意义。
CREATE TABLE tags (
    id         SERIAL PRIMARY KEY,
    article_id INTEGER REFERENCES articles(id) ON DELETE CASCADE,
    name       TEXT NOT NULL
);

INSERT INTO tags (article_id, name) VALUES
    (1, 'vector'), (1, 'index'), (2, 'vector'), (4, 'prd'),
    (5, 'design'), (6, 'growth'), (8, 'finance');

COMMENT ON TABLE orders IS '示例：订单，employee_id 指向 employees';
COMMENT ON TABLE order_items IS '示例：订单行，order_id 指向 orders（级联删除）';
COMMENT ON TABLE tags IS '示例：文章标签，article_id 指向 articles';
`

// LoadSample 载入示例数据（会先清空所有表）。
func (d *DB) LoadSample(sess *txn.Session) error {
	if err := d.DropAll(); err != nil {
		return err
	}
	if _, err := d.Exec(sess, SampleSQL); err != nil {
		return err
	}
	return nil
}

// RandomEmployees 生成用于压测的随机员工数据行（未使用，保留扩展点）。
func RandomEmployees(n int, seed int64) [][6]interface{} {
	r := rand.New(rand.NewSource(seed))
	names := []string{"张", "李", "王", "刘", "陈", "杨", "赵", "周", "吴", "徐"}
	titles := []string{"工程师", "高级工程师", "专家", "经理", "实习生"}
	out := make([][6]interface{}, 0, n)
	base := time.Date(2018, 1, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < n; i++ {
		out = append(out, [6]interface{}{
			fmt.Sprintf("%s%d", names[r.Intn(len(names))], i),
			r.Intn(5),
			titles[r.Intn(len(titles))],
			8000 + r.Intn(40000),
			base.AddDate(0, r.Intn(72), 0).Format("2006-01-02"),
			r.Intn(5) != 0,
		})
	}
	return out
}
