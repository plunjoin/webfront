//go:build dev

package dev

// ssr_dev.go：开发态 SSR bundle 的后台重编译与原子热换。
//
// 与浏览器模块不同，SSR 产物是一个覆盖整个服务端入口的 bundle，无法按模块
// HMR，只能"重编译 → 建新渲染池 → 原子替换"。替换用 ssrMu 的写锁完成：
// 在途 RenderHTML 持读锁，换池会等它们退出后才关闭旧池，避免 goja VM 在
// 执行中被回收。

import (
	"fmt"
	"time"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/plunjoin/webfront/compiler"
)

// SSRStatus 是开发态 SSR 的运行状态，供项目侧日志/健康检查读取。
type SSRStatus struct {
	Entry         string `json:"entry"`
	RendererReady bool   `json:"rendererReady"`
	Builds        int64  `json:"builds"`
	Failed        bool   `json:"failed"`
	LastError     string `json:"lastError,omitempty"`
}

func (d *DevServer) SSRStatus() SSRStatus {
	d.ssrStateMu.Lock()
	st := SSRStatus{
		Entry:     d.ssrEntry,
		Builds:    d.ssrBuilds,
		Failed:    d.ssrHadError,
		LastError: d.ssrLastErr,
	}
	d.ssrStateMu.Unlock()
	d.ssrMu.RLock()
	st.RendererReady = d.ssrRenderer != nil
	d.ssrMu.RUnlock()
	return st
}

// kickSSR 通知后台重编译；容量 1 的 channel 天然合并连续保存。
func (d *DevServer) kickSSR() {
	if d.ssrTrigger == nil || d.isClosed() {
		return
	}
	select {
	case d.ssrTrigger <- struct{}{}:
	default:
	}
}

func (d *DevServer) ssrLoop() {
	for {
		select {
		case <-d.closed:
			return
		case <-d.ssrTrigger:
			d.rebuildSSR(true)
		}
	}
}

// rebuildSSR 同步编译一次 SSR bundle 并替换渲染器。
//
// notify=false 用于 NewDevServer 的首次编译：此时还没有浏览器客户端，
// 广播没有意义，也不该因为源码暂不完整而打扰启动日志。
func (d *DevServer) rebuildSSR(notify bool) {
	if d.ssrEntry == "" || d.ssrFactory == nil || d.isClosed() {
		return
	}
	// 开发态要保留 development define 与 source map，但 SSR 产物最终跑在
	// goja 里，语法必须和构建期一样降到 ES2017。DevelopmentProfile 默认
	// ESNext 是给浏览器 dev 客户端用的，这里显式覆盖。
	profile := compiler.DevelopmentProfile()
	profile.Target = api.ES2017
	// 开发态 SSR 和浏览器 dev 模块要用同一套 define：Vue 的
	// __VUE_OPTIONS_API__ 等特性开关只有构建期注入，缺了 dev runtime
	// 会在 initFeatureFlags 里发警告（甚至 ReferenceError）。
	profile.Define = buildDefine(d.rootDeps, d.userDefine)
	bundle, err := compiler.BuildSSRWithOptions(d.root, d.ssrEntry, d.modulesDir, profile)
	if err == nil {
		var next SSRRenderer
		next, err = d.ssrFactory(bundle)
		if err == nil && next == nil {
			err = fmt.Errorf("dev: SSR factory 返回了 nil renderer")
		}
		if err == nil {
			d.swapSSR(next)
			d.ssrStateMu.Lock()
			d.ssrBuilds++
			recovered := d.ssrHadError
			d.ssrHadError = false
			d.ssrLastErr = ""
			d.ssrStateMu.Unlock()
			fmt.Printf("[SSR] bundle rebuilt (%d bytes)\n", len(bundle))
			// 构建失败期间页面已退回 SPA 外壳；恢复后必须整页刷新，
			// 下一次请求才会重新走服务端渲染。
			if notify && recovered && !d.isClosed() {
				d.broadcast(d.ssrBatch(hmrUpdate{Type: "full-reload", Reason: "SSR bundle recovered"}))
			}
			return
		}
	}
	if err == nil {
		err = fmt.Errorf("dev: SSR build failed")
	}
	msg := err.Error()
	d.recordError(d.ssrEntry, msg)
	d.ssrStateMu.Lock()
	d.ssrHadError = true
	d.ssrLastErr = msg
	d.ssrStateMu.Unlock()
	fmt.Printf("[SSR] build failed: %v\n", err)
	if notify && !d.isClosed() {
		d.broadcast(d.ssrBatch(hmrUpdate{Type: "error", Path: d.ssrEntry, Message: msg}))
	}
}

// swapSSR 原子替换渲染器。写锁会等在途渲染结束；旧池在解锁后关闭，
// 此时已不可能有新请求拿到它。
func (d *DevServer) swapSSR(next SSRRenderer) {
	d.ssrMu.Lock()
	old := d.ssrRenderer
	d.ssrRenderer = next
	d.ssrMu.Unlock()
	if old != nil && old != next {
		_ = old.Close()
	}
}

func (d *DevServer) ssrBatch(update hmrUpdate) hmrBatch {
	return hmrBatch{
		Type:      "batch",
		Seq:       d.nextSeq(),
		BuildID:   d.buildID,
		Timestamp: time.Now().UnixMilli(),
		Updates:   []hmrUpdate{update},
	}
}

// nextSeq 给不经过 flushChanges 的批次（SSR 错误/恢复）分配单调序号。
func (d *DevServer) nextSeq() int64 {
	d.hmrMu.Lock()
	d.seq++
	seq := d.seq
	d.hmrMu.Unlock()
	return seq
}
