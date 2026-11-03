package db

import (
	"fmt"
	"strings"
	"testing"
)

// 向量扫描路径的基准：2 万行 × 128 维（.vec 文件 10.2 MB）。
//
// 规模刻意压小：基准的价值在「改动前后对比」，不在绝对数字。2 万行时
// 建数据只要 4 秒，一条基准 5 秒内跑完；需要压 100 万行的量级时改
// vecBenchRows 即可（建数据约 20 秒/10 万行）。
//
//	go test ./db/ -run XXX -bench Vec -benchtime 5x -cpuprofile vec.prof
//	go tool pprof -top vec.prof
//
// 这条基准存在的理由：`.vec` 是定长槽位文件，顺序扫的理论上限由存储层决定
// （storage 包的映射视图单线程 8.1 GB/s、单槽读 0.30 µs/行）。exec 层每行
// 还要走「火山模型 Next + Row 装箱 + 表达式求值」，一旦有人在这条路上加东西，
// 这里立刻能看出多付了多少。
//
// 三个子基准对应三段成本：
//   - ScanOnly ：只 count(*)，不碰向量列 —— 纯行扫描基线；
//   - Filter   ：WHERE 距离 < 10 —— 每行都要读向量 + 算距离；
//   - TopK     ：ORDER BY 距离 LIMIT 10 —— 在上面加一个 Top-N 堆。
func vecBenchDB(b *testing.B, n, dim int) *DB {
	b.Helper()
	d, err := Open(b.TempDir(), testOptions())
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { d.Close() })
	sess := d.Session("bench")
	if _, err := d.Exec(sess, fmt.Sprintf(`CREATE TABLE vb (id INT4 PRIMARY KEY, e VECTOR(%d))`, dim)); err != nil {
		b.Fatalf("create: %v", err)
	}
	// 确定性伪随机：不依赖 math/rand 的默认种子，换 Go 版本结果也一样。
	vec := func(seed int) string {
		var sb strings.Builder
		sb.WriteByte('[')
		for i := 0; i < dim; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			// 线性同余，落在 [-1,1)
			v := (int64(seed)*2654435761 + int64(i)*40503) % 20000
			if v < 0 {
				v += 20000
			}
			fmt.Fprintf(&sb, "%.4f", float64(v)/10000.0-1)
		}
		sb.WriteByte(']')
		return sb.String()
	}
	const batch = 500
	for start := 0; start < n; start += batch {
		var sb strings.Builder
		sb.WriteString("INSERT INTO vb VALUES ")
		for i := start; i < start+batch && i < n; i++ {
			if i > start {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "(%d,'%s')", i, vec(i))
		}
		if _, err := d.Exec(sess, sb.String()); err != nil {
			b.Fatalf("insert at %d: %v", start, err)
		}
	}
	return d
}

func benchQueryVec(n, dim int) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < dim; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%.4f", float64(i%17)/8.0-1)
	}
	sb.WriteByte(']')
	return sb.String()
}

const (
	vecBenchRows = 20000
	vecBenchDim  = 128
)

func BenchmarkVecScanOnly(b *testing.B) {
	d := vecBenchDB(b, vecBenchRows, vecBenchDim)
	sess := d.Session("bench")
	// 先跑一次，把表从磁盘读进内存，量的是「热」成本。
	if _, err := d.Exec(sess, `SELECT count(*) FROM vb`); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(sess, `SELECT count(*) FROM vb`); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVecFilter(b *testing.B) {
	d := vecBenchDB(b, vecBenchRows, vecBenchDim)
	sess := d.Session("bench")
	q := benchQueryVec(vecBenchRows, vecBenchDim)
	sql := fmt.Sprintf(`SELECT count(*) FROM vb WHERE e <-> '%s' < 12`, q)
	if _, err := d.Exec(sess, sql); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(sess, sql); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVecTopK(b *testing.B) {
	d := vecBenchDB(b, vecBenchRows, vecBenchDim)
	sess := d.Session("bench")
	q := benchQueryVec(vecBenchRows, vecBenchDim)
	sql := fmt.Sprintf(`SELECT id, e <-> '%s' AS d FROM vb ORDER BY d LIMIT 10`, q)
	if _, err := d.Exec(sess, sql); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(sess, sql); err != nil {
			b.Fatal(err)
		}
	}
}
