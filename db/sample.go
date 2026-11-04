package db

import (
	"fmt"
	"math/rand"
	"time"

	"godb/txn"
)

// SampleSQL 是演示数据集：部门/员工两张表（JOIN、聚合、事务），
// 外加一张 articles（文档 + 8 维向量），让「按相似度取前 K 条」打开就能试。
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
    dept_id  INTEGER,
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
