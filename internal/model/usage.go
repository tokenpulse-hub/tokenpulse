// Package model 定义 TokenPulse 的统一数据模型。
// 所有工具适配器的解析结果都归一到 UsageEvent，再交给存储层。
package model

import "time"

// UsageEvent 是一条原子化的用量记录：一次 API 调用的 Token 消耗与折算成本。
type UsageEvent struct {
	// EventID 全局唯一，用于增量导入去重。
	// 约定格式：tool:sessionID:lineUUID
	EventID string `json:"event_id"`

	Timestamp time.Time `json:"timestamp"`
	Tool      string    `json:"tool"`    // claude-code / codex / gemini-cli / ...
	Model     string    `json:"model"`   // claude-sonnet-4-5 / gpt-6 / ...
	Project   string    `json:"project"` // 归属项目（目录名或会话名）
	SessionID string    `json:"session_id"`

	InputTokens  int64 `json:"input_tokens"`  // 输入
	OutputTokens int64 `json:"output_tokens"` // 输出
	CacheTokens  int64 `json:"cache_tokens"`  // 缓存命中 + 缓存写入

	CostUSD float64 `json:"cost_usd"` // 按定价表折算的成本（美元）
}

// TotalTokens 返回总 Token 数（输入 + 输出 + 缓存）。
func (e UsageEvent) TotalTokens() int64 {
	return e.InputTokens + e.OutputTokens + e.CacheTokens
}
