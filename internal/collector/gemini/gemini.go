// Package gemini 实现 Google Gemini CLI 的数据适配器。
//
// Gemini CLI 把每个会话写入 JSON 文件（旧版 .json，新版 .jsonl）：
//   ~/.gemini/tmp/<hash>/session-*.json
// 或 ~/.gemini/tmp/<hash>/session-*.jsonl
//
// .json 格式：整个文件一个 JSON 对象，含 conversation 历史。
// .jsonl 格式：逐行事件流，含 turn 完成、token 用量等信息。
//
// 单文件解析失败跳过，不影响其他文件。
package gemini

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

const adapterName = "gemini-cli"

func init() {
	_ = collector.Register(&Adapter{})
}

// Adapter 实现 collector.Adapter。
type Adapter struct{}

// Name 返回适配器标识。
func (a *Adapter) Name() string { return adapterName }

// Discover 扫描 ~/.gemini/tmp/ 下所有 session-*.json / .jsonl 文件。
func (a *Adapter) Discover() ([]string, error) {
	root := tmpDir()
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
		base := filepath.Base(path)
		if strings.HasPrefix(base, "session-") &&
			(strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".jsonl")) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// tmpDir 返回 Gemini CLI 临时会话目录。
func tmpDir() string {
	if v := os.Getenv("TOKENPULSE_GEMINI_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "tmp")
}

// geminiUsage 容错式声明 TurnReport / API 响应中的 Token 用量结构。
type geminiUsage struct {
	PromptTokens     int64 `json:"promptTokens"`     // 输入
	CandidatesTokens int64 `json:"candidatesTokens"` // 输出
	CachedTokens     int64 `json:"cachedTokens"`     // 缓存命中
	TotalTokens      int64 `json:"totalTokens"`      // 总计
}

// jsonlLine 新版 JSONL 事件流中可能携带用量的行结构。
type jsonlLine struct {
	Type      string `json:"type"` // 如 "gemini_api_call" / "turn_report" 等
	Timestamp string `json:"timestamp"`
	Model     string `json:"model"`
	Usage     struct {
		geminiUsage
	} `json:"usage"`
	// 部分 Gemini CLI 版本把用量挂在顶层或嵌套在不同路径，容错提取多个候选位。
	TokenUsage *geminiUsage `json:"tokenUsage"`
}

// sessionJSON 旧版 .json 会话文件顶层结构。
type sessionJSON struct {
	Model        string       `json:"model"`
	Turns        []turnEntry  `json:"turns"`
	TokenSummary *geminiUsage `json:"tokenSummary"`
}

type turnEntry struct {
	Timestamp string       `json:"timestamp"`
	Model     string       `json:"model"`
	Usage     *geminiUsage `json:"usage"`
}

// Parse 解析一个 Gemini CLI 会话文件（自动区分 .json 与 .jsonl）。
func (a *Adapter) Parse(path string) ([]model.UsageEvent, error) {
	if strings.HasSuffix(path, ".jsonl") {
		return parseJSONL(path)
	}
	return parseJSON(path)
}

func parseJSONL(path string) ([]model.UsageEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	session := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var events []model.UsageEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		var line jsonlLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		u := line.Usage.geminiUsage
		if line.TokenUsage != nil {
			u = *line.TokenUsage
		}
		in := u.PromptTokens - u.CachedTokens
		if in < 0 {
			in = u.PromptTokens
		}
		if in <= 0 && u.CandidatesTokens <= 0 {
			continue
		}
		ts := parseTime(line.Timestamp)
		if ts.IsZero() {
			ts = fileModTime(path)
		}
		events = append(events, model.UsageEvent{
			EventID:      adapterName + ":" + session + ":" + itoa(lineNo),
			Timestamp:    ts,
			Tool:         adapterName,
			Model:        line.Model,
			Project:      projectFromPath(path),
			SessionID:    session,
			InputTokens:  in,
			OutputTokens: u.CandidatesTokens,
			CacheReadTokens:  u.CachedTokens,
			CacheWriteTokens: 0,
		})
	}
	return events, sc.Err()
}

func parseJSON(path string) ([]model.UsageEvent, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sess sessionJSON
	if err := json.Unmarshal(raw, &sess); err != nil {
		return nil, err // 整个文件不是合法 JSON：跳过（返回 error 由调用方 continue）
	}

	session := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var events []model.UsageEvent

	// 路径一：turns 数组里逐轮携带 usage
	for i, t := range sess.Turns {
		if t.Usage == nil {
			continue
		}
		u := *t.Usage
		in := u.PromptTokens - u.CachedTokens
		if in < 0 {
			in = u.PromptTokens
		}
		if in <= 0 && u.CandidatesTokens <= 0 {
			continue
		}
		ts := parseTime(t.Timestamp)
		if ts.IsZero() {
			ts = fileModTime(path)
		}
		m := t.Model
		if m == "" {
			m = sess.Model
		}
		events = append(events, model.UsageEvent{
			EventID:      adapterName + ":" + session + ":" + itoa(i),
			Timestamp:    ts,
			Tool:         adapterName,
			Model:        m,
			Project:      projectFromPath(path),
			SessionID:    session,
			InputTokens:  in,
			OutputTokens: u.CandidatesTokens,
			CacheReadTokens:  u.CachedTokens,
			CacheWriteTokens: 0,
		})
	}
	return events, nil
}

func projectFromPath(path string) string {
	// ~/.gemini/tmp/<hash>/session-xxx.json → 无法取到真实项目名，
	// Gemini CLI 不在会话文件里存 cwd；用会话文件名作项目名。
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

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

func fileModTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime().UTC()
	}
	return time.Now().UTC()
}

func itoa(n int) string { return strconv.Itoa(n) }
