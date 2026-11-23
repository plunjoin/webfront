//go:build dev

package dev

import (
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
)

// runtimeVM 把 ESM runtime 转成 CJS 后在 goja 里执行：
//   - 动态 import 不能降级到 CJS，先换成"未设置 importer"的占位；
//   - window/location/console 由 Go 注入，connect() 因没有 WebSocket 直接返回。
//
// 返回的 vm 已经执行完 runtime，rt 是它的 module.exports。
func runtimeVM(t *testing.T) (*goja.Runtime, *goja.Object) {
	t.Helper()
	js := clientRuntime("", "test-build")
	js = strings.Replace(js, "return import(url);", `return Promise.reject(new Error("importer not set"));`, 1)
	res := api.Transform(js, api.TransformOptions{
		Loader:   api.LoaderJS,
		Format:   api.FormatCommonJS,
		Target:   api.ESNext,
		LogLevel: api.LogLevelSilent,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("runtime 无法转成 CJS: %s", api.FormatMessages(res.Errors, api.FormatMessagesOptions{}))
	}

	vm := goja.New()
	_, _ = vm.RunString(`if (typeof globalThis === 'undefined') { globalThis = this; }`)
	console := vm.NewObject()
	_ = console.Set("warn", func(...goja.Value) {})
	_ = console.Set("error", func(...goja.Value) {})
	_ = console.Set("log", func(...goja.Value) {})
	_ = vm.Set("console", console)
	location := vm.NewObject()
	_ = location.Set("reload", func() {
		_, _ = vm.RunString("globalThis.__reloads = (globalThis.__reloads || 0) + 1")
	})
	_ = location.Set("host", "example.test")
	_ = vm.Set("location", location)
	window := vm.NewObject()
	_ = window.Set("__WF_DISABLE_HMR_CONNECT__", true)
	_ = vm.Set("window", window)

	exports := vm.NewObject()
	module := vm.NewObject()
	_ = module.Set("exports", exports)
	_ = vm.Set("module", module)
	_ = vm.Set("exports", exports)
	_ = vm.Set("require", func(string) goja.Value { return goja.Undefined() })

	if _, err := vm.RunString(string(res.Code)); err != nil {
		t.Fatalf("goja 执行 runtime 失败: %v\n%s", err, res.Code)
	}
	rt := module.Get("exports").(*goja.Object)
	_ = vm.Set("rt", rt)
	return vm, rt
}

// runAsync 执行一段 async 脚本并等待它完成。每次 RunString 返回时 goja 会
// 清空微任务队列；循环几轮足够让嵌套 await 全部落地。
func runAsync(t *testing.T, vm *goja.Runtime, body string) {
	t.Helper()
	_, err := vm.RunString(`globalThis.__wfDone = false; globalThis.__wfErr = '';
(async () => {` + body + `
})().then(function () { globalThis.__wfDone = true; },
       function (e) { globalThis.__wfErr = String((e && e.stack) || e); globalThis.__wfDone = true; });`)
	if err != nil {
		t.Fatalf("脚本无法启动: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _ = vm.RunString(`0`)
		if done := vm.Get("__wfDone"); done != nil && done.ToBoolean() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("goja 异步脚本超时")
		}
		time.Sleep(time.Millisecond)
	}
	if e := vm.Get("__wfErr"); e != nil && !goja.IsUndefined(e) && !goja.IsNull(e) && e.String() != "" {
		t.Fatalf("脚本失败: %s", e.String())
	}
}

// 未知消息类型 / 未知 kind 必须整页刷新，而不是静默忽略。
func TestClientRuntimeUnknownMessagesReload(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
await rt.__wfHandleMessage({ type: 'mystery' });
if (__reloads !== 1) throw new Error('unknown message should reload, reloads=' + __reloads);
await rt.__wfHandleMessage({ type: 'batch', seq: 1, updates: [{ type: 'update', path: '/src/a.js', kind: 'svelte' }] });
if (__reloads !== 2) throw new Error('unknown kind should reload, reloads=' + __reloads);
await rt.__wfHandleMessage({ type: 'batch', seq: 2, updates: [{ type: 'nope', path: '/src/a.js' }] });
if (__reloads !== 3) throw new Error('unknown update type should reload, reloads=' + __reloads);
`)
}

// connected 能力表里声明的自定义 adapter kind 必须被接受，不能误判为未知。
func TestClientRuntimeAdapterKindFromCapabilities(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__accepts = 0;
rt.__wfSetImporter(function (url) {
  var hot = rt.createHotContext(url.split('?')[0]);
  hot.accept(function () { __accepts++; });
  return Promise.resolve({});
});
await rt.__wfHandleMessage({ type: 'connected', protocol: 2,
  capabilities: { batch: true, svelte: true } });
await rt.__wfHandleMessage({ type: 'batch', seq: 1,
  updates: [{ type: 'update', path: '/src/a.svelte', kind: 'svelte' }] });
if (__accepts !== 1) throw new Error('custom adapter update not applied, accepts=' + __accepts);
if ((globalThis.__reloads || 0) !== 0) throw new Error('custom adapter kind should not reload');
`)
}

