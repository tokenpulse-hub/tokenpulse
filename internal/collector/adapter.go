// Package collector 定义数据源适配器接口与注册表。
// 每个受支持的 AI 工具实现一个 Adapter，通过 Register 注册进全局注册表，
// 上游日志格式变更只需修改对应适配器，不影响其他部分。
package collector

import (
	"fmt"
	"sort"
	"sync"

	"github.com/tokenpulse-hub/tokenpulse/internal/model"
)

// Adapter 是一个数据源适配器。实现方只需关心两件事：
// 找到本机的日志文件（Discover），以及把单个文件解析成统一事件（Parse）。
type Adapter interface {
	// Name 返回适配器标识，如 "claude-code"。同时作为事件的 Tool 字段。
	Name() string

	// Discover 扫描本机，返回该工具所有会话日志文件的绝对路径。
	// 文件不存在时返回空列表，而不是错误（工具未安装是正常状态）。
	Discover() ([]string, error)

	// Parse 解析单个日志文件，返回该文件内的用量事件。
	// 单行解析失败应跳过该行（返回解析成功的事件），而不是让整个文件失败。
	Parse(path string) ([]model.UsageEvent, error)
}

var (
	mu       sync.RWMutex
	registry = make(map[string]Adapter)
)

// Register 注册一个适配器。重复注册同名适配器会返回错误。
func Register(a Adapter) error {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := registry[a.Name()]; ok {
		return fmt.Errorf("collector: adapter %q already registered", a.Name())
	}
	registry[a.Name()] = a
	return nil
}

// All 返回全部已注册适配器，按名称排序，保证扫描顺序稳定。
func All() []Adapter {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Adapter, 0, len(names))
	for _, name := range names {
		out = append(out, registry[name])
	}
	return out
}

// Get 按名称取单个适配器。
func Get(name string) (Adapter, bool) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := registry[name]
	return a, ok
}
