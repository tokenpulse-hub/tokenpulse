// Package teleagent 实现 TeleAgent 桌面智能体的日志适配器。
//
// TeleAgent 把会话写入本地 SQLite：
//   ~/.local/share/TeleAgent/users/<uid>/teleagent.db
// message 表的 data 字段是 JSON，其中携带：
//   tokens.input / tokens.output / tokens.cache.{read,write}
//   modelID / providerID / cost（TeleAgent 通常不折算成本，恒为 0，
//   成本由 TokenPulse 的定价表补算——这正是本项目的价值之一）。
//
// 注意：该库可能正被运行中的 TeleAgent 占用，因此必须以
// 只读模式（mode=ro）打开，绝不写入，避免锁冲突。
package teleagent

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tokenpulse-hub/tokenpulse/internal/collector"
	"github.com/tokenpulse-hub/tokenpulse/internal/model"
)

const adapterName = "teleagent"

func init() {
	_ = collector.Register(&Adapter{})
}

// Adapter 实现 collector.Adapter。
type Adapter struct{}

// Name 返回适配器标识。
func (a *Adapter) Name() string { return adapterName }

// Discover 找到所有用户目录下的 teleagent.db。
// 目录不存在（未安装 TeleAgent）返回空列表。
func (a *Adapter) Discover() ([]string, error) {
	root := usersRoot()
	if root == "" {
		return nil, nil
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name(), "teleagent.db")
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	return files, nil
}

// usersRoot 返回 TeleAgent 用户数据根目录（支持环境变量覆盖，便于测试）。
func usersRoot() string {
	if v := os.Getenv("TOKENPULSE_TELEAGENT_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "TeleAgent", "users")
}

// messageData 只声明解析所需字段，未声明字段自动忽略。
type messageData struct {
	Model     string `json:"modelID"`
	Provider  string `json:"providerID"`
	Role      string `json:"role"`
	Tokens    struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// Parse 以只读模式打开 teleagent.db，把带用量的 assistant 消息转成统一事件。
// JSON 解析放在 Go 端而非依赖 SQL 的 json_extract，兼容性更稳。
func (a *Adapter) Parse(path string) ([]model.UsageEvent, error) {
	// TeleAgent 可能正在运行并持有写锁：只读 + busy_timeout 等待写事务释放。
	uri := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("teleagent: open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	rows, err := db.Query(`SELECT m.id, m.session_id, m.time_created, m.data,
		COALESCE(s.directory, ''), COALESCE(p.name, ''), COALESCE(s.title, '')
		FROM message m
		LEFT JOIN session s ON s.id = m.session_id
		LEFT JOIN project p ON p.id = s.project_id`)
	if err != nil {
		return nil, fmt.Errorf("teleagent: query %s: %w", path, err)
	}
	defer rows.Close()

	var events []model.UsageEvent
	for rows.Next() {
		var (
			id, sid, dir, pname, title, raw string
			created                       int64
			data                          messageData
		)
		if err := rows.Scan(&id, &sid, &created, &raw, &dir, &pname, &title); err != nil {
			continue // 单行失败跳过
		}
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			continue
		}
		if data.Role != "assistant" || (data.Tokens.Input <= 0 && data.Tokens.Output <= 0) {
			continue // 只统计有真实用量的模型响应
		}
		// 项目名降级链：项目名 → 目录名 → 会话标题 → (unknown)。
		// 目录优先：会话数量会持续增长，按工作空间聚合才能保持面板清爽；
		// 无目录的会话（如 _SYS_MEMORY_ 记忆维护等系统任务）才落到标题兜底。
		// _SYS_MEMORY_ 前缀的系统会话每次都是独立 session、没有共享目录，
		// 统一归为"TeleAgent 系统维护"一个桶，避免随时间无限膨胀。
		project := pname
		if project == "" && dir != "" {
			project = filepath.Base(dir)
		}
		if project == "" && title != "" {
			if strings.HasPrefix(title, "_SYS_MEMORY_") {
				project = "TeleAgent 系统维护"
			} else {
				project = title
			}
		}
		if project == "" {
			project = "(unknown)"
		}
		events = append(events, model.UsageEvent{
			EventID:      adapterName + ":" + id,
			Timestamp:    time.UnixMilli(created).UTC(),
			Tool:         adapterName,
			Model:        data.Model,
			Project:      project,
			SessionID:    sid,
			InputTokens:     data.Tokens.Input,
			OutputTokens:    data.Tokens.Output,
			CacheReadTokens:  data.Tokens.Cache.Read,
			CacheWriteTokens: data.Tokens.Cache.Write,
		})
	}
	return events, rows.Err()
}
