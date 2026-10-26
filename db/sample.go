package db

import (
	"fmt"
	"math/rand"
	"time"

	"godb/txn"
)

// SampleSQL 是演示数据集：两张表，足以演示 JOIN、聚合、事务。
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
