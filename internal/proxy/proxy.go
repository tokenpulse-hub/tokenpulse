// Package proxy 实现本地反向代理网关，统计一切 OpenAI 兼容 API 调用。
//
// 用法：将 OpenAI 兼容客户端的 base_url 指向本代理（如 http://localhost:8421/v1），
// 代理透明转发请求到真正的 API 地址，同时在响应中提取 usage 字段入库统计。
//
// 支持流式（stream=true）和非流式两种响应模式：
//   - 非流式：从完整 JSON 响应体提取 usage
//   - 流式：在最后一个 chunk（data: [DONE] 前）的 usage 字段中提取
//
// 支持多上游：通过 /v1/* 路径后的第一段指定上游名称（如 /v1/deepseek/chat/completions），
// 或通过请求头 X-TokenPulse-Upstream 指定。未指定时走默认上游。
package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tokenpulse-hub/tokenpulse/internal/model"
	"github.com/tokenpulse-hub/tokenpulse/internal/pricing"
	"github.com/tokenpulse-hub/tokenpulse/internal/store"
)

// Upstream 定义一个 API 上游。
type Upstream struct {
	Name    string // 唯一标识（如 deepseek / openai / local-glm）
	BaseURL string // 真实 API 地址（如 https://api.deepseek.com）
	Token   string // API Key（可选，留空则透传客户端请求头里的 Authorization）
}

// Config 是代理网关的配置。
type Config struct {
	ListenAddr string              // 监听地址（如 :8421）
	Upstreams  map[string]*Upstream // 上游列表（key 为 Name）
	Default    string              // 默认上游名称
}

// Gateway 是代理网关实例。
type Gateway struct {
	cfg      Config
	store    *store.Store
	pricing  func() *pricing.Table // 延迟加载定价表（保证用最新自定义价格）
	mu       sync.Mutex
	revProxies map[string]*httputil.ReverseProxy // 缓存的 ReverseProxy
}

// New 创建代理网关。pricingFn 应返回最新的合并定价表。
func New(st *store.Store, cfg Config, pricingFn func() *pricing.Table) *Gateway {
	if cfg.Upstreams == nil || len(cfg.Upstreams) == 0 {
		cfg.Upstreams = map[string]*Upstream{
			"openai": {Name: "openai", BaseURL: "https://api.openai.com"},
		}
		cfg.Default = "openai"
	}
	g := &Gateway{
		cfg:        cfg,
		store:      st,
		pricing:    pricingFn,
		revProxies: map[string]*httputil.ReverseProxy{},
	}
	return g
}

// Handler 返回 HTTP Handler。
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", g.handleV1)
	mux.HandleFunc("/", g.handleInfo)
	return mux
}

// handleInfo 返回简单的网关状态信息。
func (g *Gateway) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	upstreams := make([]string, 0, len(g.cfg.Upstreams))
	for name := range g.cfg.Upstreams {
		upstreams = append(upstreams, name)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"service":   "TokenPulse Proxy Gateway",
		"upstreams": upstreams,
		"usage":     "Set base_url to http://<host>" + g.cfg.ListenAddr + "/v1/<upstream> or use X-TokenPulse-Upstream header",
	})
}

