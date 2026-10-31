package dev

import (
	"strconv"
	"strings"

	"github.com/plunjoin/webfront/devui"
)

// hmrJS 是注入 dev 页面的客户端脚本（不含 <script> 标签）。
//
// 三件事：
//  1. 连 /__wf_hmr，收到 update 就 import() 热替换；
//  2. 失败时把真正的编译错误摆到前台（错误覆盖层），而不是白屏或一句
//     "Failed to fetch dynamically imported module"；
//  3. Go 进程重启（wf dev 重新编译）期间轮询 /__wf_ping，回来后整页重载。
//
// __WF_ENTRY__ 会被替换成带引号的入口路径；用 ReplaceAll 而不是 Sprintf，
// 免得将来往 JS 里写个 % 就炸。
const hmrJS = `
var ENTRY = __WF_ENTRY__;

var ov = window.__wfOverlay || {
  show: function () {},
  hide: function () {},
  kind: function () { return ''; }
};

var render = null;
var connected = false;
// 服务端最近一次编译错误的序号：用来在"嵌套依赖报错"时取回真正的错误原文。
var errSeq = 0;

function isCSS(p) {
  return /\.css(\?|$)/.test(p);
}

// import() 失败时浏览器只说 "Failed to fetch dynamically imported module"，
// 真正的错误在 500 的响应体里。
//   - 直接请求本模块 500  → 用它自己的错误
//   - 本模块 200（说明是它 import 的某个子模块坏了）→ 问 /__wf_error 要最近一条
async function explain(path, err) {
  var detail = '';
  var file = path;
  try {
    var r = await fetch(path + '?t=' + Date.now(), { cache: 'no-store' });
    if (!r.ok) detail = await r.text();
  } catch (_) {}
  if (!detail) {
    try {
      var er = await fetch('/__wf_error?since=' + errSeq, { cache: 'no-store' });
      if (er.ok) {
        var j = await er.json();
        if (j && j.body) {
          detail = j.body;
          file = j.path || path;
          errSeq = j.id || errSeq;
        }
      }
    } catch (_) {}
  }
  if (!detail) detail = (err && err.message) ? err.message : String(err);
  return { file: file, detail: detail };
}

async function report(path, err) {
  var info = await explain(path, err);
  ov.show({ kind: 'error', title: '编译失败', file: info.file, detail: info.detail });
}

function connect() {
  var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
  var ws = new WebSocket(proto + location.host + '/__wf_hmr');

  ws.onopen = function () {
    connected = true;
    // 只收掉"重启中"的提示；运行期错误是用户正在看的，别抢。
    if (ov.kind() === 'server') ov.hide();
  };

  ws.onmessage = async function (e) {
    var m;
    try { m = JSON.parse(e.data); } catch (_) { return; }
    if (!m || m.type !== 'update') return;
    try {
      var mod = await import(m.path + '?t=' + Date.now());
      ov.hide();
      if (mod && typeof mod.render === 'function') {
        render = mod.render;
        render();
      } else if (isCSS(m.path)) {
        // .css 模块自己把 <style> 换掉了，不用刷新页面
      } else {
        // 普通子组件（App.tsx / App.vue 等）：轻量整页刷新，穿透 ESM 缓存
        location.reload();
      }
    } catch (err) {
      // 编译失败时绝不 reload：那会变成白屏。把错误摆在页面上，
      // 等下一次成功的 update 自然收场。
      console.error('[HMR] update failed', err);
      await report(m.path, err);
    }
  };

  // 断开只有两种可能：页面正在跳转，或服务端没了（wf dev 正在重启 Go 进程）。
  // 后者的正确姿势是等新进程起来再整页重载，而不是立刻 reload。
  ws.onclose = function () {
    if (!connected) return;
    connected = false;
    // 600ms 才开始提示：实测 kill + 重启只有 300~500ms，太早弹层会闪一下。
    setTimeout(function () {
      ov.show({
        kind: 'server',
        title: '后端重启中',
        file: location.host,
        detail: 'wf dev 正在重新编译 Go 代码并重启进程。',
        hint: '服务回来后本页会自动刷新'
      });
    }, 600);
    (async function () {
      for (;;) {
        try {
          await fetch('/__wf_ping?t=' + Date.now(), { cache: 'no-store' });
          break;
        } catch (_) {
          await new Promise(function (r) { setTimeout(r, 300); });
        }
      }
      location.reload();
    })();
  };
}
connect();

import(ENTRY + '?t=' + Date.now()).then(function (m) {
  ov.hide();
  // main.* 约定导出 render() 以便热替换；entry-client.* 自己挂载，没有 render 也正常。
  if (m && typeof m.render === 'function') { render = m.render; render(); }
}).catch(function (e) {
  console.error('[HMR] initial import failed', e);
  report(ENTRY, e);
});
`

// clientScript 返回注入 index.html 的完整内联脚本内容（覆盖层 + HMR）。
func clientScript(entry string) string {
	js := strings.ReplaceAll(hmrJS, "__WF_ENTRY__", strconv.Quote(entry))
	return devui.ClientScript() + js
}
