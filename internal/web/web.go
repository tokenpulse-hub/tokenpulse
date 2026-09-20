// Package web 提供内嵌的 Web 面板（单文件 HTML，go:embed 打进二进制）。
//
// 除展示外，面板还支持三种控制：
//   - POST /api/scan           手动立即扫描一次（"立即扫描"按钮）
//   - POST /api/scanner/toggle 切换自动扫描开关
//   - GET/POST/DELETE /api/pricing  定价管理（查看/保存/删除，保存后全量重算）
//
// 自动扫描定时器运行在面板进程内（可随时开关，无需重启）。
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tokenpulse-hub/tokenpulse/internal/pricing"
	"github.com/tokenpulse-hub/tokenpulse/internal/store"
)

//go:embed static/index.html
var staticFS embed.FS

// Options 是面板服务的构造选项。
type Options struct {
	// ScanFn 执行一次增量扫描，返回新增事件数（由 cmd 层注入 scan.Run）。
	ScanFn func() (int64, error)

	// AutoScanEnabled / AutoScanInterval 控制内置定时扫描。
	AutoScanEnabled  bool
	AutoScanInterval time.Duration
}

// Server 是面板 HTTP 服务。
type Server struct {
	store *store.Store
	opts  Options

	mu        sync.Mutex // 保护 enabled 与扫描互斥
	enabled   bool       // 自动扫描当前开关状态
	scanning  bool       // 是否有扫描正在进行
	stopCh    chan struct{}
	stopped   bool
}

// NewServer 构造面板服务；启用自动扫描时内部启动定时 goroutine。
// 间隔未指定（CLI 传 0）时兜底为 1h，保证面板内开关始终可用。
func NewServer(st *store.Store, opts Options) *Server {
	if opts.AutoScanInterval <= 0 {
		opts.AutoScanInterval = time.Hour
	}
	s := &Server{
		store:   st,
		opts:    opts,
		enabled: opts.AutoScanEnabled,
		stopCh:  make(chan struct{}),
	}
	if opts.AutoScanEnabled {
		s.startAutoScan()
	}
	return s
}

// Stop 关闭自动扫描 goroutine（Server 停止后调用）。
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
	}
}

// startAutoScan 启动定时扫描 goroutine。
// ticker 常驻运行，每次触发时检查 enabled——开关切换即时生效，无需重建定时器。
func (s *Server) startAutoScan() {
	interval := s.opts.AutoScanInterval
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-t.C:
				if !s.autoScanEnabled() {
					continue
				}
				added, err := s.runScan()
				if err != nil {
					log.Printf("[%s] 自动扫描失败: %v", time.Now().Format("15:04:05"), err)
					continue
				}
				log.Printf("[%s] 自动扫描完成，新增 %d 条事件", time.Now().Format("15:04:05"), added)
			}
		}
	}()
	log.Printf("自动扫描已启用：每 %s 一次", interval)
}

func (s *Server) autoScanEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// runScan 串行化执行扫描（手动与自动互斥，避免并发重复扫描）。
func (s *Server) runScan() (int64, error) {
	s.mu.Lock()
	if s.scanning { // 已有扫描在进行：直接返回 0，不排队堆积请求
		s.mu.Unlock()
		return 0, nil
	}
	s.scanning = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.scanning = false
		s.mu.Unlock()
	}()

	if s.opts.ScanFn == nil {
		return 0, fmt.Errorf("scan function not configured")
	}
	return s.opts.ScanFn()
}

// ----- 路由 -----

// Handler 返回面板路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/scan", s.handleScanNow)
	mux.HandleFunc("/api/scanner/toggle", s.handleToggle)
	mux.HandleFunc("/api/pricing", s.handlePricing)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "panel asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// scannerStatus 是自动扫描的当前状态（随 summary 下发 + toggle 返回）。
type scannerStatus struct {
	Enabled        bool  `json:"enabled"`
	IntervalSec    int64 `json:"interval_sec"`
	NextInSec      int64 `json:"next_in_sec,omitempty"` // 暂未实现倒计时，预留
}

func (s *Server) status() scannerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	iv := int64(0)
	if s.opts.AutoScanInterval > 0 {
		iv = int64(s.opts.AutoScanInterval / time.Second)
	}
	return scannerStatus{Enabled: s.enabled, IntervalSec: iv}
}

