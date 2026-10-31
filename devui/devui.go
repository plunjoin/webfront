// Package devui 提供开发态的错误界面：一块 Vite 风格的错误覆盖层（注入 dev 页面），
// 以及一张独立的错误页（SSR / goja 渲染失败时直接当响应体返回）。
//
// 设计约束：
//   - 只依赖标准库，不 import 任何 webfront 子包 —— 生产二进制里的 webfront.Engine
//     也能引用它（只在 Config.Dev / WF_DEV 时才真的输出），不会把 esbuild、
//     goja、fsnotify 拖进来。
//   - 样式与结构都塞进 Shadow DOM：宿主页面的 `* { color: #fff }`、`body > div`
//     之类的规则改不动它，错误信息不会因为样式冲突而看不见。
//   - 所有文本走 textContent / HTML 转义，错误信息里出现 <script> 也只是文本。
package devui

import (
	"encoding/json"
	htmlpkg "html"
	"strings"
	"time"
)

// overlayCSS 是覆盖层与错误页共用的样式。
// 注意：不要在里面写 %（ClientScript 虽然不走 Sprintf，但保持习惯更安全）。
const overlayCSS = `.wf-ov,.wf-ov *{box-sizing:border-box;margin:0;padding:0;border:0}
.wf-ov{position:fixed;inset:0;z-index:2147483647;display:flex;align-items:center;justify-content:center;padding:24px;color:#e6e8eb;font:14px/1.6 ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif;-webkit-font-smoothing:antialiased;text-align:left}
.wf-ov-backdrop{position:absolute;inset:0;background:rgba(9,11,15,.82);-webkit-backdrop-filter:blur(3px);backdrop-filter:blur(3px);overscroll-behavior:contain}
.wf-ov-panel{position:relative;display:flex;flex-direction:column;width:min(880px,100%);max-height:100%;overflow:hidden;background:#171a21;border:1px solid rgba(255,255,255,.09);border-radius:12px;box-shadow:0 24px 72px rgba(0,0,0,.62);animation:wf-ov-in .16s ease-out}
@keyframes wf-ov-in{from{opacity:0;transform:translateY(6px) scale(.995)}to{opacity:1;transform:none}}
.wf-ov-head{display:flex;align-items:flex-start;gap:10px;padding:14px 14px 14px 16px;border-bottom:1px solid rgba(255,255,255,.07);background:linear-gradient(180deg,rgba(255,255,255,.04),rgba(255,255,255,0))}
.wf-ov-dot{flex:none;width:9px;height:9px;margin-top:6px;border-radius:50%;background:#f87171;box-shadow:0 0 0 3px rgba(248,113,113,.16)}
.wf-ov[data-kind=server] .wf-ov-dot{background:#fbbf24;box-shadow:0 0 0 3px rgba(251,191,36,.16);animation:wf-ov-pulse 1.1s ease-in-out infinite}
@keyframes wf-ov-pulse{50%{opacity:.3}}
.wf-ov-titles{flex:1;min-width:0}
.wf-ov-title{font-size:14px;font-weight:600;color:#f3f4f6;letter-spacing:.01em}
.wf-ov-file{margin-top:2px;color:#8b93a1;font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;word-break:break-all}
.wf-ov-acts{flex:none;display:flex;align-items:center;gap:6px}
.wf-ov-btn{-webkit-appearance:none;appearance:none;padding:6px 10px;border:1px solid rgba(255,255,255,.12);border-radius:6px;background:rgba(255,255,255,.04);color:#cbd2dc;font:12px/1 ui-sans-serif,system-ui,sans-serif;cursor:pointer;transition:background .12s,color .12s}
.wf-ov-btn:hover{background:rgba(255,255,255,.1);color:#fff}
.wf-ov-btn:active{transform:translateY(1px)}
.wf-ov-x{width:26px;height:26px;padding:0;font-size:15px;line-height:1;color:#9aa3b0}
.wf-ov-body{flex:1;min-height:0;overflow:auto;padding:14px 16px 16px}
.wf-ov-msg{margin-bottom:10px;color:#ffd7d7;font:13px/1.65 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;white-space:pre-wrap;word-break:break-word}
.wf-ov-msg:empty{display:none}
.wf-ov-frame{padding:12px 14px;border:1px solid rgba(255,255,255,.06);border-radius:8px;background:#0d1015;color:#c9d1d9;font:12.5px/1.62 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;white-space:pre;overflow:auto;tab-size:2;max-height:52vh}
.wf-ov-frame:empty{display:none}
.wf-ov-gutter{color:#4d5866}
.wf-ov-caret{color:#f87171}
.wf-ov-err{color:#f87171;font-weight:600}
.wf-ov-foot{display:flex;justify-content:space-between;gap:12px;padding:9px 16px;border-top:1px solid rgba(255,255,255,.06);background:rgba(0,0,0,.18);color:#6b7482;font-size:11.5px}
@media (max-width:600px){.wf-ov{padding:0}.wf-ov-panel{width:100%;max-height:100%;border:0;border-radius:0}}`

