// Package claude 实现 Claude Code 的日志适配器。
// Claude Code 会把每个会话写成一个 JSONL 文件，位于：
//   ~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl
// 其中 type=="assistant" 的行携带 message.usage（Token 计数）与 message.model。
package claude

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tokenpulse-hub/tokenpulse/internal/collector"
	"github.com/tokenpulse-hub/tokenpulse/internal/model"
)

const (
	adapterName = "claude-code"
)

func init() {
	// 通过 collector.Register 挂进全局注册表；重复注册视为程序性错误。
	_ = collector.Register(&Adapter{})
}

// Adapter 实现 collector.Adapter。
type Adapter struct{}

// Name 返回适配器标识。
func (a *Adapter) Name() string { return adapterName }

// Discover 找到 ~/.claude/projects 下所有 .jsonl 会话文件。
// 目录不存在（未安装/未使用 Claude Code）返回空列表。
func (a *Adapter) Discover() ([]string, error) {
	root := ProjectsDir()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单目录失败不影响整体扫描
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// ProjectsDir 返回 Claude Code 会话日志根目录（支持环境变量覆盖，便于测试）。
func ProjectsDir() string {
	if v := os.Getenv("TOKENPULSE_CLAUDE_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// claudeLine 只声明关心的字段，未声明字段自动忽略——上游新增字段不会破坏解析。
type claudeLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Message   struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// Parse 逐行解析一个 JSONL 会话文件。
// 只关心 assistant 行；单行解析失败跳过，不中断整个文件。
func (a *Adapter) Parse(path string) ([]model.UsageEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 项目名优先从文件路径推断（~/.claude/projects/<encoded-cwd>/），
	// 行内 cwd 字段更准时覆盖它。
	project := projectFromPath(path)

	var events []model.UsageEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024) // 单行上限 16MB，长对话兜底
	for sc.Scan() {
		var line claudeLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue // 坏行/非 JSON 行：跳过
		}
		if line.Type != "assistant" {
			continue // 只统计模型侧响应行（usage 挂在 assistant 消息上）
		}
		u := line.Message.Usage
		if u.InputTokens == 0 && u.OutputTokens == 0 &&
			u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0 {
			continue // 无用量数据的事件没有记账意义
		}
		ts := parseTime(line.Timestamp)
		session := line.SessionID
		if session == "" {
			session = strings.TrimSuffix(filepath.Base(path), ".jsonl")
		}
		if line.Cwd != "" {
			project = filepath.Base(line.Cwd)
		}
		ev := model.UsageEvent{
			EventID:      adapterName + ":" + session + ":" + line.UUID,
			Timestamp:    ts,
			Tool:         adapterName,
			Model:        line.Message.Model,
			Project:      project,
			SessionID:    session,
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			CacheTokens:  u.CacheReadInputTokens + u.CacheCreationInputTokens,
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return events, err
	}
	return events, nil
}

// projectFromPath 从 ~/.claude/projects/-Users-foo-myproject/x.jsonl
// 提取出 "myproject"。编码规则是把 cwd 中的路径分隔符替换成 '-'，
// 无法精确还原层级，取最后一段即可满足"项目名"展示需求。
func projectFromPath(path string) string {
	dir := filepath.Base(filepath.Dir(path))
	parts := strings.Split(dir, "-")
	if len(parts) == 0 {
		return dir
	}
	name := parts[len(parts)-1]
	if name == "" {
		return dir
	}
	return name
}

// parseTime 容错解析时间戳；失败时返回零值，由存储层丢弃。
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