// handleV1 处理 /v1/* 路径，解析上游并转发。
func (g *Gateway) handleV1(w http.ResponseWriter, r *http.Request) {
	// 解析上游名称：/v1/<upstream>/chat/completions → <upstream>
	// 或 /v1/chat/completions → 默认上游
	rest := strings.TrimPrefix(r.URL.Path, "/v1/")
	parts := strings.SplitN(rest, "/", 2)

	upstreamName := g.cfg.Default
	if len(parts) > 1 {
		// 第一段是上游名
		candidate := parts[0]
		if _, ok := g.cfg.Upstreams[candidate]; ok {
			upstreamName = candidate
			rest = parts[1] // 去掉上游名前缀，保留后续路径
		}
	}
	// 也支持请求头指定
	if h := r.Header.Get("X-TokenPulse-Upstream"); h != "" {
		if _, ok := g.cfg.Upstreams[h]; ok {
			upstreamName = h
		}
	}

	up, ok := g.cfg.Upstreams[upstreamName]
	if !ok {
		http.Error(w, fmt.Sprintf(`{"error":"unknown upstream %q"}`, upstreamName), http.StatusBadRequest)
		return
	}

	// 构建 upstream URL
	targetURL, err := url.Parse(up.BaseURL)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid upstream url: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// 读取请求体（用于后面判断 stream 和提取模型名）
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"read request body failed"}`, http.StatusBadRequest)
		return
	}
	r.Body.Close()

	// 从请求体提取 model 名（用于流式场景下提前知道模型）
	var reqModel string
	var isStream bool
	var reqBodyMap map[string]any
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &reqBodyMap)
		if m, ok := reqBodyMap["model"].(string); ok {
			reqModel = m
		}
		if s, ok := reqBodyMap["stream"].(bool); ok {
			isStream = s
		}
	}

	// 获取或创建 ReverseProxy
	rp := g.getProxy(upstreamName, up, targetURL)

	// 改写请求路径：去掉 /v1/<upstream> 前缀，保留 /v1/ 后的 API 路径
	// 如 /v1/deepseek/chat/completions → /v1/chat/completions
	newPath := "/v1/" + rest
	r.URL.Path = newPath
	r.URL.RawPath = ""

	// 注入或保留 Authorization
	if up.Token != "" {
		r.Header.Set("Authorization", "Bearer "+up.Token)
	}

	// 用自定义 ResponseWriter 拦截响应
	rec := &responseRecorder{
		buf:      &bytes.Buffer{},
		status:   200,
		isStream: isStream,
	}

	// 重新设置请求体
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	rp.ServeHTTP(rec, r)

	// 拷贝响应头
	for k, vs := range rec.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.status)
	// 写响应体（同时尝试提取 usage）
	usage := g.extractUsage(rec.buf.Bytes(), isStream, reqModel)
	w.Write(rec.buf.Bytes())

	// 异步入库（不阻塞响应）
	if usage != nil {
		go g.recordUsage(usage, upstreamName)
	}
}

// getProxy 获取或创建一个上游的 ReverseProxy。
func (g *Gateway) getProxy(name string, up *Upstream, target *url.URL) *httputil.ReverseProxy {
	g.mu.Lock()
	defer g.mu.Unlock()
	if rp, ok := g.revProxies[name]; ok {
		return rp
	}
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.Host = target.Host
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[proxy] upstream %s error: %v", name, err)
			http.Error(w, fmt.Sprintf(`{"error":"upstream %s: %v"}`, name, err), http.StatusBadGateway)
		},
	}
	g.revProxies[name] = rp
	return rp
}

// usageRecord 是从响应中提取的用量信息。
type usageRecord struct {
	Model       string
	InputTokens int64
	OutputTokens int64
	CacheRead   int64
	CacheWrite  int64
}

// extractUsage 从响应体中提取 OpenAI usage 字段。
// 非流式：响应体是完整 JSON，usage 在顶层。
// 流式：最后一个 chunk（[DONE] 前）的 usage 字段。
func (g *Gateway) extractUsage(body []byte, isStream bool, fallbackModel string) *usageRecord {
	if len(body) == 0 {
		return nil
	}

	if isStream {
		return extractUsageFromStream(body, fallbackModel)
	}
	return extractUsageFromJSON(body, fallbackModel)
}

// extractUsageFromJSON 从非流式响应中提取 usage。
func extractUsageFromJSON(body []byte, fallbackModel string) *usageRecord {
	var resp struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 {
		return nil
	}
	model := resp.Model
	if model == "" {
		model = fallbackModel
	}
	return &usageRecord{
		Model:       model,
		InputTokens: resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
		CacheRead:   resp.Usage.PromptTokensDetails.CachedTokens,
	}
}

// extractUsageFromStream 从流式响应中提取 usage。
// OpenAI 流式响应中，最后一个 chunk（在 data: [DONE] 之前）包含 usage 字段。
func extractUsageFromStream(body []byte, fallbackModel string) *usageRecord {
	// 从后往前扫描 SSE 行，找到含 usage 的 chunk
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var lastUsageChunk []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			continue
		}
		// 检查是否含 usage
		if bytes.Contains(data, []byte(`"usage"`)) {
			lastUsageChunk = data
		}
	}

	if lastUsageChunk == nil {
		return nil
	}

	var chunk struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(lastUsageChunk, &chunk); err != nil {
		return nil
	}
	if chunk.Usage.PromptTokens == 0 && chunk.Usage.CompletionTokens == 0 {
		return nil
	}
	model := chunk.Model
	if model == "" {
		model = fallbackModel
	}
	return &usageRecord{
		Model:       model,
		InputTokens: chunk.Usage.PromptTokens,
		OutputTokens: chunk.Usage.CompletionTokens,
		CacheRead:   chunk.Usage.PromptTokensDetails.CachedTokens,
	}
}

// recordUsage 将一次 API 调用的用量入库。
func (g *Gateway) recordUsage(u *usageRecord, upstreamName string) {
	table := g.pricing()
	if table == nil {
		return
	}

	cost := table.Cost(u.Model, u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheWrite)

	// 生成唯一 EventID
	idBytes := make([]byte, 8)
	rand.Read(idBytes)
	eventID := "proxy:" + time.Now().UTC().Format("20060102T150405.000") + ":" + hex.EncodeToString(idBytes)

	event := model.UsageEvent{
		EventID:         eventID,
		Timestamp:       time.Now().UTC(),
		Tool:            "proxy",
		Model:           u.Model,
		Project:         upstreamName,
		SessionID:       "",
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens:  u.CacheRead,
		CacheWriteTokens: u.CacheWrite,
		CostUSD:         cost,
	}

	if _, err := g.store.InsertEvents([]model.UsageEvent{event}); err != nil {
		log.Printf("[proxy] insert usage failed: %v", err)
	} else {
		log.Printf("[proxy] %s %s: in=%d out=%d cacheR=%d → $%.4f",
			upstreamName, u.Model, u.InputTokens, u.OutputTokens, u.CacheRead, cost)
	}
}

// responseRecorder 捕获上游响应（用于提取 usage）。
type responseRecorder struct {
	buf      *bytes.Buffer
	header   http.Header
	status   int
	isStream bool
}

func (r *responseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

func (r *responseRecorder) WriteHeader(code int) {
	r.status = code
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	return r.buf.Write(b)
}
