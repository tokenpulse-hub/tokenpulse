// tokenpulse 是 TokenPulse 的 CLI 入口。
//
// 用法：
//
//	tokenpulse scan            扫描本机 AI 工具日志并导入数据库
//	tokenpulse status           查看今日/本周/本月 Token 与成本汇总
//	tokenpulse top             本月消耗最高的项目 TOP5
//	tokenpulse models          本月模型成本明细
//	tokenpulse serve [--port]  启动 Web 面板（默认先自动扫描一次）
//	tokenpulse --db PATH       指定数据库路径（默认 ~/.tokenpulse/tokenpulse.db）
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	// 注册数据源适配器（init 挂进 collector 注册表）。
	_ "github.com/tokenpulse-hub/tokenpulse/internal/collector/claude"
	_ "github.com/tokenpulse-hub/tokenpulse/internal/collector/codex"
	_ "github.com/tokenpulse-hub/tokenpulse/internal/collector/gemini"
	_ "github.com/tokenpulse-hub/tokenpulse/internal/collector/opencode"
	_ "github.com/tokenpulse-hub/tokenpulse/internal/collector/teleagent"
	_ "github.com/tokenpulse-hub/tokenpulse/internal/collector/workbuddy"
	"github.com/tokenpulse-hub/tokenpulse/internal/collector"
	"github.com/tokenpulse-hub/tokenpulse/internal/pricing"
	"github.com/tokenpulse-hub/tokenpulse/internal/scan"
	"github.com/tokenpulse-hub/tokenpulse/internal/store"
	"github.com/tokenpulse-hub/tokenpulse/internal/web"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "tokenpulse: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	// 全局 --db 提前解析（任意位置出现均生效）
	args, dbPath, err := extractDBFlag(args)
	if err != nil {
		return err
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "scan":
		return cmdScan(dbPath, rest)
	case "status":
		return cmdStatus(dbPath)
	case "top":
		return cmdTop(dbPath)
	case "models":
		return cmdModels(dbPath)
	case "serve":
		return cmdServe(dbPath, rest)
	case "version", "--version":
		fmt.Printf("tokenpulse %s\n", version)
		return nil
	default:
		return usage()
	}
}

// ---------- 全局参数 ----------

func extractDBFlag(args []string) ([]string, string, error) {
	dbPath := defaultDBPath()
	out := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		switch {
		case args[i] == "--db" || args[i] == "-db":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("--db 需要一个路径参数")
			}
			dbPath = args[i+1]
			i += 2
		case strings.HasPrefix(args[i], "--db="):
			dbPath = strings.TrimPrefix(args[i], "--db=")
			i++
		default:
			out = append(out, args[i])
			i++
		}
	}
	return out, dbPath, nil
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "tokenpulse.db"
	}
	return filepath.Join(home, ".tokenpulse", "tokenpulse.db")
}

func mustOpen(dbPath string) (*store.Store, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s: %w", dir, err)
		}
	}
	return store.Open(dbPath)
}

// ---------- scan：采集导入 ----------

