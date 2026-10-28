// Package jsruntime 的 goja 后端：纯 Go 的 ECMAScript 引擎，链接成本 ≈ +9.5MB。
// 覆盖 ES5.1 + ES6 主体（class/字段/私有字段/Proxy/Promise 等，接近 ES2022），
// React 18 与 Vue 3 的 SSR 产物实测都能跑；新语法由 esbuild Target=ES2017 降级兜底。
package jsruntime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// Pool 是 goja 虚拟机池：预编译 SSR bundle，一次只服务一个 goroutine，支持请求超时中断与自动重建。
type Pool struct {
	ch         chan *goja.Runtime
	shimProg   *goja.Program
	bundleProg *goja.Program
	renderProg *goja.Program
	timeout    time.Duration
}

// New 预编译 bundle 并创建 size 台 VM（默认 2 台），默认渲染超时 5s。
func New(bundle []byte, size int) (*Pool, error) {
	return NewWithTimeout(bundle, size, 5*time.Second)
}

// NewWithTimeout 允许自定义单次 SSR 渲染超时时间。
func NewWithTimeout(bundle []byte, size int, timeout time.Duration) (*Pool, error) {
	if size <= 0 {
		size = 2
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	shimProg, err := goja.Compile("shim.js", shim, false)
	if err != nil {
		return nil, fmt.Errorf("compile jsruntime shim: %w", err)
	}
	bundleProg, err := goja.Compile("ssr.js", string(bundle), false)
	if err != nil {
		return nil, fmt.Errorf("compile SSR bundle: %w", err)
	}
	renderProg, err := goja.Compile("render.js", renderScript, false)
	if err != nil {
		return nil, fmt.Errorf("compile render script: %w", err)
	}

	p := &Pool{
		ch:         make(chan *goja.Runtime, size),
		shimProg:   shimProg,
		bundleProg: bundleProg,
		renderProg: renderProg,
		timeout:    timeout,
	}

	for i := 0; i < size; i++ {
		vm, err := p.createVM()
		if err != nil {
			return nil, err
		}
		p.ch <- vm
	}
	return p, nil
}

// SetTimeout 设置单次渲染超时。
func (p *Pool) SetTimeout(d time.Duration) {
	p.timeout = d
}

func (p *Pool) createVM() (*goja.Runtime, error) {
	vm := goja.New()
	if _, err := vm.RunProgram(p.shimProg); err != nil {
		return nil, fmt.Errorf("jsruntime shim: %w", err)
	}
	if _, err := vm.RunProgram(p.bundleProg); err != nil {
		return nil, fmt.Errorf("load SSR bundle: %w", err)
	}
	return vm, nil
}

// shim 补齐 SSR 产物会碰到的宿主全局。
const shim = `
if (typeof globalThis === 'undefined') { globalThis = this; }
if (typeof globalThis.__WF_STYLES__ === 'undefined') { globalThis.__WF_STYLES__ = []; }
function TextEncoder(){}
TextEncoder.prototype.encode = function(str){
  var out=[];
  for (var i=0;i<str.length;i++){
    var c=str.charCodeAt(i);
    if (c<0x80) out.push(c);
    else if (c<0x800) out.push(0xc0|(c>>6),0x80|(c&63));
    else if (c<0xd800||c>=0xe000) out.push(0xe0|(c>>12),0x80|((c>>6)&63),0x80|(c&63));
    else { i++; var c2=str.charCodeAt(i); var cp=0x10000+((c-0xd800)<<10)+(c2-0xdc00);
           out.push(0xf0|(cp>>18),0x80|((cp>>12)&63),0x80|((cp>>6)&63),0x80|(cp&63)); }
  }
  return new Uint8Array(out);
};
globalThis.TextEncoder = TextEncoder;
function TextDecoder(){}
TextDecoder.prototype.decode = function(){ return ""; };
globalThis.TextDecoder = TextDecoder;
if (typeof globalThis.queueMicrotask === 'undefined') {
  globalThis.queueMicrotask = function(f){ Promise.resolve().then(f); };
}
`

// renderScript 统一用 Promise.resolve().then 收口：
// React 的 renderToString 同步返回对象，Vue 的 renderToString 返回 Promise，两者都能接。
// 收集 Vue 组件推入 __WF_STYLES__ 的样式并追加进 head，杜绝 SSR 首屏闪烁（FOUC）。
const renderScript = `
globalThis.__WF_OUT__ = null;
globalThis.__WF_ERR__ = null;
globalThis.__WF_STYLES__ = [];
Promise.resolve(globalThis.__WF_APP__.render(globalThis.__WF_URL__, globalThis.__WF_INITIAL_PROPS__))
  .then(function(v){
    if (v && typeof v === 'object' && Array.isArray(globalThis.__WF_STYLES__) && globalThis.__WF_STYLES__.length > 0) {
      var styleTags = '';
      for (var i = 0; i < globalThis.__WF_STYLES__.length; i++) {
        styleTags += '<style>' + globalThis.__WF_STYLES__[i] + '</style>';
      }
      v.head = (v.head || '') + styleTags;
    }
    globalThis.__WF_OUT__ = v;
  },
  function(e){ globalThis.__WF_ERR__ = String((e && e.stack) || e); });
`

// RenderHTML 渲染一个页面，返回 (html 片段, head 片段)。
func (p *Pool) RenderHTML(url string, propsJSON []byte) (string, string, error) {
	ctx := context.Background()
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	return p.RenderHTMLContext(ctx, url, propsJSON)
}

// RenderHTMLContext 接收 Context，支持外部超时与取消控制。
func (p *Pool) RenderHTMLContext(ctx context.Context, url string, propsJSON []byte) (html string, head string, err error) {
	var vm *goja.Runtime
	select {
	case <-ctx.Done():
		return "", "", fmt.Errorf("acquire VM failed: %w", ctx.Err())
	case vm = <-p.ch:
	}

	healthy := true
	defer func() {
		if r := recover(); r != nil {
			healthy = false
			err = fmt.Errorf("ssr render panic: %v", r)
		}
		if healthy {
			_, _ = vm.RunString(`delete globalThis.__WF_INITIAL_PROPS__; delete globalThis.__WF_URL__; ` +
				`delete globalThis.__WF_OUT__; delete globalThis.__WF_ERR__; delete globalThis.__WF_STYLES__;`)
			p.ch <- vm
		} else {
			// 发生超时/死循环中断或严重异常，销毁旧 VM，用预编译 Program 快速重建新 VM 补回池中
			newVM, createErr := p.createVM()
			if createErr == nil {
				p.ch <- newVM
			}
		}
	}()

	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			vm.Interrupt("ssr render timeout")
		}
	}()

	if _, runErr := vm.RunString("globalThis.__WF_INITIAL_PROPS__ = " + string(propsJSON)); runErr != nil {
		if ctx.Err() != nil {
			healthy = false
			return "", "", fmt.Errorf("render timeout: %w", ctx.Err())
		}
		return "", "", runErr
	}
	if _, runErr := vm.RunString("globalThis.__WF_URL__ = " + quote(url)); runErr != nil {
		if ctx.Err() != nil {
			healthy = false
			return "", "", fmt.Errorf("render timeout: %w", ctx.Err())
		}
		return "", "", runErr
	}
	if _, runErr := vm.RunProgram(p.renderProg); runErr != nil {
		if ctx.Err() != nil || strings.Contains(runErr.Error(), "interrupted") || strings.Contains(runErr.Error(), "timeout") {
			healthy = false
			return "", "", fmt.Errorf("render timeout: %w", ctx.Err())
		}
		return "", "", runErr
	}

	if e := vm.Get("__WF_ERR__"); e != nil && !goja.IsNull(e) && !goja.IsUndefined(e) {
		return "", "", fmt.Errorf("render: %s", e.String())
	}
	out := vm.Get("__WF_OUT__")
	if out == nil || goja.IsNull(out) || goja.IsUndefined(out) {
		return "", "", fmt.Errorf("render 返回空值（bundle 里的 __WF_APP__.render 没给结果）")
	}
	obj := out.ToObject(vm)
	return obj.Get("html").String(), obj.Get("head").String(), nil
}

// Close 关闭池内所有 VM（goja 无显式销毁，交给 GC）。
func (p *Pool) Close() error {
	for i := 0; i < cap(p.ch); i++ {
		select {
		case <-p.ch:
		default:
			return nil
		}
	}
	return nil
}
