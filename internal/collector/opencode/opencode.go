// Package opencode 实现 OpenCode 的数据适配器。
//
// OpenCode 把会话写入本地 SQLite：
//   ~/.local/share/opencode/opencode.db
// 同样以只读模式打开正在运行的库，避免锁冲突。
package opencode

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

const adapterName = "opencode"

func init() {
	_ = collector.Register(&Adapter{})
}

// Adapter 实现 collector.Adapter。
type Adapter struct{}

// Name 返回适配器标识。
func (a *Adapter) Name() string { return adapterName }

// Discover 找到独立安装的 OpenCode 数据目录下的 opencode.db。
// 仅认标准路径 ~/.local/share/opencode/opencode.db（或环境变量覆盖）。
func (a *Adapter) Discover() ([]string, error) {
	if v := os.Getenv("TOKENPULSE_OPENCODE_DIR"); v != "" {
		p := filepath.Join(v, "opencode.db")
		if _, err := os.Stat(p); err == nil {
			return []string{p}, nil
		}
		return nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	p := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if _, err := os.Stat(p); err == nil {
		return []string{p}, nil
	}
	return nil, nil
}

// messageData 与 TeleAgent 同源（OpenCode 数据模型）。
type messageData struct {
	Model    string `json:"modelID"`
	Role     string `json:"role"`
	Tokens   struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// Parse 以只读模式打开 opencode.db。
func (a *Adapter) Parse(path string) ([]model.UsageEvent, error) {
	uri := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("opencode: open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	rows, err := db.Query(`SELECT m.id, m.session_id, m.time_created, m.data,
		COALESCE(s.directory, ''), COALESCE(s.title, '')
		FROM message m
		LEFT JOIN session s ON s.id = m.session_id`)
	if err != nil {
		return nil, fmt.Errorf("opencode: query %s: %w", path, err)
	}
	defer rows.Close()

	var events []model.UsageEvent
	for rows.Next() {
		var (
			id, sid, dir, title, raw string
			created                  int64
			data                     messageData
		)
		if err := rows.Scan(&id, &sid, &created, &raw, &dir, &title); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			continue
		}
		if data.Role != "assistant" || (data.Tokens.Input <= 0 && data.Tokens.Output <= 0) {
			continue
		}
		project := filepath.Base(dir)
		if project == "" || project == "." {
			if title != "" && !strings.HasPrefix(title, "_SYS_") {
				project = title
			} else {
				project = "(unknown)"
			}
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
