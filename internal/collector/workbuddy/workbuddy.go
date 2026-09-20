// Package workbuddy 实现 WorkBuddy 的数据适配器。
//
// WorkBuddy 把每次 Agent 工作流的执行追踪写入本地 trace 文件：
//   ~/.workbuddy/traces/<pid>/trace_<hash>.json
//
// 每个 trace 文件是一次 Agent 工作流（含多步 span），其中 type=generation
// 的 span 对应一次 LLM API 调用，其 toolOutput 是标准 OpenAI Chat Completion
// 响应，内含 model 和 usage（prompt_tokens / completion_tokens /
// prompt_tokens_details.cached_tokens）。
//
// 适配器遍历所有 trace 文件，提取 generation span 的 token 用量，
// 按模型定价表折算成本。只读访问，不修改任何文件。
package workbuddy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tokenpulse-hub/tokenpulse/internal/collector"
	"github.com/tokenpulse-hub/tokenpulse/internal/model"
)

const adapterName = "workbuddy"

func init() {
	_ = collector.Register(&Adapter{})
}

// Adapter 实现 collector.Adapter。
type Adapter struct{}

// Name 返回适配器标识。
func (a *Adapter) Name() string { return adapterName }

// Discover 找到 WorkBuddy 数据目录下的全部 trace JSON 文件。
// 标准路径 ~/.workbuddy/traces/<pid>/trace_*.json（或环境变量覆盖）。
func (a *Adapter) Discover() ([]string, error) {
	dir := os.Getenv("TOKENPULSE_WORKBUDDY_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil
		}
		dir = filepath.Join(home, ".workbuddy")
	}

	tracesDir := filepath.Join(dir, "traces")
	entries, err := os.ReadDir(tracesDir)
	if err != nil {
		return nil, nil // 目录不存在=工具未安装或未使用
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(tracesDir, e.Name())
		fs, err := filepath.Glob(filepath.Join(sub, "trace_*.json"))
		if err != nil {
			continue
		}
		files = append(files, fs...)
	}
	return files, nil
}

// traceFile 是 trace_*.json 的顶层结构。
type traceFile struct {
	Trace struct {
		TraceID    string    `json:"traceId"`
		SessionID  string    `json:"sessionId"`
		StartedAt  time.Time `json:"startedAt"`
		ModelInfo  *struct {
			Models           []string `json:"models"`
			TotalInputTokens int64    `json:"totalInputTokens"`
			TotalOutputTokens int64   `json:"totalOutputTokens"`
			TotalCachedTokens int64   `json:"totalCachedTokens"`
		} `json:"modelInfo"`
	} `json:"trace"`
	Spans []traceSpan `json:"spans"`
}

// traceSpan 是 trace 内的一个执行步骤。
type traceSpan struct {
	SpanID     string `json:"spanId"`
	Type       string `json:"type"`
	StartedAt  string `json:"startedAt"` // ISO 8601
	ToolOutput string `json:"toolOutput"` // JSON 字符串（generation 时为 API 响应）
}

// apiResponse 是 generation span toolOutput 里嵌入的 OpenAI 响应。
type apiResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"` // Unix 秒
	Usage   struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		TotalTokens             int64 `json:"total_tokens"`
		PromptTokensDetails     struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// Parse 解析单个 trace JSON 文件，提取所有 generation span 的 token 用量。
func (a *Adapter) Parse(path string) ([]model.UsageEvent, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: read %s: %w", path, err)
	}
	var tf traceFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, fmt.Errorf("workbuddy: parse %s: %w", path, err)
	}

	var events []model.UsageEvent
	for _, span := range tf.Spans {
		if span.Type != "generation" || span.ToolOutput == "" {
			continue
		}
		// toolOutput 可能是单个对象或数组（取第一个）
		var respList []apiResponse
		if err := json.Unmarshal([]byte(span.ToolOutput), &respList); err != nil {
			var single apiResponse
			if err2 := json.Unmarshal([]byte(span.ToolOutput), &single); err2 != nil {
				continue
			}
			respList = []apiResponse{single}
		}
		for _, resp := range respList {
			if resp.Usage.PromptTokens <= 0 && resp.Usage.CompletionTokens <= 0 {
				continue
			}
			// 时间戳优先用 API 响应的 created（Unix 秒），回退到 span startedAt
			var ts time.Time
			if resp.Created > 0 {
				ts = time.Unix(resp.Created, 0).UTC()
			} else if span.StartedAt != "" {
				if t, err := time.Parse(time.RFC3339, span.StartedAt); err == nil {
					ts = t
				}
			}
			// 模型名：WorkBuddy 用别名（fast-model / balanced-model 等），
			// 定价表可通过自定义文件覆盖
			modelName := resp.Model
			if modelName == "" && tf.Trace.ModelInfo != nil && len(tf.Trace.ModelInfo.Models) > 0 {
				modelName = tf.Trace.ModelInfo.Models[0]
			}
			// 项目名：所有 WorkBuddy 会话统一归为 "WorkBuddy"，
			// 避免会话数量增长导致项目列表无限膨胀
			project := "WorkBuddy"

			events = append(events, model.UsageEvent{
				EventID:      adapterName + ":" + resp.ID,
				Timestamp:    ts,
				Tool:         adapterName,
				Model:        modelName,
				Project:      project,
				SessionID:    tf.Trace.SessionID,
				InputTokens:     resp.Usage.PromptTokens,
				OutputTokens:    resp.Usage.CompletionTokens,
				CacheReadTokens:  resp.Usage.PromptTokensDetails.CachedTokens,
				CacheWriteTokens: 0,
			})
		}
	}
	return events, nil
}