// pageCSS 只给独立错误页用（覆盖层在 Shadow DOM 里，页面背景由宿主页面决定）。
const pageCSS = `html,body{margin:0;padding:0;height:100%;background:#0b0d12}`

// overlayJS 是覆盖层客户端代码。__WF_CSS__ 会被替换成一个 JSON 字符串字面量。
// 约定：不含反引号（Go 原始字符串装不下），也不含 %（避免被 Sprintf 误当动词）。
const overlayJS = `(function () {
  if (typeof window === 'undefined' || window.__wfOverlay) return;

  var CSS = __WF_CSS__;

  var TPL =
    '<div class="wf-ov">' +
      '<div class="wf-ov-backdrop"></div>' +
      '<div class="wf-ov-panel" role="dialog" aria-modal="true">' +
        '<div class="wf-ov-head">' +
          '<span class="wf-ov-dot"></span>' +
          '<div class="wf-ov-titles">' +
            '<div class="wf-ov-title"></div>' +
            '<div class="wf-ov-file"></div>' +
          '</div>' +
          '<div class="wf-ov-acts">' +
            '<button class="wf-ov-btn" type="button" data-act="copy">复制</button>' +
            '<button class="wf-ov-btn" type="button" data-act="reload">重新加载</button>' +
            '<button class="wf-ov-btn wf-ov-x" type="button" data-act="close" title="关闭（Esc）">&times;</button>' +
          '</div>' +
        '</div>' +
        '<div class="wf-ov-body">' +
          '<div class="wf-ov-msg"></div>' +
          '<pre class="wf-ov-frame"></pre>' +
        '</div>' +
        '<div class="wf-ov-foot"><span class="wf-ov-hint"></span><span class="wf-ov-when"></span></div>' +
      '</div>' +
    '</div>';

  var host = null, shadow = null, el = null, state = null;

  function esc(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  // 把 esbuild 的代码框涂上色：行号灰、插入符红、X [ERROR] 红。
  // 没有代码框（比如 goja 的 JS 栈）就原样转义输出。
  function frameHTML(text) {
    var lines = String(text).split('\n');
    var out = [];
    for (var i = 0; i < lines.length; i++) {
      var l = lines[i];
      var g = /^(\s*\d+\s*│)([\s\S]*)$/.exec(l);
      if (g) { out.push('<span class="wf-ov-gutter">' + esc(g[1]) + '</span>' + esc(g[2])); continue; }
      if (/^\s*[│╵]/.test(l)) { out.push('<span class="wf-ov-caret">' + esc(l) + '</span>'); continue; }
      if (/^X \[ERROR\]/.test(l)) { out.push('<span class="wf-ov-err">' + esc(l) + '</span>'); continue; }
      if (/^\s+at\s/.test(l)) { out.push('<span class="wf-ov-gutter">' + esc(l) + '</span>'); continue; }
      out.push(esc(l));
    }
    return out.join('\n');
  }

  // esbuild 的报文形如 "X [ERROR] msg\n\n  file:1:2:\n  ..."，
  // 把首行抬成标题，剩下的当代码框，视觉上更像 Vite。
  function splitDetail(detail) {
    var text = String(detail == null ? '' : detail).replace(/\r\n/g, '\n').replace(/\s+$/, '');
    var lines = text.split('\n');
    var head = '';
    var first = lines[0] || '';
    var m = /^X \[ERROR\]\s*([\s\S]*)$/.exec(first);
    if (m) {
      head = m[1];
      lines.shift();
    } else if (lines.length > 1 && first.length > 0 && first.length <= 200) {
      head = first;
      lines.shift();
    }
    while (lines.length && !lines[0].trim()) lines.shift();
    return { head: head, frame: lines.join('\n') };
  }

  function ensure() {
    if (host && host.isConnected) return true;
    var de = document.documentElement;
    if (!de) return false;
    host = document.createElement('div');
    host.id = '__wf_dev_overlay';
    host.style.cssText = 'position:fixed;inset:0;z-index:2147483647;display:block';
    shadow = host.attachShadow ? host.attachShadow({ mode: 'open' }) : host;
    shadow.innerHTML = '<style>' + CSS + '</style>' + TPL;
    de.appendChild(host);
    el = {
      wrap: shadow.querySelector('.wf-ov'),
      backdrop: shadow.querySelector('.wf-ov-backdrop'),
      title: shadow.querySelector('.wf-ov-title'),
      file: shadow.querySelector('.wf-ov-file'),
      msg: shadow.querySelector('.wf-ov-msg'),
      frame: shadow.querySelector('.wf-ov-frame'),
      hint: shadow.querySelector('.wf-ov-hint'),
      when: shadow.querySelector('.wf-ov-when'),
      reload: shadow.querySelector('[data-act="reload"]')
    };
    shadow.addEventListener('click', onClick);
    return true;
  }

  function onClick(e) {
    var t = e.target;
    var act = t && t.getAttribute ? t.getAttribute('data-act') : null;
    if (act === 'close') { hide(); return; }
    if (act === 'reload') { location.reload(); return; }
    if (act === 'copy') { copyDetail(); return; }
    if (t === el.backdrop) hide();
  }

  function copyDetail() {
    if (!state) return;
    var text = state.title + '\n' + (state.file || '') + '\n\n' + state.detail;
    var btn = shadow.querySelector('[data-act="copy"]');
    var done = function () {
      if (!btn) return;
      btn.textContent = '已复制';
      setTimeout(function () { btn.textContent = '复制'; }, 1200);
    };
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, done);
        return;
      }
    } catch (_) {}
    done();
  }

  function show(opts) {
    opts = opts || {};
    if (!ensure()) return;
    var kind = opts.kind || 'error';
    var parts = splitDetail(opts.detail);
    var title = opts.title || (kind === 'server' ? '后端重启中' : '编译失败');
    el.wrap.setAttribute('data-kind', kind);
    el.title.textContent = title;
    el.file.textContent = opts.file || '';
    el.file.style.display = opts.file ? '' : 'none';
    el.msg.textContent = parts.head;
    el.frame.innerHTML = frameHTML(parts.frame);
    el.hint.textContent = opts.hint || (kind === 'server' ? '服务回来后本页会自动刷新' : '修好后会自动刷新 · 按 Esc 关闭');
    el.when.textContent = opts.time || new Date().toLocaleTimeString();
    if (el.reload) el.reload.style.display = kind === 'server' ? 'none' : '';
    state = { kind: kind, title: title, file: opts.file || '', detail: String(opts.detail == null ? '' : opts.detail) };
  }

  function hide() {
    state = null;
    if (host && host.parentNode) host.parentNode.removeChild(host);
    host = null; shadow = null; el = null;
  }

  // Esc 走捕获阶段：即使页面自己 stopPropagation 也拦得住。
  window.addEventListener('keydown', function (e) {
    if (!state) return;
    if (e.key === 'Escape' || e.key === 'Esc') {
      e.preventDefault();
      e.stopPropagation();
      hide();
    }
  }, true);

  // 运行期错误也摆到台面上（Vite 的行为）。资源加载失败（img/script 的
  // error 事件 target 是元素）跳过，不然满屏都是 404 图片。
  var lastRuntime = '';

  function runtime(kind, err) {
    var detail = (err && err.stack) ? String(err.stack) : String(err);
    var sig = kind + '|' + detail.slice(0, 240);
    if (sig === lastRuntime && state) return;
    lastRuntime = sig;
    show({
      kind: 'error',
      title: kind === 'rejection' ? '未处理的 Promise 拒绝' : '运行期错误',
      file: location.pathname,
      detail: detail,
      hint: '浏览器里抛的异常 · 按 Esc 关闭'
    });
  }

  window.addEventListener('error', function (e) {
    if (!e) return;
    var t = e.target;
    if (t && t !== window && t !== document && t.tagName) return;
    var err = e.error;
    if (!err) {
      if (!e.message) return;
      err = e.message + (e.filename ? ' (' + e.filename + ':' + e.lineno + ':' + e.colno + ')' : '');
    }
    try { runtime('error', err); } catch (_) {}
  });

  window.addEventListener('unhandledrejection', function (e) {
    try { runtime('rejection', e && e.reason); } catch (_) {}
  });

  window.__wfOverlay = {
    show: function (o) { try { show(o); } catch (e) { console.error('[wf overlay]', e); } },
    hide: hide,
    kind: function () { return state ? state.kind : ''; }
  };
})();
`