// summaryPayload 是 /api/summary 的响应体。
type summaryPayload struct {
	Now      string             `json:"now"`
	Today    store.Range        `json:"today"`
	Week     store.Range        `json:"week"`
	Month    store.Range        `json:"month"`
	Models   []store.Bucket     `json:"models"`
	Projects []store.Bucket     `json:"projects"`
	Tools    []store.Bucket     `json:"tools"`
	Trend    []store.DayPoint   `json:"trend"`
	AutoScan scannerStatus      `json:"auto_scan"`
	ToolFilter string            `json:"tool_filter,omitempty"` // 当前筛选的工具（空=全部）
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	tool := r.URL.Query().Get("tool") // 空=全部工具

	today, week, month, err := s.store.Summary(now, tool)
	if err != nil {
		http.Error(w, fmt.Sprintf("summary: %v", err), http.StatusInternalServerError)
		return
	}
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Local().Location())
	models, err := s.store.GroupBy("model", monthStart, 20, tool)
	if err != nil {
		http.Error(w, fmt.Sprintf("models: %v", err), http.StatusInternalServerError)
		return
	}
	projects, err := s.store.GroupBy("project", monthStart, 10, tool)
	if err != nil {
		http.Error(w, fmt.Sprintf("projects: %v", err), http.StatusInternalServerError)
		return
	}
	tools, err := s.store.GroupBy("tool", monthStart, 10, tool)
	if err != nil {
		http.Error(w, fmt.Sprintf("tools: %v", err), http.StatusInternalServerError)
		return
	}
	trend, err := s.store.DailyTrend(now, 30, tool)
	if err != nil {
		http.Error(w, fmt.Sprintf("trend: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(summaryPayload{
		Now: now.Format(time.RFC3339), Today: today, Week: week, Month: month,
		Models: models, Projects: projects, Tools: tools, Trend: trend,
		AutoScan: s.status(), ToolFilter: tool,
	})
}

