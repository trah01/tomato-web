package main

import (
	_ "embed"
	"io/fs"
	"net/http"
)

//go:embed static/login.html
var loginHTML []byte

// handleHomePage 渲染首页。
func (s *server) handleHomePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(homePage())
}

// handleReaderPage 渲染阅读器页面（static/index.html）。
func (s *server) handleReaderPage(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		http.Error(w, "阅读器页面缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// handleUsagePage 渲染使用说明页面（含数据与隐私部分）。
func (s *server) handleUsagePage(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(staticFS, "static/usage.html")
	if err != nil {
		http.Error(w, "使用说明页面缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// handleLoginPage 渲染站点登录页。
func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.writeLoginPage(w)
}

// writeLoginPage 输出登录页；未登录时访问任何页面都会看到它。
func (s *server) writeLoginPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(loginHTML)
}