// ClientScript 返回注入 dev 页面的覆盖层脚本（不含 <script> 标签）。
// 暴露 window.__wfOverlay = { show, hide, kind }。
func ClientScript() string {
	// json.Marshal 默认把 < > & 转成 \u003c 等，天然避免 CSS/文本里的
	// </script> 提前截断内联脚本。
	css, err := json.Marshal(overlayCSS)
	if err != nil {
		css = []byte(`""`)
	}
	return strings.Replace(overlayJS, "__WF_CSS__", string(css), 1)
}

// PageOptions 是独立错误页的内容。
type PageOptions struct {
	Title  string // 标题，如 "SSR 渲染失败"
	File   string // 副标题里的定位信息，如 "/about"
	Detail string // 原始错误文本（含 goja 栈）
	Hint   string // 页脚提示
	Time   string // 时间，留空取当前时间
}

// ErrorPage 生成一张独立的错误页（完整 HTML）。
// 用于 SSR / goja 渲染失败：此时根本没有前端可挂载，只能整页替换。
func ErrorPage(o PageOptions) string {
	if o.Title == "" {
		o.Title = "渲染失败"
	}
	if o.Time == "" {
		o.Time = time.Now().Format("15:04:05")
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString("<title>" + htmlpkg.EscapeString(o.Title) + " · webfront</title>")
	b.WriteString("<style>" + overlayCSS + pageCSS + "</style></head><body>")

	b.WriteString(`<div class="wf-ov"><div class="wf-ov-backdrop"></div>`)
	b.WriteString(`<div class="wf-ov-panel" role="dialog" aria-modal="true">`)
	b.WriteString(`<div class="wf-ov-head"><span class="wf-ov-dot"></span><div class="wf-ov-titles">`)
	b.WriteString(`<div class="wf-ov-title">` + htmlpkg.EscapeString(o.Title) + `</div>`)
	if o.File != "" {
		b.WriteString(`<div class="wf-ov-file">` + htmlpkg.EscapeString(o.File) + `</div>`)
	}
	b.WriteString(`</div><div class="wf-ov-acts">`)
	b.WriteString(`<button class="wf-ov-btn" type="button" data-act="copy">复制</button>`)
	b.WriteString(`<button class="wf-ov-btn" type="button" data-act="reload">重新加载</button>`)
	b.WriteString(`</div></div>`)

	b.WriteString(`<div class="wf-ov-body">`)
	b.WriteString(`<pre class="wf-ov-frame" id="wf-detail">` + colorizeStack(o.Detail) + `</pre>`)
	b.WriteString(`</div>`)

	b.WriteString(`<div class="wf-ov-foot"><span class="wf-ov-hint">`)
	if o.Hint != "" {
		b.WriteString(htmlpkg.EscapeString(o.Hint))
	} else {
		b.WriteString("开发态错误页 · 修好后刷新即可")
	}
	b.WriteString(`</span><span class="wf-ov-when">` + htmlpkg.EscapeString(o.Time) + `</span></div>`)

	b.WriteString(`</div></div>`)
	b.WriteString(`<script>(function(){` +
		`var b=document.querySelector('[data-act="copy"]');` +
		`var r=document.querySelector('[data-act="reload"]');` +
		`if(r){r.addEventListener('click',function(){location.reload()})}` +
		`if(!b)return;` +
		`b.addEventListener('click',function(){` +
		`var t=document.getElementById('wf-detail');` +
		`var s=t?t.textContent:'';` +
		`var d=function(){b.textContent='已复制';setTimeout(function(){b.textContent='复制'},1200)};` +
		`if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(s).then(d,d);return}` +
		`d()})})();</script>`)
	b.WriteString(`</body></html>`)
	return b.String()
}

// colorizeStack 把 JS 栈帧（"    at fn (file:1:2)"）压暗，突出第一行错误消息。
func colorizeStack(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "at ") {
			lines[i] = `<span class="wf-ov-gutter">` + htmlpkg.EscapeString(l) + `</span>`
			continue
		}
		lines[i] = htmlpkg.EscapeString(l)
	}
	return strings.Join(lines, "\n")
}
