// Package scan 提供"扫描本机全部适配器并入库"的共享入口，
// 供 CLI（tokenpulse scan / serve 启动扫描）与 Web 面板
// （手动扫描按钮、自动扫描定时器）共用。
package scan

import (
	"fmt"

	"github.com/tokenpulse-hub/tokenpulse/internal/collector"
	"github.com/tokenpulse-hub/tokenpulse/internal/model"
	"github.com/tokenpulse-hub/tokenpulse/internal/pricing"
	"github.com/tokenpulse-hub/tokenpulse/internal/store"
)

// Run 执行一次全量增量扫描：遍历所有已注册适配器，
// 解析本机日志 → 定价表（内置 + 用户自定义合并）补算成本 → 幂等写入数据库。
// 返回本次真正新增的事件条数（重复事件自动去重为 0）。
func Run(st *store.Store) (int64, error) {
	table, err := pricing.LoadWithOverrides(pricing.CustomPath())
	if err != nil {
		return 0, fmt.Errorf("scan: load pricing: %w", err)
	}
	var total int64
	for _, a := range collector.All() {
		files, err := a.Discover()
		if err != nil {
			continue // 单适配器发现失败不阻断整体
		}
		var batch []model.UsageEvent
		for _, f := range files {
			events, err := a.Parse(f)
			if err != nil {
				continue // 单文件失败跳过
			}
			// 解析器只负责 Token 计数，成本由定价表统一折算
			for i := range events {
				events[i].CostUSD = table.Cost(events[i].Model,
					events[i].InputTokens, events[i].OutputTokens,
					events[i].CacheReadTokens, events[i].CacheWriteTokens)
			}
			batch = append(batch, events...)
		}
		n, err := st.InsertEvents(batch)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
