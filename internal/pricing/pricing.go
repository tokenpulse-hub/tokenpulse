// Package pricing 提供模型定价表加载、合并与成本折算。
//
// 定价来源分两层：
//  1. 内置表（data/pricing.json，go:embed 内嵌，随版本发布）
//  2. 用户自定义表（~/.tokenpulse/pricing.json，面板可编辑）
//
// 自定义价格优先于内置——用户可为内置未覆盖的模型（如私有网关的
// 聚合模型名 chat-pro）手动填价，也可以覆盖内置价格。
//
// 支持最长前缀匹配，模型名带日期后缀（如 claude-sonnet-4-5-20260101）也能命中。
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed data/pricing.json
var defaultPricingJSON []byte

// ModelPrice 单个模型（或模型前缀族）的定价，单位：美元 / 1M tokens。
type ModelPrice struct {
	Input      float64 `json:"input"`       // 输入
	Output     float64 `json:"output"`      // 输出
	CacheRead  float64 `json:"cache_read"`  // 缓存命中（读）
	CacheWrite float64 `json:"cache_write"` // 缓存写入
}

type pricingFile struct {
	// Note 会写进文件，提醒维护者价格需要按官方定价校准。
	Note   string                 `json:"_note,omitempty"`
	Models map[string]ModelPrice `json:"models"`
}

// Table 是已加载的定价表（内置 + 自定义合并后）。
type Table struct {
	prefixes []string // 按长度降序排列，保证最长前缀优先匹配
	models   map[string]ModelPrice
}

// Load 解析定价表 JSON。传 nil 时加载内嵌默认表。
func Load(raw []byte) (*Table, error) {
	if raw == nil {
		raw = defaultPricingJSON
	}
	var pf pricingFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, fmt.Errorf("pricing: parse table: %w", err)
	}
	return newTable(pf.Models), nil
}

// LoadWithOverrides 加载内置表并合并用户自定义表（自定义优先）。
// customPath 为空或文件不存在时退化为纯内置表。
func LoadWithOverrides(customPath string) (*Table, error) {
	t, err := Load(nil)
	if err != nil {
		return nil, err
	}
	if customPath == "" {
		return t, nil
	}
	raw, err := os.ReadFile(customPath)
	if os.IsNotExist(err) {
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pricing: read custom %s: %w", customPath, err)
	}
	var pf pricingFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, fmt.Errorf("pricing: parse custom %s: %w", customPath, err)
	}
	// 自定义价格覆盖同名内置条目
	for name, p := range pf.Models {
		t.models[name] = p
	}
	// 重建前缀索引
	t.prefixes = make([]string, 0, len(t.models))
	for name := range t.models {
		t.prefixes = append(t.prefixes, name)
	}
	sort.Slice(t.prefixes, func(i, j int) bool {
		return len(t.prefixes[i]) > len(t.prefixes[j])
	})
	return t, nil
}

// newTable 从模型→价格映射构建带排序前缀索引的 Table。
func newTable(models map[string]ModelPrice) *Table {
	t := &Table{models: models}
	t.prefixes = make([]string, 0, len(models))
	for name := range models {
		t.prefixes = append(t.prefixes, name)
	}
	// 长前缀优先：claude-sonnet-4-5-20260101 应先命中
	// claude-sonnet-4-5 而不是更短的 claude-sonnet。
	sort.Slice(t.prefixes, func(i, j int) bool {
		return len(t.prefixes[i]) > len(t.prefixes[j])
	})
	return t
}

// Match 对模型名做最长前缀匹配（按 '-' 边界），未命中返回 false。
func (t *Table) Match(model string) (ModelPrice, bool) {
	for _, p := range t.prefixes {
		if model == p || strings.HasPrefix(model, p+"-") || strings.HasPrefix(model, p+"_") {
			return t.models[p], true
		}
	}
	return ModelPrice{}, false
}

// Cost 把一次调用的 Token 用量折算成美元成本。
// 找不到定价的模型按 0 计（面板会提示"未定价模型"），宁可低估也不瞎猜。
func (t *Table) Cost(model string, in, out, cacheRead, cacheWrite int64) float64 {
	p, ok := t.Match(model)
	if !ok {
		return 0
	}
	const perM = 1_000_000.0
	return float64(in)/perM*p.Input +
		float64(out)/perM*p.Output +
		float64(cacheRead)/perM*p.CacheRead +
		float64(cacheWrite)/perM*p.CacheWrite
}

// All 返回全部模型→价格映射（含内置+自定义合并后的结果）。
func (t *Table) All() map[string]ModelPrice { return t.models }

// CustomPath 返回用户自定义定价文件的默认路径（~/.tokenpulse/pricing.json）。
// 取不到 home 时返回空串，调用方传空路径即退化为纯内置表。
func CustomPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tokenpulse", "pricing.json")
}

// CustomEntries 读取自定义定价文件里的全部条目（文件不存在返回空 map）。
// 供面板展示"哪些价格是用户自定义的"以及删除条目时复用。
func CustomEntries(path string) (map[string]ModelPrice, error) {
	out := map[string]ModelPrice{}
	if path == "" {
		return out, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pricing: read custom %s: %w", path, err)
	}
	var pf pricingFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, fmt.Errorf("pricing: parse custom %s: %w", path, err)
	}
	for name, p := range pf.Models {
		out[name] = p
	}
	return out, nil
}

// SaveCustom 把用户自定义定价写入 JSON 文件（原子写：先写临时文件再 rename）。
func SaveCustom(path string, models map[string]ModelPrice) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	pf := pricingFile{
		Note: "TokenPulse 用户自定义定价（单位：美元/1M tokens）。自定义价格优先于内置表；删除本文件可恢复默认。",
		Models: models,
	}
	raw, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