// 一个批次里的 update 按顺序执行；重复 / 过期的 seq 必须被忽略；
// 重新执行时 dispose 先于 import，data 跨更新保留。
func TestClientRuntimeBatchSeqAndLifecycle(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__log = [];
globalThis.__urls = [];
globalThis.__hotData = null;
globalThis.__disposed = null;
rt.__wfSetImporter(function (url) {
  var base = url.split('?')[0];
  __urls.push(url);
  __log.push('import');
  var hot = rt.createHotContext(base);
  __hotData = hot.data;
  hot.dispose(function (data) { __log.push('dispose'); __disposed = data; });
  hot.accept(function () { __log.push('accept'); });
  return Promise.resolve({ base: base });
});

var update = { type: 'update', path: '/src/a.js', kind: 'js' };
await rt.__wfHandleMessage({ type: 'batch', seq: 1, updates: [update] });
if (__log.join(',') !== 'import,accept') throw new Error('first batch: ' + __log.join(','));
__hotData.count = 7;

// 同一个 seq 重放：不能再次 import。
await rt.__wfHandleMessage({ type: 'batch', seq: 1, updates: [update] });
if (__log.length !== 2) throw new Error('duplicate seq re-imported: ' + __log.join(','));

await rt.__wfHandleMessage({ type: 'batch', seq: 2, updates: [update] });
if (__log.join(',') !== 'import,accept,dispose,import,accept') {
  throw new Error('lifecycle order: ' + __log.join(','));
}
if (__hotData.count !== 7) throw new Error('hot.data 没有跨更新保留: ' + __hotData.count);
if (!__disposed || __disposed.count !== 7) throw new Error('dispose 没拿到旧 data');
if (__urls[1] !== '/src/a.js?t=2') throw new Error('边界重新 import 必须带批次号: ' + __urls[1]);
`)
}

// 重新执行后不再 self-accept 的模块不能继续热替换，必须整页刷新。
func TestClientRuntimeLostAcceptReloads(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__acceptNext = true;
rt.__wfSetImporter(function (url) {
  var hot = rt.createHotContext(url.split('?')[0]);
  if (__acceptNext) hot.accept(function () {});
  return Promise.resolve({});
});
var update = { type: 'update', path: '/src/a.js', kind: 'js' };
await rt.__wfHandleMessage({ type: 'batch', seq: 1, updates: [update] });
__acceptNext = false;
await rt.__wfHandleMessage({ type: 'batch', seq: 2, updates: [update] });
if (__reloads !== 1) throw new Error('lost accept should reload, reloads=' + __reloads);
`)
}

// prune 要在更新前清理记录与 data；full-reload 直接刷新。
func TestClientRuntimePruneAndFullReload(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__pruned = 0;
rt.__wfSetImporter(function (url) {
  var hot = rt.createHotContext(url.split('?')[0]);
  hot.prune(function (data) { __pruned++; });
  hot.accept(function () {});
  return Promise.resolve({});
});
await rt.__wfHandleMessage({ type: 'batch', seq: 1, updates: [{ type: 'update', path: '/src/a.js', kind: 'js' }] });
if (rt.__wfDebugState().records !== 1) throw new Error('record missing');
await rt.__wfHandleMessage({ type: 'batch', seq: 2, updates: [], prunes: [{ path: '/src/a.js', id: 'src/a.js' }] });
if (__pruned !== 1) throw new Error('prune callback not called');
if (rt.__wfDebugState().records !== 0) throw new Error('record not pruned');
await rt.__wfHandleMessage({ type: 'batch', seq: 3, updates: [{ type: 'full-reload', reason: 'test' }] });
if (__reloads !== 1) throw new Error('full-reload not applied');
`)
}

// 生产协议仍兼容单条 update（旧服务端），且非首批 batch 缺 seq 也能处理。
func TestClientRuntimeLegacyUpdateMessage(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__accepts = 0;
globalThis.__urls = [];
rt.__wfSetImporter(function (url) {
  var hot = rt.createHotContext(url.split('?')[0]);
  __urls.push(url);
  hot.accept(function () { __accepts++; });
  return Promise.resolve({});
});
await rt.__wfHandleMessage({ type: 'update', path: '/src/a.js', kind: 'js' });
await rt.__wfHandleMessage({ type: 'update', path: '/src/a.js', kind: 'js' });
if (__accepts !== 2) throw new Error('legacy update not applied, accepts=' + __accepts);
if (__urls[0] === __urls[1]) throw new Error('legacy updates reused the same cache key');
`)
}

