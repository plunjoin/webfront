//go:build dev

// runtime.go：浏览器端 HMR runtime（协议 v2）与它的三个入口。
//
//	GET /@wf/client   ESM 单例：createHotContext / accept / dispose / data / prune
//	GET /@wf/boot     ESM 引导：连上 runtime 后 import 应用入口
//	GET /@wf/overlay.js 经典脚本：错误覆盖层（CSP 场景不依赖内联脚本）
//
// 每个 dev 源模块由 esbuild 的 banner 注入
//
//	import { createHotContext } from "/@wf/client"
//	import.meta.hot = createHotContext("/src/App.vue")
//
// 因而 runtime 只加载一次，模块记录以 canonical URL 为键；服务端广播的
// update.path 也用同一套 URL（含 basePath），两边不会错位。
package dev

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/plunjoin/webfront/devui"
)

// clientRuntimeJS 是协议 v2 的客户端实现。模板占位符只有 BASE / BUILD_ID，
// 用 strconv.Quote 替换，避免 basePath 或 buildId 里的字符破坏 JS 字符串。
const clientRuntimeJS = `
const PROTOCOL = 2;
const BASE = __WF_BASE__;
const BUILD_ID = __WF_BUILD_ID__;

const records = new Map();
const dataStore = new Map();
var lastSeq = 0;
var legacySeq = 0;
// 页面加载时服务端就固化在模块里：首次连上的 buildId 若不同，说明
// 中间经历了 Go 重启，直接整页刷新比混用两代模块安全。
var connectedBuild = BUILD_ID;
var connectedCaps = Object.create(null);
var socket = null;
var queue = Promise.resolve();
var importModule = function (url) { return import(url); };
var errSeq = 0;

function overlay() {
  if (typeof window !== 'undefined' && window.__wfOverlay) return window.__wfOverlay;
  return { show: function () {}, hide: function () {}, kind: function () { return ''; } };
}

function withQuery(url, key, value) {
  return url + (url.indexOf('?') >= 0 ? '&' : '?') + key + '=' + encodeURIComponent(String(value));
}

function reload(reason) {
  console.warn('[HMR] full reload:', reason || '');
  if (typeof location !== 'undefined' && typeof location.reload === 'function') location.reload();
}

function recordFor(ownerPath) {
  var rec = records.get(ownerPath);
  if (rec) return rec;
  var data = dataStore.get(ownerPath);
  if (!data) {
    data = {};
    dataStore.set(ownerPath, data);
  }
  rec = {
    id: ownerPath,
    data: data,
    selfAccepted: false,
    acceptCallbacks: [],
    depsAccepted: [],
    disposeCallbacks: [],
    pruneCallbacks: [],
    listeners: Object.create(null),
    declined: false,
    module: null
  };
  records.set(ownerPath, rec);
  return rec;
}

export function createHotContext(ownerPath) {
  var rec = recordFor(ownerPath);
  // 模块重新执行时重新登记回调；data 按设计跨更新保留。
  rec.selfAccepted = false;
  rec.acceptCallbacks = [];
  rec.depsAccepted = [];
  rec.disposeCallbacks = [];
  rec.pruneCallbacks = [];
  // 监听器属于这一次模块执行：旧的闭包不能在新模块求值后继续收到事件。
  rec.listeners = Object.create(null);
  rec.declined = false;

  function hotAccept(deps, cb) {
    if (typeof deps === 'function' || deps == null) {
      rec.selfAccepted = true;
      if (typeof deps === 'function') rec.acceptCallbacks.push(deps);
      return;
    }
    var list = Array.isArray(deps) ? deps : [deps];
    if (typeof cb !== 'function') {
      rec.selfAccepted = true;
      return;
    }
    rec.depsAccepted.push({ deps: list, cb: cb });
  }

  var hot = {
    get data() { return rec.data; },
    accept: hotAccept,
    acceptExports: function (_exports, cb) { hotAccept(cb); },
    dispose: function (cb) {
      if (typeof cb === 'function') rec.disposeCallbacks.push(cb);
    },
    prune: function (cb) {
      if (typeof cb === 'function') rec.pruneCallbacks.push(cb);
    },
    decline: function () { rec.declined = true; },
    invalidate: function (message) {
      send({ type: 'invalidate', path: ownerPath, message: message || '' });
      reload('invalidate: ' + (message || ownerPath));
    },
    send: function (type, data) {
      send({ type: type, path: ownerPath, data: data });
    },
    on: function (event, cb) {
      if (typeof event !== 'string' || typeof cb !== 'function') return;
      (rec.listeners[event] || (rec.listeners[event] = [])).push(cb);
    }
  };
  return hot;
}

function emitRecord(path, event, payload) {
  var rec = records.get(path);
  if (!rec) return;
  var list = rec.listeners[event];
  if (!list) return;
  for (var i = 0; i < list.length; i++) {
    try { list[i](payload); } catch (e) { console.error('[HMR] custom event failed', event, e); }
  }
}

function send(msg) {
  if (socket && socket.readyState === 1) {
    try { socket.send(JSON.stringify(msg)); } catch (e) { console.warn('[HMR] send failed', e); }
  }
}

function pruneRecord(path) {
  var rec = records.get(path);
  if (rec) {
    for (var i = 0; i < rec.pruneCallbacks.length; i++) {
      try { rec.pruneCallbacks[i](rec.data); } catch (e) { console.error('[HMR] prune callback failed', path, e); }
    }
    records.delete(path);
  }
  dataStore.delete(path);
}

function hideErrorOverlay() {
  var ov = overlay();
  if (typeof ov.kind === 'function' && ov.kind() === 'error') ov.hide();
}

function showError(file, detail) {
  overlay().show({ kind: 'error', title: '编译失败', file: file || '', detail: detail || '' });
}

async function explain(path, err) {
  var detail = '';
  var file = path;
  try {
    var r = await fetch(withQuery(path, 't', Date.now()), { cache: 'no-store' });
    if (!r.ok) detail = await r.text();
  } catch (_) {}
  if (!detail) {
    try {
      var er = await fetch(BASE + '/__wf_error?since=' + errSeq, { cache: 'no-store' });
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

export async function reportImportError(path, err) {
  var info = await explain(path, err);
  showError(info.file, info.detail);
}

var KNOWN_KINDS = { js: 1, ts: 1, jsx: 1, tsx: 1, vue: 1, css: 1, json: 1, asset: 1 };

async function applyUpdate(update, seq) {
  if (!update || typeof update !== 'object') {
    reload('invalid update');
    return;
  }
  if (update.type === 'full-reload') {
    reload(update.reason || 'full reload');
    return;
  }
  if (update.type === 'error') {
    showError(update.path || 'build', update.message || 'compile error');
    return;
  }
  if (update.type === 'prune') {
    pruneRecord(update.path || update.id || '');
    return;
  }
  if (update.type !== 'update' && update.type !== 'css') {
    reload('unknown update type: ' + update.type);
    return;
  }
  var kind = update.kind || '';
  if (kind && !KNOWN_KINDS[kind] && !connectedCaps[kind]) {
    reload('unknown update kind: ' + kind);
    return;
  }
  var path = update.path;
  if (!path) {
    reload('update without path');
    return;
  }

  var ctx = {
    mode: update.mode === 'reload' ? 'reload' : 'auto',
    seq: seq,
    path: path
  };
  var old = records.get(path);
  var disposeCallbacks = old ? old.disposeCallbacks.slice() : [];
  var oldData = old ? old.data : undefined;
  // dispose 先于新模块执行（与 Vite 一致）：旧实例的定时器/监听器先拆掉，
  // 再让新模块登记自己的。回调执行后清空，失败重试时不会重复 dispose。
  if (old) old.disposeCallbacks = [];
  for (var i = 0; i < disposeCallbacks.length; i++) {
    try { disposeCallbacks[i](oldData); } catch (e) { console.error('[HMR] dispose failed', path, e); }
  }

  var mod;
  try {
    mod = await importModule(withQuery(path, 't', seq));
  } catch (err) {
    console.error('[HMR] update failed', path, err);
    await reportImportError(path, err);
    return false;
  }

  var next = records.get(path);
  if (!next || !next.selfAccepted) {
    // accept(deps, cb) 在切片 1 按保守策略处理：没有 self-accept 的模块
    // 无法保证新依赖被安全换入，整页刷新而不是留下半新半旧的实例。
    if (next && next.depsAccepted.length) {
      console.warn('[HMR] accept(deps, cb) is conservative in this build; reloading', path);
    }
    reload('module ' + path + ' is no longer self-accepting');
    return false;
  }
  if (next.declined) {
    reload('module ' + path + ' declined HMR');
    return false;
  }
  next.module = mod;
  if (!next.acceptCallbacks.length) {
    // CSS 模块在重新执行时自己替换 <style>；没有 accept 回调也属于完成。
    hideErrorOverlay();
    emitRecord(path, 'update', { path: path, module: mod, mode: ctx.mode, seq: seq });
    return true;
  }
  var acceptFailed = false;
  for (var j = 0; j < next.acceptCallbacks.length; j++) {
    try {
      next.acceptCallbacks[j](mod, ctx);
    } catch (err) {
      console.error('[HMR] accept callback failed', path, err);
      showError(path, (err && err.message) ? err.message : String(err));
      acceptFailed = true;
    }
  }
  if (acceptFailed) return false;
  hideErrorOverlay();
  emitRecord(path, 'update', { path: path, module: mod, mode: ctx.mode, seq: seq });
  return true;
}

async function applyBatch(batch) {
  var updates = Array.isArray(batch.updates) ? batch.updates : [];
  var prunes = Array.isArray(batch.prunes) ? batch.prunes : [];
  for (var i = 0; i < prunes.length; i++) {
    var p = prunes[i];
    pruneRecord(p && (p.path || p.id) ? (p.path || p.id) : '');
  }
  for (var j = 0; j < updates.length; j++) {
    var u = updates[j];
    if (u && u.type === 'full-reload') {
      reload(u.reason || 'full reload');
      return;
    }
  }
  for (var k = 0; k < updates.length; k++) {
    var ok = await applyUpdate(updates[k], batch.seq);
    // 一个边界 import 失败后不再执行同批次的后续边界：页面停在一个
    // 可解释的错误覆盖层上，下一次成功保存再收敛。
    if (ok === false) return;
  }
}

function enqueue(fn) {
  queue = queue.then(fn, fn);
  return queue;
}

function onConnected(msg) {
  if (msg.protocol !== PROTOCOL) {
    console.warn('[HMR] protocol mismatch: server=' + msg.protocol + ' client=' + PROTOCOL);
    reload('protocol mismatch');
    return;
  }
  if (connectedBuild && msg.buildId && msg.buildId !== connectedBuild) {
    connectedBuild = msg.buildId;
    reload('build id changed');
    return;
  }
  connectedBuild = msg.buildId || '';
  connectedCaps = msg.capabilities || Object.create(null);
  lastSeq = 0;
  if (typeof window !== 'undefined') {
    window.__WF_HMR__ = {
      protocol: PROTOCOL,
      buildId: connectedBuild,
      capabilities: msg.capabilities || {}
    };
  }
  var ov = overlay();
  if (typeof ov.kind === 'function' && ov.kind() === 'server') ov.hide();
  hideErrorOverlay();
}

export function __wfHandleMessage(msg) {
  if (!msg || typeof msg.type !== 'string') {
    reload('invalid message');
    return Promise.resolve();
  }
  if (msg.type === 'connected') {
    onConnected(msg);
    return Promise.resolve();
  }
  if (msg.type === 'batch') {
    if (typeof msg.seq === 'number') {
      if (msg.seq <= lastSeq) return Promise.resolve();
      lastSeq = msg.seq;
    }
    if (msg.buildId && connectedBuild && msg.buildId !== connectedBuild) {
      reload('build id changed');
      return Promise.resolve();
    }
    return enqueue(function () { return applyBatch(msg); });
  }
  if (msg.type === 'update' || msg.type === 'css' || msg.type === 'full-reload' ||
      msg.type === 'error' || msg.type === 'prune') {
    var seq = typeof msg.seq === 'number' ? msg.seq : ++legacySeq;
    return enqueue(function () { return applyUpdate(msg, seq); });
  }
  reload('unknown message type: ' + msg.type);
  return Promise.resolve();
}

// 测试与调试钩子：浏览器里可通过 window.__WF_HMR__ 观察协议状态；
// goja 单测用 __wfSetImporter 替换动态 import，完全不需要网络。
export function __wfSetImporter(fn) { importModule = fn; }
export function __wfDebugState() {
  return { lastSeq: lastSeq, buildId: connectedBuild, records: records.size, socket: !!socket };
}

function connect() {
  if (typeof window === 'undefined' || typeof WebSocket === 'undefined') return;
  if (window.__WF_DISABLE_HMR_CONNECT__) return;
  var proto = (typeof location !== 'undefined' && location.protocol === 'https:') ? 'wss://' : 'ws://';
  var ws = new WebSocket(proto + location.host + BASE + '/__wf_hmr');
  socket = ws;

  ws.onopen = function () {
    var ov = overlay();
    if (typeof ov.kind === 'function' && ov.kind() === 'server') ov.hide();
  };

  ws.onmessage = function (e) {
    var msg;
    try { msg = JSON.parse(e.data); } catch (_) { return; }
    __wfHandleMessage(msg);
  };

  ws.onclose = function () {
    if (socket !== ws) return;
    socket = null;
    setTimeout(function () {
      overlay().show({
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
          await fetch(BASE + '/__wf_ping?t=' + Date.now(), { cache: 'no-store' });
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
`

