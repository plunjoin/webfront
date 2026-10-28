package webfront

// static.go：生产态"嵌入客户端产物"的静态资源服务。
// 关键点（单文件二进制场景）：
//   - 资源在编译期就固定不变，故首次请求读一次、常驻内存（sync.Map 缓存）；
//   - 同时预生成 gzip 副本，命中 Accept-Encoding 就直接发压缩体（省 CPU、省带宽）；
//   - 用内容 sha1 做 ETag，二次访问走 304，连 45KB 都不用重传。

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// gzThreshold 小于此体积不值得压缩（gzip 头尾约 18B，小文件反而变大）。
const gzThreshold = 1024

// staticAsset 一份嵌入资源的三种形态：原始体、gzip 体、内容指纹。
type staticAsset struct {
	body  []byte
	gz    []byte
	etag  string
	ctype string
}

// StaticHandler 返回服务"嵌入的客户端产物"的 http.Handler（生产态单文件二进制用）。
// 典型挂载：mux.Handle("/assets/", engine.StaticHandler())。
// 配置了 Config.BasePath 时，路径里的前缀会先被剥掉（/site/assets/x → assets/x）。
// 未配置 EmbedFS 时返回 404（开发态请用 DevServer，不走此路）。
func (e *Engine) StaticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.clientFS == nil {
			http.Error(w, "webfront: no embedded client filesystem (set Config.EmbedFS)", http.StatusNotFound)
			return
		}
		name := strings.TrimPrefix(e.stripBase(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		a, err := e.asset(name)
		if err != nil {
			http.Error(w, "not found: "+name, http.StatusNotFound)
			return
		}
		e.serveAsset(w, r, a, "public, max-age=3600")
	})
}

// serveAsset 统一写出一个缓存好的资源：ETag/304 + gzip 协商 + Content-Length。
// 页面（pages/*.html）、静态资源、SPA 外壳都走这一条路径，行为不会各自漂移。
func (e *Engine) serveAsset(w http.ResponseWriter, r *http.Request, a *staticAsset, cacheControl string) {
	w.Header().Set("ETag", a.etag)
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	if a.gz != nil {
		w.Header().Set("Vary", "Accept-Encoding")
	}
	// 条件请求：内容指纹一致则 304，无需重传。
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	out := a.body
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		out = a.gz
		w.Header().Set("Content-Encoding", "gzip")
	}
	if a.ctype != "" {
		w.Header().Set("Content-Type", a.ctype)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	// 预渲染页面和 SPA 外壳都经框架的兜底路由（如 Gin 的 NoRoute）进来，而 Gin 在
	// 调用兜底处理器之前就把状态预设成 404（gin.serveError）。命中即显式声明 200，
	// 覆盖预设值，避免"内容正确、状态码 404"。（304 分支已在上面显式写走。）
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(out)
	}
}

// asset 读取并缓存一份嵌入资源（含 gzip 预压缩）。
func (e *Engine) asset(name string) (*staticAsset, error) {
	if v, ok := e.assets.Load(name); ok {
		return v.(*staticAsset), nil
	}
	f, err := e.clientFS.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	a := &staticAsset{
		body:  body,
		etag:  `"` + shortHash(body) + `"`,
		ctype: contentType(name),
	}
	if a.ctype != "" && compressible(a.ctype) && len(body) >= gzThreshold {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(body)
		_ = zw.Close()
		if buf.Len() < len(body) { // 压不小就别压
			a.gz = buf.Bytes()
		}
	}
	actual, _ := e.assets.LoadOrStore(name, a)
	return actual.(*staticAsset), nil
}

func shortHash(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:8])
}

func contentType(name string) string {
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		switch filepath.Ext(name) {
		case ".js", ".mjs":
			ct = "text/javascript; charset=utf-8"
		case ".css":
			ct = "text/css; charset=utf-8"
		case ".html":
			ct = "text/html; charset=utf-8"
		case ".json", ".map":
			ct = "application/json"
		case ".svg":
			ct = "image/svg+xml"
		case ".woff2":
			ct = "font/woff2"
		}
	}
	return ct
}

func compressible(ctype string) bool {
	if strings.HasPrefix(ctype, "text/") {
		return true
	}
	for _, p := range []string{"javascript", "json", "xml", "svg"} {
		if strings.Contains(ctype, p) {
			return true
		}
	}
	return false
}
