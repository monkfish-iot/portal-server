// SPDX-FileCopyrightText: 2026 MonkFish IoT
// SPDX-License-Identifier: Apache-2.0

// portal-server —— MonkFish 云平台独立 PORTAL 页面服务器。
//
// 职责：接收 AP 侧 nginx（portal_type=3, listen 20001）反代上来的
// webauthd 302 请求，按 URL 参数选择 PORTAL 模板并返回登录页。
//
// 请求形态（webauthd portal_server.c CLOUD_PORTAL 分支）：
//
//	GET /{device_id}/?wlanuserip=<STA IP>&wlanusermac=<STA MAC>&wlanacip=<AC IP>&wlanacmac=<AP MAC>
//
// 经 AP nginx `location / { proxy_pass $proxy_pass; }` 原样透传（路径与查询串保留）。
//
// 模板选择顺序：
//  1. 查询参数 t={code}（调试/预览用）
//  2. 配置 rules：device_id 精确匹配 → 前缀匹配
//  3. default_template 兜底
//
// 模板布局与 WrtHub-UI 的 portal.template_root 完全一致：
//
//	{template_root}/{code}/index.html   入口页（entry）
//	{template_root}/{code}/其它文件      静态资源
//
// 与云侧 nginx 的协同（两种模式，见 nginx/portal-server.conf）：
//
//	base 模式（默认）：入口 HTML 由本服务读文件并注入 <base href="/p/{code}/">
//	  后返回，页内相对资源由 nginx 在 /p/ 前缀直接回文件；
//	x_accel 模式（x_accel: true）：本服务不回 body，仅返回
//	  X-Accel-Redirect: /_portal_internal/{code}/index.html 响应头，
//	  把"真正的页面路径"交给 nginx，由其 internal location 直接回文件
//	  （要求模板用 /p/{code}/... 绝对路径引用资源）。
//
// 认证交互不经过本服务：登录页相对路径 POST /v1/login 命中 AP nginx 的
// 本地 location，直接进 webauthd（127.0.0.1:20002）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	publicPrefix  = "/p/"                // 模板静态资源公开前缀（nginx alias 直回文件）
	accelPrefix   = "/_portal_internal/" // X-Accel-Redirect 内部前缀（nginx internal）
	entryPage     = "index.html"         // 模板入口页，与 WrtHub-UI Portal 模板约定一致
	maxCodeLength = 64
)

// Rule device_id → 模板 映射规则。device（精确）与 prefix（前缀）至少填一项。
type Rule struct {
	Device   string `yaml:"device"`
	Prefix   string `yaml:"prefix"`
	Template string `yaml:"template"`
}

type Config struct {
	Listen          string `yaml:"listen"`           // 默认 127.0.0.1:19090（置于云 nginx 后）
	TemplateRoot    string `yaml:"template_root"`    // 默认 ./data/portal-templates
	DefaultTemplate string `yaml:"default_template"` // 默认 example-blue（内置示例模板 · 清爽蓝）
	XAccel          bool   `yaml:"x_accel"`          // true: 入口页改用 X-Accel-Redirect 交 nginx 回文件
	Rules           []Rule `yaml:"rules"`
}

var codeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:19090"
	}
	if cfg.TemplateRoot == "" {
		cfg.TemplateRoot = "./data/portal-templates"
	}
	if cfg.DefaultTemplate == "" {
		cfg.DefaultTemplate = "example-blue"
	}
	return cfg, nil
}

// validCode 模板 code 白名单校验（同时阻断路径穿越）。
func validCode(s string) bool {
	return codeRe.MatchString(s)
}

// templateExists 模板入口页是否存在。
func (s *Server) templateExists(code string) bool {
	st, err := os.Stat(filepath.Join(s.cfg.TemplateRoot, code, entryPage))
	return err == nil && !st.IsDir()
}

// listTemplates 枚举磁盘上可用的模板 code（404 提示与 healthz 用）。
func (s *Server) listTemplates() []string {
	entries, err := os.ReadDir(s.cfg.TemplateRoot)
	if err != nil {
		return nil
	}
	codes := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && validCode(e.Name()) && s.templateExists(e.Name()) {
			codes = append(codes, e.Name())
		}
	}
	return codes
}

// selectTemplate 按 查询参数 t → rules（精确→前缀）→ default 顺序选择模板 code。
func (s *Server) selectTemplate(deviceID string, r *http.Request) string {
	if t := r.URL.Query().Get("t"); validCode(t) && s.templateExists(t) {
		return t
	}
	for _, rule := range s.cfg.Rules {
		if rule.Device != "" && rule.Device == deviceID && validCode(rule.Template) && s.templateExists(rule.Template) {
			return rule.Template
		}
	}
	for _, rule := range s.cfg.Rules {
		if rule.Prefix != "" && strings.HasPrefix(deviceID, rule.Prefix) && validCode(rule.Template) && s.templateExists(rule.Template) {
			return rule.Template
		}
	}
	// default_template 兜底；配置的兜底模板不存在时（如全新安装只有播种的示例模板），
	// 退到磁盘上首个可用模板，避免设备第一次弹 Portal 就 404。
	if s.templateExists(s.cfg.DefaultTemplate) {
		return s.cfg.DefaultTemplate
	}
	if codes := s.listTemplates(); len(codes) > 0 {
		return codes[0]
	}
	return s.cfg.DefaultTemplate
}