// accept 回调拿到的 ctx.mode 必须原样反映服务端的直接/间接判定：
// 依赖冒泡的 update 带 mode=reload，边界自己的模板改动是 auto。
func TestClientRuntimeAcceptContextMode(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__modes = [];
rt.__wfSetImporter(function (url) {
  var hot = rt.createHotContext(url.split('?')[0]);
  hot.accept(function (mod, ctx) { __modes.push(ctx && ctx.mode); });
  return Promise.resolve({});
});
await rt.__wfHandleMessage({ type: 'batch', seq: 1,
  updates: [{ type: 'update', path: '/src/a.js', kind: 'js', mode: 'reload' }] });
await rt.__wfHandleMessage({ type: 'batch', seq: 2,
  updates: [{ type: 'update', path: '/src/a.js', kind: 'js' }] });
if (__modes.join(',') !== 'reload,auto') throw new Error('modes=' + __modes.join(','));
`)
}

// hot.on 监听器属于单次模块执行：重新 import 后旧闭包不能继续收事件。
func TestClientRuntimeListenersArePerRecord(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__events = 0;
rt.__wfSetImporter(function (url) {
  var hot = rt.createHotContext(url.split('?')[0]);
  hot.accept(function () {});
  hot.on('update', function () { __events++; });
  return Promise.resolve({});
});
var update = { type: 'update', path: '/src/a.js', kind: 'js' };
await rt.__wfHandleMessage({ type: 'batch', seq: 1, updates: [update] });
await rt.__wfHandleMessage({ type: 'batch', seq: 2, updates: [update] });
if (__events !== 2) throw new Error('listeners leaked across executions: ' + __events);
`)
}

// 只声明 accept(deps, cb) 的模块在切片 1 保守地整页刷新，而不是留下
// 半新半旧的依赖闭包。
func TestClientRuntimeDepsOnlyAcceptReloads(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__depsCb = 0;
rt.__wfSetImporter(function (url) {
  var hot = rt.createHotContext(url.split('?')[0]);
  hot.accept(['./dep'], function () { __depsCb++; });
  return Promise.resolve({});
});
await rt.__wfHandleMessage({ type: 'batch', seq: 1,
  updates: [{ type: 'update', path: '/src/a.js', kind: 'js' }] });
if (__reloads !== 1) throw new Error('deps-only accept should reload, reloads=' + __reloads);
if (__depsCb !== 0) throw new Error('deps callback should not run on reload');
`)
}

// hot.decline() 的模块即使也注册了 accept，也必须整页刷新。
func TestClientRuntimeDeclineReloads(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__accepts = 0;
rt.__wfSetImporter(function (url) {
  var hot = rt.createHotContext(url.split('?')[0]);
  hot.decline();
  hot.accept(function () { __accepts++; });
  return Promise.resolve({});
});
await rt.__wfHandleMessage({ type: 'batch', seq: 1,
  updates: [{ type: 'update', path: '/src/a.js', kind: 'js' }] });
if (__reloads !== 1) throw new Error('declined module should reload');
if (__accepts !== 0) throw new Error('declined accept callback ran');
`)
}

// 一个边界 import 失败后，同批次剩余 update 不再执行。
func TestClientRuntimeBatchStopsAfterImportFailure(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
globalThis.__imports = [];
rt.__wfSetImporter(function (url) {
  __imports.push(url);
  if (url.indexOf('/src/bad.js') >= 0) return Promise.reject(new Error('boom'));
  var hot = rt.createHotContext(url.split('?')[0]);
  hot.accept(function () {});
  return Promise.resolve({});
});
await rt.__wfHandleMessage({ type: 'batch', seq: 1, updates: [
  { type: 'update', path: '/src/bad.js', kind: 'js' },
  { type: 'update', path: '/src/good.js', kind: 'js' }
] });
if (__imports.length !== 1) throw new Error('batch kept going after failure: ' + __imports.join(','));
`)
}

// 能力协商：connected 必须暴露协议号、buildId 与能力表。
func TestClientRuntimeConnected(t *testing.T) {
	vm, _ := runtimeVM(t)
	runAsync(t, vm, `
await rt.__wfHandleMessage({
  type: 'connected', protocol: 2, buildId: 'test-build',
  capabilities: { batch: true, vue: true }
});
var s = rt.__wfDebugState();
if (s.buildId !== 'test-build') throw new Error('buildId=' + s.buildId);
if (!window.__WF_HMR__ || !window.__WF_HMR__.capabilities.vue) throw new Error('capabilities missing');
`)
}