const bootJS = `
import { reportImportError } from __WF_CLIENT__;
const ENTRY = __WF_ENTRY__;
import(ENTRY).then(function (m) {
  var ov = window.__wfOverlay;
  if (ov && typeof ov.kind === 'function' && ov.kind() === 'error') ov.hide();
  if (m && typeof m.render === 'function') m.render();
}).catch(function (e) {
  console.error('[HMR] initial import failed', e);
  reportImportError(ENTRY, e);
});
`

// clientRuntime 返回带 basePath / buildId 的 runtime 模块源码。
func clientRuntime(base, buildID string) string {
	js := strings.ReplaceAll(clientRuntimeJS, "__WF_BASE__", strconv.Quote(base))
	return strings.ReplaceAll(js, "__WF_BUILD_ID__", strconv.Quote(buildID))
}

// bootScript 把入口 URL 与 runtime URL 固化进外部 module script。
func bootScript(entry, clientURL string) string {
	js := strings.ReplaceAll(bootJS, "__WF_CLIENT__", strconv.Quote(clientURL))
	return strings.ReplaceAll(js, "__WF_ENTRY__", strconv.Quote(entry))
}

// hotBanner 是每个 dev 源模块顶部的注入片段。esbuild 的 Banner 在输出阶段
// 拼接，不参与 import 解析，因此不会把 /@wf/client 记成业务依赖边。
func (d *DevServer) hotBanner(ownerURL string) string {
	return "import { createHotContext as __wfHotCreate } from " + strconv.Quote(d.url("/@wf/client")) + ";\n" +
		"import.meta.hot = __wfHotCreate(" + strconv.Quote(ownerURL) + ");\n"
}

func (d *DevServer) serveClient(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(clientRuntime(d.basePath, d.buildID)))
}

func (d *DevServer) serveBoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(bootScript(d.url(d.entry), d.url("/@wf/client"))))
}

func (d *DevServer) serveOverlay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(devui.ClientScript()))
}
