// Package store 实现基于 SQLite 的用量事件存储。
// 使用 modernc.org/sqlite（纯 Go 实现，无 CGO），保证交叉编译出静态单二进制。
package store

import (
	"database/sql"
	"fmt"
	"time"

	// 纯 Go SQLite 驱动（无 CGO），blank import 触发 database/sql 驱动注册。
	_ "modernc.org/sqlite"

	"github.com/tokenpulse-hub/tokenpulse/internal/model"
	"github.com/tokenpulse-hub/tokenpulse/internal/pricing"
)

// Store 封装 SQLite 句柄。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）数据库并确保表结构就绪。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite 单写者模型：串行写、并发读。
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS usage_events (
  event_id      TEXT PRIMARY KEY,
  ts            TEXT NOT NULL,
  tool          TEXT NOT NULL,
  model         TEXT NOT NULL,
  project       TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_tokens  INTEGER NOT NULL DEFAULT 0,
  cost_usd      REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_ts      ON usage_events(ts);
CREATE INDEX IF NOT EXISTS idx_usage_tool    ON usage_events(tool);
CREATE INDEX IF NOT EXISTS idx_usage_model   ON usage_events(model);
CREATE INDEX IF NOT EXISTS idx_usage_project ON usage_events(project);
`
	if _, err := s.db.Exec(ddl); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Clear 清空全部事件数据。配合 scan --reset 使用，用于解析逻辑变更后重建。
// 使用 TRUNCATE 比 DELETE 快（不写回滚日志），但要求无活动事务——调用方需确保串行。
func (s *Store) Clear() error {
	_, err := s.db.Exec(`DELETE FROM usage_events`)
	return err
}

// RecalcCosts 按给定定价表重算库内全部历史事件的成本并写回。
// 定价是线性的（成本 = Σtoken × 单价），因此按模型分组在 SQL 内直接算术重算，
// 每行各自精确更新，避免逐条 Go 往返。未命中定价的模型统一归零。
// 返回受影响的行数。整个重算包在一个事务里：要么全部生效，要么全部回滚。
func (s *Store) RecalcCosts(t *pricing.Table) (int64, error) {
	rows, err := s.db.Query(`SELECT DISTINCT model FROM usage_events`)
	if err != nil {
		return 0, fmt.Errorf("store: recalc distinct models: %w", err)
	}
	var models []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return 0, err
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var affected int64
	// 与 scan 时的折算口径一致：CacheTokens 按 cache_read 单价计，cache_write 按 0。
	const perM = 1000000.0
	for _, m := range models {
		p, ok := t.Match(m)
		var res sql.Result
		if ok {
			res, err = tx.Exec(`UPDATE usage_events SET cost_usd =
				input_tokens/?*? + output_tokens/?*? + cache_tokens/?*?
				WHERE model = ?`,
				perM, p.Input, perM, p.Output, perM, p.CacheRead, m)
		} else {
			res, err = tx.Exec(`UPDATE usage_events SET cost_usd = 0 WHERE model = ?`, m)
		}
		if err != nil {
			return 0, fmt.Errorf("store: recalc %s: %w", m, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			affected += n
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return affected, nil
}

// InsertEvents 批量写入事件。event_id 冲突（重复扫描）自动忽略，
// 返回本次真正新插入的行数——增量导入幂等的关键。
func (s *Store) InsertEvents(events []model.UsageEvent) (int64, error) {
	if len(events) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	const q = `INSERT OR IGNORE INTO usage_events
		(event_id, ts, tool, model, project, session_id,
		 input_tokens, output_tokens, cache_tokens, cost_usd)
		VALUES (?,?,?,?,?,?,?,?,?,?)`
	var inserted int64
	for _, e := range events {
		res, err := tx.Exec(q,
			e.EventID, e.Timestamp.UTC().Format(time.RFC3339Nano),
			e.Tool, e.Model, e.Project, e.SessionID,
			e.InputTokens, e.OutputTokens, e.CacheTokens, e.CostUSD)
		if err != nil {
			return 0, fmt.Errorf("store: insert %s: %w", e.EventID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted += n
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

// Range 是一个时间区间内的聚合结果。
type Range struct {
	Label        string  `json:"label"` // today / week / month
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheTokens  int64   `json:"cache_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Calls        int64   `json:"calls"`
}

// Summary 返回今日 / 本周（周一起） / 本自然月三个区间的聚合。
// tool 为空时统计全部工具；非空时只统计指定工具。
func (s *Store) Summary(now time.Time, tool string) (today, week, month Range, err error) {
	local := now.Local()

	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	weekStart := dayStart
	if offset := (int(weekStart.Weekday()) + 6) % 7; offset > 0 { // 周一=0
		weekStart = weekStart.AddDate(0, 0, -offset)
	}
	monthStart := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, local.Location())

	if today, err = s.rangeAgg("today", dayStart, now, tool); err != nil {
		return
	}
	if week, err = s.rangeAgg("week", weekStart, now, tool); err != nil {
		return
	}
	month, err = s.rangeAgg("month", monthStart, now, tool)
	return
}