// injectBase 在 HTML 的 <head> 后注入 <base href="/p/{code}/">，
// 使页内相对路径资源（css/js/img）解析到 /p/ 前缀，由云 nginx 直接回文件。
func injectBase(html []byte, code string) []byte {
	tag := []byte(fmt.Sprintf("\n<base href=\"%s%s/\">\n", publicPrefix, code))
	lower := strings.ToLower(string(html))
	idx := strings.Index(lower, "<head")
	if idx < 0 {
		return append(tag, html...) // 无 <head>：前插，浏览器仍可解析 base
	}
	gt := strings.IndexByte(string(html[idx:]), '>')
	if gt < 0 {
		return append(tag, html...)
	}
	pos := idx + gt + 1
	out := make([]byte, 0, len(html)+len(tag))
	out = append(out, html[:pos]...)
	out = append(out, tag...)
	return append(out, html[pos:]...)
}

type Server struct {
	cfg       *Config
	log       *slog.Logger
	startedAt time.Time
}

// handleEntry 入口页：GET / 与 GET /{device_id}/。
func (s *Server) handleEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// webauthd type=3 的 302 路径为 /{device_id}/；根路径（type=2 直连等）deviceID 为空。
	deviceID := strings.Trim(r.URL.Path, "/")
	if i := strings.IndexByte(deviceID, '/'); i >= 0 {
		deviceID = deviceID[:i]
	}

	code := s.selectTemplate(deviceID, r)
	if !validCode(code) || !s.templateExists(code) {
		s.serveUnavailable(w, code)
		return
	}

	if s.cfg.XAccel {
		// X-Accel 模式：不回 body，把真正的页面路径交给 nginx（internal location）。
		w.Header().Set("X-Accel-Redirect", accelPrefix+code+"/"+entryPage)
		w.Header().Set("X-Portal-Template", code) // 调试可观测：实际选中的模板
		w.WriteHeader(http.StatusOK)
		return
	}

	html, err := os.ReadFile(filepath.Join(s.cfg.TemplateRoot, code, entryPage))
	if err != nil {
		s.log.Error("读取模板失败", "code", code, "err", err)
		s.serveUnavailable(w, code)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // 入口页按参数动态选模板，禁止缓存
	_, _ = w.Write(injectBase(html, code))
}

// serveUnavailable 模板不可用时的提示页（含可用模板清单，便于排障）。
func (s *Server) serveUnavailable(w http.ResponseWriter, code string) {
	detail := ""
	if validCode(code) {
		detail = "（模板 <code>" + code + "</code> 未部署或缺少 " + entryPage + "）"
	}
	body := `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8"><title>Portal 暂不可用</title></head>
<body style="font-family:sans-serif;text-align:center;padding-top:15vh;color:#444">
<h2>认证页面暂不可用</h2><p>请联系网络管理员` + detail + `</p></body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(body))
}

// handleStatic /p/ 前缀静态资源。
// 生产由云 nginx alias 直接回文件，本处理器仅用于无 nginx 的单机部署兜底。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, publicPrefix)
	if !validCode(strings.SplitN(rest, "/", 2)[0]) {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.cfg.TemplateRoot, filepath.FromSlash(pathSafe(rest)))
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		http.NotFound(w, r) // 不做目录列举
		return
	}
	http.ServeFile(w, r, full)
}

// pathSafe 清洗相对路径（防穿越），仅保留安全字符。
func pathSafe(p string) string {
	p = filepath.ToSlash(path.Clean("/" + p))
	return strings.TrimPrefix(p, "/")
}

// handleHealthz 健康检查与运行信息：状态、版本、运行时长、内存、模板清单。
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	now := time.Now()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":             "ok",
		"version":            buildVersion(),
		"started_at":         s.startedAt,
		"uptime_seconds":     int64(now.Sub(s.startedAt).Seconds()),
		"goroutines":         runtime.NumGoroutine(),
		"memory_alloc_bytes": ms.Alloc,
		"memory_sys_bytes":   ms.Sys,
		"templates":          s.listTemplates(),
		"template_root":      s.cfg.TemplateRoot,
		"default_template":   s.cfg.DefaultTemplate,
		"x_accel":            s.cfg.XAccel,
	})
}

// buildVersion 返回二进制版本号：优先模块版本，开发构建回退 dev。
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	cfgPath := flag.String("c", "config.yaml", "配置文件路径")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Error("加载配置失败", "err", err)
		os.Exit(1)
	}
	if !validCode(cfg.DefaultTemplate) {
		log.Error("default_template 非法", "code", cfg.DefaultTemplate)
		os.Exit(1)
	}

	s := &Server{cfg: cfg, log: log, startedAt: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.Handle(publicPrefix, http.HandlerFunc(s.handleStatic))
	mux.HandleFunc("/", s.handleEntry)

	log.Info("portal-server 启动",
		"listen", cfg.Listen,
		"template_root", cfg.TemplateRoot,
		"default_template", cfg.DefaultTemplate,
		"x_accel", cfg.XAccel,
		"templates", s.listTemplates(),
	)
	if err := http.ListenAndServe(cfg.Listen, mux); err != nil {
		log.Error("服务退出", "err", err)
		os.Exit(1)
	}
}