// handleTools 返回本月所有工具列表（用于前端筛选条）。
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	tools, err := s.store.DistinctTools(time.Now())
	if err != nil {
		http.Error(w, fmt.Sprintf("tools: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(tools)
}

// handleScanNow 手动立即扫描：POST /api/scan。
// 若已有扫描在进行，直接返回 0（幂等，不堆积重复扫描）。
func (s *Server) handleScanNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	added, err := s.runScan()
	if err != nil {
		http.Error(w, fmt.Sprintf("scan: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"added":       added,
		"finished_at": time.Now().Format(time.RFC3339),
	})
}

// handleToggle 切换自动扫描开关：POST /api/scanner/toggle。
// Body: {"enabled": true|false}，返回切换后的状态。
func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
		http.Error(w, `{"error":"expect JSON body {\"enabled\": bool}}"`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.enabled = *body.Enabled
	s.mu.Unlock()
	state := "关闭"
	if *body.Enabled {
		state = "开启"
	}
	log.Printf("自动扫描已切换为：%s（面板操作）", state)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(s.status())
}

// ----- 定价管理 -----

// priceEntry 是 /api/pricing 返回的单条定价（含来源标注）。
type priceEntry struct {
	Name       string  `json:"name"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Source     string  `json:"source"` // builtin=内置 / custom=用户自定义（含覆盖）
}

// unpricedModel 是数据库里出现过、但定价表未覆盖的模型（漏计成本提醒）。
type unpricedModel struct {
	Name   string `json:"name"`
	Tokens int64  `json:"tokens"`
	Calls  int64  `json:"calls"`
}

// pricingPayload 是 GET /api/pricing 的响应体。
type pricingPayload struct {
	Models   []priceEntry   `json:"models"`
	Unpriced []unpricedModel `json:"unpriced"`
	CustomOK bool           `json:"custom_ok"` // 自定义文件当前是否可读（false 时前端提示文件损坏）
}

// handlePricing 定价管理三合一入口：
//   - GET    列出合并后定价表（标注来源）+ 库内未定价模型
//   - POST   新增/更新一条自定义价格 → 立即全量重算历史成本
//   - DELETE 删除一条自定义价格（恢复内置价）→ 同样全量重算
func (s *Server) handlePricing(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.pricingGet(w)
	case http.MethodPost:
		s.pricingSet(w, r)
	case http.MethodDelete:
		s.pricingDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "GET/POST/DELETE only", http.StatusMethodNotAllowed)
	}
}

// mergedTable 加载合并定价表 + 自定义条目名集合。
// 返回的 customNames 用于给合并结果标注来源。
func mergedTable() (t *pricing.Table, customNames map[string]bool, customOK bool, err error) {
	customNames = map[string]bool{}
	entries, entriesErr := pricing.CustomEntries(pricing.CustomPath())
	if entriesErr != nil {
		return nil, nil, false, entriesErr
	}
	for name := range entries {
		customNames[name] = true
	}
	t, err = pricing.LoadWithOverrides(pricing.CustomPath())
	if err != nil {
		return nil, nil, false, err
	}
	return t, customNames, true, nil
}

func (s *Server) pricingGet(w http.ResponseWriter) {
	t, customNames, customOK, err := mergedTable()
	if err != nil {
		http.Error(w, fmt.Sprintf("pricing: %v", err), http.StatusInternalServerError)
		return
	}

	entries := make([]priceEntry, 0, len(t.All()))
	for name, p := range t.All() {
		source := "builtin"
		if customNames[name] {
			source = "custom"
		}
		entries = append(entries, priceEntry{name, p.Input, p.Output, p.CacheRead, p.CacheWrite, source})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	// 库内出现但定价表未覆盖的模型（全量维度，不按月份截断）
	var unpriced []unpricedModel
	all, err := s.store.GroupBy("model", time.Time{}, 10000, "")
	if err == nil {
		for _, b := range all {
			if _, ok := t.Match(b.Name); !ok {
				unpriced = append(unpriced, unpricedModel{b.Name, b.TotalTokens, b.Calls})
			}
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(pricingPayload{Models: entries, Unpriced: unpriced, CustomOK: customOK})
}

// pricingSet POST /api/pricing：新增/更新一条自定义价格并全量重算。
// Body: {"name":"chat-pro","input":1.25,"output":10,"cache_read":0.15,"cache_write":1.25}
// 缺省的价格字段按 0 计；价格为 0 也是合法定价（明确按 0 计费）。
func (s *Server) pricingSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string   `json:"name"`
		Input      *float64 `json:"input"`
		Output     *float64 `json:"output"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	for label, v := range map[string]*float64{
		"input": body.Input, "output": body.Output,
		"cache_read": body.CacheRead, "cache_write": body.CacheWrite,
	} {
		if v != nil && *v < 0 {
			http.Error(w, fmt.Sprintf(`{"error":"%s must be >= 0"}`, label), http.StatusBadRequest)
			return
		}
	}
	val := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	price := pricing.ModelPrice{
		Input: val(body.Input), Output: val(body.Output),
		CacheRead: val(body.CacheRead), CacheWrite: val(body.CacheWrite),
	}

	path := pricing.CustomPath()
	entries, err := pricing.CustomEntries(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("pricing: %v", err), http.StatusInternalServerError)
		return
	}
	entries[name] = price
	if err := pricing.SaveCustom(path, entries); err != nil {
		http.Error(w, fmt.Sprintf("pricing save: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("定价更新：%s → in=$%g out=$%g cacheR=$%g cacheW=$%g /1M（面板操作）",
		name, price.Input, price.Output, price.CacheRead, price.CacheWrite)

	recalculated, err := s.recalcAll()
	if err != nil {
		http.Error(w, fmt.Sprintf("recalc: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "recalculated": recalculated})
}

// pricingDelete DELETE /api/pricing：删除一条自定义价格（恢复内置价）并全量重算。
// Body: {"name":"chat-pro"}；该条目本就不存在时幂等成功。
func (s *Server) pricingDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	path := pricing.CustomPath()
	entries, err := pricing.CustomEntries(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("pricing: %v", err), http.StatusInternalServerError)
		return
	}
	delete(entries, name)
	if err := pricing.SaveCustom(path, entries); err != nil {
		http.Error(w, fmt.Sprintf("pricing save: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("定价删除：%s（恢复内置价，面板操作）", name)

	recalculated, err := s.recalcAll()
	if err != nil {
		http.Error(w, fmt.Sprintf("recalc: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "recalculated": recalculated})
}

// recalcAll 用最新合并定价表重算全部历史事件成本。
func (s *Server) recalcAll() (int64, error) {
	t, _, _, err := mergedTable()
	if err != nil {
		return 0, err
	}
	n, err := s.store.RecalcCosts(t)
	if err == nil && n > 0 {
		log.Printf("已按新定价重算 %d 条历史事件成本", n)
	}
	return n, err
}