func (s *Store) rangeAgg(label string, from, to time.Time, tool string) (Range, error) {
	q := `SELECT
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_tokens),0), COALESCE(SUM(cost_usd),0), COUNT(*)
		FROM usage_events WHERE ts >= ? AND ts < ?`
	args := []any{from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}
	if tool != "" {
		q += ` AND tool = ?`
		args = append(args, tool)
	}
	var r Range
	err := s.db.QueryRow(q, args...).
		Scan(&r.InputTokens, &r.OutputTokens, &r.CacheTokens, &r.CostUSD, &r.Calls)
	r.Label = label
	r.TotalTokens = r.InputTokens + r.OutputTokens + r.CacheTokens
	return r, err
}

// Bucket 是按模型或项目的聚合桶。
type Bucket struct {
	Name         string  `json:"name"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheTokens  int64   `json:"cache_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Calls        int64   `json:"calls"`
}

// GroupBy 返回指定起始时间之后、按指定维度（"model"/"project"/"tool"）的聚合，
// 按成本降序取前 limit 名。tool 为空时统计全部工具。
func (s *Store) GroupBy(dimension string, from time.Time, limit int, tool string) ([]Bucket, error) {
	switch dimension {
	case "model", "project", "tool":
	default:
		return nil, fmt.Errorf("store: invalid dimension %q", dimension)
	}
	q := `SELECT %s,
		SUM(input_tokens), SUM(output_tokens), SUM(cache_tokens),
		SUM(input_tokens)+SUM(output_tokens)+SUM(cache_tokens),
		SUM(cost_usd), COUNT(*)
		FROM usage_events WHERE ts >= ?`
	args := []any{from.UTC().Format(time.RFC3339Nano)}
	if tool != "" {
		q += ` AND tool = ?`
		args = append(args, tool)
	}
	q += ` GROUP BY %s ORDER BY SUM(cost_usd) DESC, 5 DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(fmt.Sprintf(q, dimension, dimension), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Name, &b.InputTokens, &b.OutputTokens, &b.CacheTokens,
			&b.TotalTokens, &b.CostUSD, &b.Calls); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DayPoint 是按天的趋势点（本地日历日）。
type DayPoint struct {
	Day         string  `json:"day"` // YYYY-MM-DD（本地时区）
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

// DailyTrend 返回最近 days 天（含今天）的逐日趋势，空日期补零点，
// 保证前端画出的曲线连续。tool 为空时统计全部工具。
func (s *Store) DailyTrend(now time.Time, days int, tool string) ([]DayPoint, error) {
	if days <= 0 {
		days = 30
	}
	local := now.Local()
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())

	q := `SELECT
		date(ts, 'localtime'), SUM(input_tokens)+SUM(output_tokens)+SUM(cache_tokens), SUM(cost_usd)
		FROM usage_events WHERE ts >= ?`
	args := []any{today.AddDate(0, 0, -(days - 1)).UTC().Format(time.RFC3339Nano)}
	if tool != "" {
		q += ` AND tool = ?`
		args = append(args, tool)
	}
	q += ` GROUP BY date(ts, 'localtime')`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := make(map[string]DayPoint, days)
	for rows.Next() {
		var p DayPoint
		if err := rows.Scan(&p.Day, &p.TotalTokens, &p.CostUSD); err != nil {
			return nil, err
		}
		byDay[p.Day] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]DayPoint, 0, days)
	for i := days - 1; i >= 0; i-- {
		d := today.AddDate(0, 0, -i)
		key := d.Format("2006-01-02")
		if p, ok := byDay[key]; ok {
			out = append(out, p)
		} else {
			out = append(out, DayPoint{Day: key})
		}
	}
	return out, nil
}

// ToolStat 是一个工具的汇总信息（用于前端筛选条上的标签与数字）。
type ToolStat struct {
	Tool        string `json:"tool"`
	TotalTokens int64  `json:"total_tokens"`
	Calls       int64  `json:"calls"`
}

// DistinctTools 返回本月所有出现过的工具及其汇总数据，按 Token 降序。
// 用于前端工具筛选条：显示每个工具的标签和消耗量概览。
func (s *Store) DistinctTools(now time.Time) ([]ToolStat, error) {
	local := now.Local()
	monthStart := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, local.Location())
	rows, err := s.db.Query(`SELECT tool,
		SUM(input_tokens)+SUM(output_tokens)+SUM(cache_tokens), COUNT(*)
		FROM usage_events WHERE ts >= ?
		GROUP BY tool ORDER BY 2 DESC`,
		monthStart.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolStat
	for rows.Next() {
		var t ToolStat
		if err := rows.Scan(&t.Tool, &t.TotalTokens, &t.Calls); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
