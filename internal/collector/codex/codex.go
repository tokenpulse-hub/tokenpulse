// Package codex 实现 OpenAI Codex CLI 的数据适配器。
//
// Codex CLI 把会话写入 JSONL 事件流文件：
//   ~/.codex/sessions/<session-hash>/rollout-*.jsonl
//   ~/.codex/sessions/session_index.jsonl（会话名称索引，可选）
// 每行一个 JSON 事件，关键事件：
//   - session_meta：含 cwd、git 分支
//   - event_msg / payload.type=token_count：Token 用量
//   - event_msg / payload.type=agent_message：助手回复（携带 model 信息）
//
// 单行解析失败跳过，坏文件不影响其他文件。
package codex

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tokenpulse-hub/tokenpulse/internal/collector"
	"github.com/tokenpulse-hub/tokenpulse/internal/model"
)

const adapterName = "codex"

func init() {
	_ = collector.Register(&Adapter{})
}

// Adapter 实现 collector.Adapter。
type Adapter struct{}

// Name 返回适配器标识。
func (a *Adapter) Name() string { return adapterName }

// Discover 扫描 ~/.codex/sessions/ 下所有 rollout-*.jsonl。
func (a *Adapter) Discover() ([]string, error) {
	root := sessionsDir()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(filepath.Base(path), "rollout-") && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// sessionsDir 返回 Codex 会话日志根目录。
func sessionsDir() string {
	if v := os.Getenv("TOKENPULSE_CODEX_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// codexLine 容错式声明：只提取关心的字段。
type codexLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type      string `json:"type"` // token_count / agent_message / user_message 等
		Message   string `json:"message"`
		Model     string `json:"model"`     // agent_message 事件的模型标识
		Input     int64  `json:"input"`     // token_count：输入
		CacheRead int64  `json:"cached"`    // token_count：缓存命中
		Output    int64  `json:"output"`    // token_count：输出
		Cwd       string `json:"cwd"`       // session_meta：工作目录
	} `json:"payload"`
}

// sessionState 在逐行解析过程中累积会话上下文。
type sessionState struct {
	cwd     string // 从 session_meta 提取的工作目录
	model   string // 最近一次 agent_message 携带的模型名
	session string // 从文件路径推断的会话 ID
}

// Parse 解析一个 rollout-*.jsonl 事件流文件。
// token_count 事件是 Token 用量的权威来源；
// session_meta 提供 cwd（用于项目名），agent_message 提供模型名。
func (a *Adapter) Parse(path string) ([]model.UsageEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	state := &sessionState{}
	state.session = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	// 从父目录名提取会话哈希（rollout-<ts>-<hash>.jsonl → 取时间戳部分作为备用）
	dirName := filepath.Base(filepath.Dir(path))
	if dirName != "sessions" {
		state.session = dirName
	}

	var events []model.UsageEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		var line codexLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		switch line.Type {
		case "session_meta":
			if line.Payload.Cwd != "" {
				state.cwd = line.Payload.Cwd
			}
		case "event_msg":
			switch line.Payload.Type {
			case "agent_message":
				if line.Payload.Model != "" {
					state.model = line.Payload.Model
				}
			case "token_count":
				if line.Payload.Input <= 0 && line.Payload.Output <= 0 {
					continue
				}
				project := "(unknown)"
				if state.cwd != "" {
					project = filepath.Base(state.cwd)
				}
				ts := parseTime(line.Timestamp)
				if ts.IsZero() {
					ts = fileModTime(path)
				}
				events = append(events, model.UsageEvent{
					EventID:      adapterName + ":" + state.session + ":" + strconv.Itoa(lineNo),
					Timestamp:    ts,
					Tool:         adapterName,
					Model:        state.model,
					Project:      project,
					SessionID:    state.session,
					InputTokens:  line.Payload.Input,
					OutputTokens: line.Payload.Output,
					CacheTokens:  line.Payload.CacheRead,
				})
			}
		}
	}
	return events, sc.Err()
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func fileModTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime().UTC()
	}
	return time.Now().UTC()
}