func cmdScan(dbPath string, rest []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	reset := fs.Bool("reset", false, "清空已有数据后重新导入（用于解析逻辑变更后重建）")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	table, err := pricing.LoadWithOverrides(pricing.CustomPath())
	if err != nil {
		return err
	}
	st, err := mustOpen(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if *reset {
		if err := st.Clear(); err != nil {
			return fmt.Errorf("scan --reset: 清空数据: %w", err)
		}
		fmt.Println("已清空历史数据（--reset 模式）")
	}

	var totalFiles, totalInserted int64
	start := time.Now()
	for _, a := range collector.All() {
		files, err := a.Discover()
		if err != nil {
			fmt.Printf("[%s] 发现文件失败: %v\n", a.Name(), err)
			continue
		}
		if len(files) == 0 {
			fmt.Printf("[%s] 未发现日志文件（工具未安装或未使用过，跳过）\n", a.Name())
			continue
		}
		var toolEvents, toolInserted int64
		for _, f := range files {
			events, err := a.Parse(f)
			if err != nil {
				fmt.Printf("  解析失败 %s: %v\n", f, err)
				continue
			}
			totalFiles++
			// 解析器只负责 Token 计数，成本由定价表统一折算
			for i := range events {
				events[i].CostUSD = table.Cost(events[i].Model,
					events[i].InputTokens, events[i].OutputTokens,
					events[i].CacheReadTokens, events[i].CacheWriteTokens)
			}
			n, err := st.InsertEvents(events)
			if err != nil {
				fmt.Printf("  写入失败 %s: %v\n", f, err)
				continue
			}
			toolEvents += int64(len(events))
			toolInserted += n
		}
		totalInserted += toolInserted
		fmt.Printf("[%s] %d 个文件，解析 %d 条事件，新增 %d 条\n",
			a.Name(), len(files), toolEvents, toolInserted)
	}
	fmt.Printf("\n扫描完成：%d 个文件，新增 %d 条事件，耗时 %s\n",
		totalFiles, totalInserted, time.Since(start).Round(time.Millisecond))
	fmt.Printf("数据库位置：%s\n", dbPath)
	return nil
}

// ---------- status / top / models：查询 ----------

func cmdStatus(dbPath string) error {
	st, err := mustOpen(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	today, week, month, err := st.Summary(time.Now(), "")
	if err != nil {
		return err
	}
	ranges := [3]store.Range{today, week, month}
	fmt.Println("TokenPulse 用量汇总")
	fmt.Println(strings.Repeat("─", 60))
	for _, r := range ranges {
		label := map[string]string{"today": "今日", "week": "本周", "month": "本月"}[r.Label]
		fmt.Printf("%s：%s / %s tokens / %d 次调用\n",
			label, usd(r.CostUSD), human(r.TotalTokens), r.Calls)
		fmt.Printf("      输入 %s · 输出 %s · 缓存读 %s · 缓存写 %s\n",
			human(r.InputTokens), human(r.OutputTokens), human(r.CacheReadTokens), human(r.CacheWriteTokens))
	}
	return nil
}

func cmdTop(dbPath string) error {
	st, err := mustOpen(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Local().Location())
	buckets, err := st.GroupBy("project", from, 5, "")
	if err != nil {
		return err
	}
	return printBuckets("本月项目 TOP5（按成本降序）", buckets)
}

func cmdModels(dbPath string) error {
	st, err := mustOpen(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Local().Location())
	buckets, err := st.GroupBy("model", from, 20, "")
	if err != nil {
		return err
	}
	return printBuckets("本月模型明细（按成本降序）", buckets)
}

func printBuckets(title string, buckets []store.Bucket) error {
	fmt.Println(title)
	fmt.Println(strings.Repeat("─", 60))
	if len(buckets) == 0 {
		fmt.Println("暂无数据：请先运行 tokenpulse scan")
		return nil
	}
	for _, b := range buckets {
		fmt.Printf("%-32s %10s tokens  %10s  %6d 次调用\n",
			trunc(b.Name, 32), human(b.TotalTokens), usd(b.CostUSD), b.Calls)
	}
	return nil
}

// ---------- serve：Web 面板 ----------

func cmdServe(dbPath string, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", 8420, "面板监听端口")
	skipScan := fs.Bool("skip-scan", false, "跳过启动时的一次自动扫描")
	autoScan := fs.Duration("auto-scan", time.Hour, "自动扫描间隔（0 表示启动时即关闭，默认 1h；面板内可随时开关）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := mustOpen(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if !*skipScan {
		fmt.Println("启动前自动扫描一次日志…")
		if n, err := scan.Run(st); err != nil {
			fmt.Fprintf(os.Stderr, "警告：启动扫描失败（%v），继续启动面板\n", err)
		} else {
			fmt.Printf("启动扫描完成，新增 %d 条事件\n", n)
		}
	}

	// 面板接管自动扫描：进程内定时器，支持面板内随时开关与手动触发。
	panel := web.NewServer(st, web.Options{
		ScanFn:           func() (int64, error) { return scan.Run(st) },
		AutoScanEnabled:  *autoScan > 0,
		AutoScanInterval: *autoScan,
	})
	defer panel.Stop()

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	fmt.Printf("TokenPulse 面板已启动：http://%s  （Ctrl+C 退出）\n", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           panel.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// ---------- 工具函数 ----------

func usage() error {
	fmt.Print(`TokenPulse ` + version + ` — AI 消费记账本

用法：
  tokenpulse scan              扫描本机 AI 工具日志并导入
  tokenpulse status             查看今日/本周/本月汇总
  tokenpulse top                本月项目 TOP5
  tokenpulse models             本月模型成本明细
  tokenpulse serve [--port N]   启动 Web 面板（默认 :8420，--auto-scan 1h 定时扫描，0 关闭）
  tokenpulse version            显示版本

全局参数：
  --db PATH    数据库路径（默认 ~/.tokenpulse/tokenpulse.db）
`)
	return nil
}

// human 把 token 数格式化成 1.2M / 3.4K 形式。
func human(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// usd 格式化美元成本。
func usd(c float64) string {
	switch {
	case c == 0:
		return "$0.00"
	case c < 0.01:
		return fmt.Sprintf("$%.4f", c)
	default:
		return fmt.Sprintf("$%.2f", c)
	}
}

// trunc 截断过长名称，保证表格对齐。
func trunc(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
