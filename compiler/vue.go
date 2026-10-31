package compiler

// vue.go：构建期 .vue 单文件组件编译。
// 思路：esbuild 原生不识别 .vue，这里用 esbuild Plugin 在 OnLoad 阶段拦截 .vue 文件，
// 在纯 Go 的 goja 里调用 @vue/compiler-sfc 的 parse/compileScript/compileTemplate/compileStyle
// 把 SFC 编译成 JS 模块（含 <style> 提取与 CSS 自动注入），再交回 esbuild 继续打包。纯 Go 调度，零 Node。

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	gopath "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
)

type compilerInstance struct {
	compilerProg *goja.Program
	stubProg     *goja.Program
	compilerPool chan *goja.Runtime
}

var (
	compilerMu        sync.Mutex
	compilerInstances = map[string]*compilerInstance{}
)

const nodeStub = `globalThis.global=globalThis;` +
	`if(typeof globalThis.process==='undefined'){globalThis.process={env:{NODE_ENV:'production'},argv:[],platform:'linux',browser:false,version:'v20.0.0',versions:{node:'20.0.0'},cwd:function(){return '/';},nextTick:function(f){f();},on:function(){},emit:function(){}};}` +
	`if(typeof globalThis.Buffer==='undefined'){function B(){};B.from=function(){return {};};B.isBuffer=function(){return false;};B.alloc=function(){return {};};globalThis.Buffer=B;}` +
	`function __wfStub(){var f=function(){return __wfStub();};` +
	`return new Proxy(f,{get:function(t,p){` +
	`if(p==='then')return undefined;` +
	`if(p==='default')return f;` +
	`if(p==='__esModule')return false;` +
	`if(p===Symbol.toPrimitive)return function(){return '';};` +
	`if(p==='toString')return function(){return '';};` +
	`return __wfStub();},apply:function(){return __wfStub();},` +
	`construct:function(){return __wfStub();}});}` +
	// path 不能走 __wfStub：@vue/compiler-sfc 解析 defineProps 的导入类型时要真算目录。
	// 这里用 Proxy 延迟到调用时再取 globalThis.__wfPath（由 Go 侧注入 filepath 实现）。
	`var __wfPathProxy=new Proxy({},{get:function(t,p){` +
	`if(typeof p!=='string')return undefined;` +
	`var target=globalThis.__wfPath;var v=target?target[p]:undefined;` +
	`if(typeof v==='function')return function(){return v.apply(target,arguments);};` +
	`return v;}});` +
	`globalThis.require=function(n){if(n==='path'||n==='node:path')return __wfPathProxy;return __wfStub();};`

// installNodeShims 给 VM 注入 Node 的 path 模块（filepath 语义）。
// @vue/compiler-sfc 解析 defineProps 的导入类型时会调用 path.dirname / join / posix.*，
// 走通用 stub 只会得到空串，最终报 "Failed to resolve import source"。
func installNodeShims(vm *goja.Runtime) {
	strFn := func(fn func(string) string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			return vm.ToValue(fn(call.Argument(0).String()))
		}
	}
	joinFn := func(join func(...string) string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, 0, len(call.Arguments))
			for _, a := range call.Arguments {
				parts = append(parts, a.String())
			}
			return vm.ToValue(join(parts...))
		}
	}
	absFn := func(join func(...string) string, forceSlash bool) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, 0, len(call.Arguments))
			for _, a := range call.Arguments {
				parts = append(parts, a.String())
			}
			p := join(parts...)
			if !forceSlash {
				if abs, err := filepath.Abs(p); err == nil {
					p = abs
				}
			} else if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			return vm.ToValue(p)
		}
	}

	p := vm.NewObject()
	_ = p.Set("dirname", strFn(filepath.Dir))
	_ = p.Set("basename", strFn(filepath.Base))
	_ = p.Set("extname", strFn(filepath.Ext))
	_ = p.Set("join", joinFn(filepath.Join))
	_ = p.Set("resolve", absFn(filepath.Join, false))
	_ = p.Set("isAbsolute", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(filepath.IsAbs(call.Argument(0).String()))
	})
	_ = p.Set("sep", string(filepath.Separator))

	posix := vm.NewObject()
	_ = posix.Set("dirname", strFn(gopath.Dir))
	_ = posix.Set("basename", strFn(gopath.Base))
	_ = posix.Set("extname", strFn(gopath.Ext))
	_ = posix.Set("join", joinFn(gopath.Join))
	_ = posix.Set("resolve", absFn(gopath.Join, true))
	_ = posix.Set("sep", "/")
	_ = p.Set("posix", posix)

	_ = vm.Set("__wfPath", p)
}

func createCompilerInstance(rootDir, modulesDir string) (*compilerInstance, error) {
	abs, _ := filepath.Abs(rootDir)

	md := modulesDir
	if md == "" {
		md = "node_modules"
	}
	realDeps := map[string]bool{}
	sfcPkgJSON := ""
	if d := StorePackageDir(abs, md, "@vue/compiler-sfc"); d != "" {
		sfcPkgJSON = filepath.Join(d, "package.json")
	} else {
		sfcPkgJSON = filepath.Join(resolveModulesDir(abs, md), "@vue/compiler-sfc/package.json")
	}
	if b, err := os.ReadFile(sfcPkgJSON); err == nil {
		var pkg struct {
			Dependencies map[string]string `json:"dependencies"`
		}
		if json.Unmarshal(b, &pkg) == nil {
			for k := range pkg.Dependencies {
				realDeps[k] = true
			}
		}
	}

	opts := api.BuildOptions{
		Bundle:        true,
		Format:        api.FormatIIFE,
		GlobalName:    "compilerSfc",
		Platform:      api.PlatformNode,
		EntryPoints:   []string{"@vue/compiler-sfc"},
		AbsWorkingDir: abs,
		NodePaths:     modulesNodePaths(rootDir, md),
		LogLevel:      api.LogLevelSilent,
		Plugins: append(storePlugins(abs, md), api.Plugin{
			Name: "ext-optional-sfc-deps",
			Setup: func(b api.PluginBuild) {
				b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
					if args.Kind == api.ResolveEntryPoint {
						return api.OnResolveResult{}, nil
					}
					p := args.Path
					if strings.HasPrefix(p, ".") || strings.HasPrefix(p, "/") {
						return api.OnResolveResult{}, nil
					}
					if isNodeBuiltin(p) {
						return api.OnResolveResult{Path: p, External: true}, nil
					}
					base := p
					if strings.HasPrefix(p, "@") {
						parts := strings.SplitN(p, "/", 3)
						if len(parts) >= 2 {
							base = parts[0] + "/" + parts[1]
						}
					} else if i := strings.Index(p, "/"); i >= 0 {
						base = p[:i]
					}
					if realDeps[base] {
						return api.OnResolveResult{}, nil
					}
					return api.OnResolveResult{Path: p, External: true}, nil
				})
			},
		}),
	}
	res := api.Build(opts)
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("bundle @vue/compiler-sfc:\n%s", FormatMessages(res.Errors))
	}
	compilerCode := string(res.OutputFiles[0].Contents)

	sProg, err := goja.Compile("stub.js", nodeStub, false)
	if err != nil {
		return nil, fmt.Errorf("compile node shim: %w", err)
	}
	cProg, err := goja.Compile("compiler-sfc.js", compilerCode, false)
	if err != nil {
		return nil, fmt.Errorf("compile compiler-sfc: %w", err)
	}

	return &compilerInstance{
		stubProg:     sProg,
		compilerProg: cProg,
		compilerPool: make(chan *goja.Runtime, 4),
	}, nil
}

func getCompilerVM(rootDir, modulesDir string) (*goja.Runtime, func(), error) {
	abs, _ := filepath.Abs(rootDir)
	key := filepath.Clean(abs) + "|" + modulesDir

	compilerMu.Lock()
	inst, ok := compilerInstances[key]
	if !ok {
		var err error
		inst, err = createCompilerInstance(rootDir, modulesDir)
		if err != nil {
			compilerMu.Unlock()
			return nil, nil, err // 不缓存失败状态，避免污染其他目录或阻断重试
		}
		compilerInstances[key] = inst
	}
	compilerMu.Unlock()

	var vm *goja.Runtime
	select {
	case vm = <-inst.compilerPool:
	default:
		vm = goja.New()
		installNodeShims(vm)
		if _, err := vm.RunProgram(inst.stubProg); err != nil {
			return nil, nil, fmt.Errorf("node globals shim: %w", err)
		}
		if _, err := vm.RunProgram(inst.compilerProg); err != nil {
			return nil, nil, fmt.Errorf("load compiler in VM: %w", err)
		}
	}

	cleanup := func() {
		if vm == nil {
			return
		}
		_, _ = vm.RunString(`delete globalThis.__d; delete globalThis.__s; delete globalThis.__t; delete globalThis.__css; delete globalThis.__scopeId; delete globalThis.__serr; delete globalThis.__wfStyles;`)
		select {
		case inst.compilerPool <- vm:
		default:
		}
	}
	return vm, cleanup, nil
}

// isNodeBuiltin 判断是否为 Node 内建模块（external 后由运行环境提供；这里仅打包用）。
func isNodeBuiltin(p string) bool {
	if strings.HasPrefix(p, "node:") {
		return true
	}
	switch p {
	case "fs", "path", "util", "vm", "url", "process", "os", "events", "stream",
		"crypto", "assert", "buffer", "module", "querystring", "zlib", "http",
		"https", "net", "tls", "child_process", "string_decoder", "punycode",
		"tty", "readline", "worker_threads", "perf_hooks", "async_hooks",
		"timers", "console", "constants", "sys":
		return true
	}
	return false
}

// SFCResult 是 .vue 单文件组件的编译产物。
type SFCResult struct {
	// Code 是编译出的 ESM 模块源码。注意：脚本声明 lang="ts" 时，产物里仍然
	// 带着类型注解（interface / ref<number>() / setup(__props: any) …），
	// 必须再按 TS 转译一次，才能交给 esbuild / 浏览器 / goja。
	Code string
	// Lang 是 <script> / <script setup> 声明的语言（"" / "js" / "ts"）。
	Lang string
}

// SFCLoader 把 SFC 脚本语言映射成 esbuild 的 Loader。
// @vue/compiler-sfc 只负责"SFC 语法"这一层，并不擦除 TS 类型；
// 产物必须用 LoaderTS 再过一遍，否则会在 `interface`、`x as T` 处直接语法报错。
func SFCLoader(lang string) api.Loader {
	if strings.EqualFold(lang, "ts") {
		return api.LoaderTS
	}
	return api.LoaderJS
}

// sfcStyle 是一个 <style> 块的元信息（内容在预处理后会被替换成纯 CSS）。
type sfcStyle struct {
	Lang    string `json:"lang"`
	Content string `json:"content"`
	Scoped  bool   `json:"scoped"`
	Src     string `json:"src"`
}

// sfcMeta 是 parse 之后从 descriptor 里提取的元信息。
type sfcMeta struct {
	Errors          []string   `json:"errors"`
	ScriptLang      string     `json:"scriptLang"`
	ScriptSetupLang string     `json:"scriptSetupLang"`
	TemplateLang    string     `json:"templateLang"`
	Styles          []sfcStyle `json:"styles"`
	HasScoped       bool       `json:"hasScoped"`
	ScriptSrc       string     `json:"scriptSrc"`
}

// CompileSFC 用 goja 内嵌的 @vue/compiler-sfc 编译单个 .vue 源为 JS 模块。
// 支持 <template>、<script>、<script setup lang="ts">、<style scoped>/<style>，
// 自动提取样式并注入 DOM，复用常驻 VM 池。
//
// 注意：脚本声明 lang="ts" 时，返回值里仍带 TS 语法，调用方必须再按 TS 转译一次；
// 需要语言信息请用 CompileSFCResult + SFCLoader。
func CompileSFC(rootDir, modulesDir, source, filename string) (string, error) {
	res, err := CompileSFCResult(rootDir, modulesDir, source, filename)
	return res.Code, err
}

// CompileSFCResult 同 CompileSFC，但额外返回脚本语言，供调用方选择 esbuild Loader。
//
// 支持的 <style lang>：css / scss / sass / less。预处理器按需从项目依赖目录加载
// （见 preprocess.go），没装对应包时会在用到的那一刻报出明确指引。
//
// 错误契约：**任何编译错误都以 error 返回，不 panic**。goja 把 JS 异常作为
// *goja.Exception 返回，各阶段再包上文件名与阶段名；这里额外兜一层 recover，
// 保证即使 VM 或宿主回调里出了意外，也不会把 dev server / 构建进程带走。
func CompileSFCResult(rootDir, modulesDir, source, filename string) (res SFCResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res = SFCResult{}
			err = fmt.Errorf("%s: SFC 编译内部错误（已恢复，未中断进程）: %v", filename, r)
		}
	}()

	vm, cleanup, err := getCompilerVM(rootDir, modulesDir)
	if err != nil {
		return SFCResult{}, err
	}
	defer cleanup()

	// 统一成正斜杠：@vue/compiler-sfc 的浏览器版内置的是 POSIX 风格 path 垫片，
	// Windows 反斜杠会让 path.dirname 失效，进而解析不了 defineProps 的导入类型。
	filename = filepath.ToSlash(filename)

	hashBytes := sha256.Sum256([]byte(filename))
	scopeId := "data-v-" + fmt.Sprintf("%x", hashBytes)[:8]

	// 1) parse
	if _, err := vm.RunString("globalThis.__d = compilerSfc.parse(" +
		strconv.Quote(source) + ", { filename: " + strconv.Quote(filename) + " });"); err != nil {
		return SFCResult{}, fmt.Errorf("%s: parse: %w", filename, err)
	}

	// 2) 元信息：解析错误、脚本/模板/样式语言、是否 scoped
	metaJS := `
(function () {
  var d = globalThis.__d, desc = d.descriptor;
  function msg(e) {
    var m = (e && e.message) ? e.message : String(e);
    if (e && e.loc && e.loc.start) { m += ' (' + e.loc.start.line + ':' + e.loc.start.column + ')'; }
    return m;
  }
  return JSON.stringify({
    errors: (d.errors || []).map(msg),
    scriptLang: (desc.script && desc.script.lang) || '',
    scriptSetupLang: (desc.scriptSetup && desc.scriptSetup.lang) || '',
    templateLang: (desc.template && desc.template.lang) || '',
    styles: (desc.styles || []).map(function (s) {
      return { lang: s.lang || '', content: s.content || '', scoped: !!s.scoped, src: s.src || '' };
    }),
    hasScoped: (desc.styles || []).some(function (s) { return !!s.scoped; }),
    scriptSrc: (desc.script && desc.script.src) || (desc.scriptSetup && desc.scriptSetup.src) || ''
  });
})()`
	metaVal, err := vm.RunString(metaJS)
	if err != nil {
		return SFCResult{}, fmt.Errorf("%s: 读取 SFC 元信息: %w", filename, err)
	}
	var meta sfcMeta
	if err := json.Unmarshal([]byte(metaVal.String()), &meta); err != nil {
		return SFCResult{}, fmt.Errorf("%s: 解析 SFC 元信息: %w", filename, err)
	}
	if len(meta.Errors) > 0 {
		return SFCResult{}, fmt.Errorf("%s: %s", filename, strings.Join(meta.Errors, "; "))
	}
	if meta.ScriptSrc != "" {
		return SFCResult{}, fmt.Errorf("%s: 暂不支持 <script src=%q>，请把脚本内联进 SFC", filename, meta.ScriptSrc)
	}
	if meta.TemplateLang != "" && !strings.EqualFold(meta.TemplateLang, "html") {
		return SFCResult{}, fmt.Errorf("%s: 暂不支持 <template lang=%q>（纯 Go 编译链没有模板预处理器）", filename, meta.TemplateLang)
	}
	// <style> 只支持内联的 css / scss / sass / less（预处理器见 preprocess.go）。
	for _, st := range meta.Styles {
		if st.Src != "" {
			return SFCResult{}, fmt.Errorf("%s: 暂不支持 <style src=%q>，请把样式内联进 SFC", filename, st.Src)
		}
		if _, ok := preprocessKind(st.Lang); !ok {
			return SFCResult{}, fmt.Errorf("%s: <style lang=%q> 暂不支持（可用：css / scss / sass / less）", filename, st.Lang)
		}
	}
	lang := meta.ScriptSetupLang
	if lang == "" {
		lang = meta.ScriptLang
	}
	if lang != "" && !strings.EqualFold(lang, "js") && !strings.EqualFold(lang, "ts") {
		return SFCResult{}, fmt.Errorf("%s: 暂不支持 <script lang=%q>（只支持 js / ts）", filename, lang)
	}
	isTS := strings.EqualFold(lang, "ts")

	// 3) compileScript
	// genDefaultAs 让 <script setup> 直接产出 `const __script = ...`，不必再用正则
	// 替换 `export default`（那种做法会误伤字符串/注释里的同名字样）。
	// 普通 <script> 没有 setup 时 compileScript 原样返回内容，用官方 rewriteDefault 改写。
	//
	// fs 必须自己提供：`defineProps<ImportedType>()` 需要读被导入的类型文件，
	// 否则 @vue/compiler-sfc 会报 "No fs option provided ... non-Node environment"。
	_ = vm.Set("__wfFileExists", func(call goja.FunctionCall) goja.Value {
		_, statErr := os.Stat(call.Argument(0).String())
		return vm.ToValue(statErr == nil)
	})
	_ = vm.Set("__wfReadFile", func(call goja.FunctionCall) goja.Value {
		b, readErr := os.ReadFile(call.Argument(0).String())
		if readErr != nil {
			return goja.Undefined()
		}
		return vm.ToValue(string(b))
	})
	_ = vm.Set("__wfRealpath", func(call goja.FunctionCall) goja.Value {
		p, absErr := filepath.Abs(call.Argument(0).String())
		if absErr != nil {
			return vm.ToValue(call.Argument(0).String())
		}
		return vm.ToValue(p)
	})

	parserPlugins := "[]"
	if isTS {
		parserPlugins = "['typescript']"
	}
	compileScriptCall := fmt.Sprintf(`
(function () {
  var desc = globalThis.__d.descriptor;
  if (!desc.script && !desc.scriptSetup) {
    globalThis.__s = { content: 'const __script = {};', bindings: {} };
    return;
  }
  var opts = {
    id: %s,
    fs: {
      fileExists: globalThis.__wfFileExists,
      readFile: globalThis.__wfReadFile,
      realpath: globalThis.__wfRealpath
    }
  };
  if (desc.scriptSetup) { opts.genDefaultAs = '__script'; }
  var s = compilerSfc.compileScript(desc, opts);
  if (!desc.scriptSetup) {
    s.content = compilerSfc.rewriteDefault(s.content, '__script', %s);
  }
  globalThis.__s = s;
})();`, strconv.Quote(scopeId), parserPlugins)
	if _, err := vm.RunString(compileScriptCall); err != nil {
		return SFCResult{}, fmt.Errorf("%s: compileScript: %w", filename, err)
	}

	// 4) compileTemplate（仅当有 <template>）
	// isTS 必须透传：compiler-core 只有在 options.isTS 为真时才会给模板表达式
	// 加上 babel 的 typescript 插件，否则 {{ x as string }} 这类写法编译不过。
	tmplCall := fmt.Sprintf(`
(function () {
  var desc = globalThis.__d.descriptor;
  if (!desc.template || !desc.template.content) { globalThis.__t = null; return; }
  var co = {
    scopeId: %t ? %s : undefined,
    bindingMetadata: globalThis.__s.bindings
  };
  if (%t) { co.isTS = true; co.expressionPlugins = ['typescript']; }
  globalThis.__t = compilerSfc.compileTemplate({
    source: desc.template.content,
    filename: %s,
    id: %s,
    scoped: %t,
    compilerOptions: co
  });
})();`, meta.HasScoped, strconv.Quote(scopeId), isTS, strconv.Quote(filename), strconv.Quote(scopeId), meta.HasScoped)
	if _, err := vm.RunString(tmplCall); err != nil {
		return SFCResult{}, fmt.Errorf("%s: compileTemplate: %w", filename, err)
	}
	if tv, err := vm.RunString(`(globalThis.__t && globalThis.__t.errors) ? globalThis.__t.errors.map(function (e) { return (e && e.message) ? e.message : String(e); }).join('; ') : ''`); err == nil && tv != nil && tv.String() != "" {
		return SFCResult{}, fmt.Errorf("%s: %s", filename, tv.String())
	}

	// 5) 预处理 <style lang="scss|sass|less">：先转成纯 CSS，再交给 compileStyle。
	// 分两步走是为了让 compileStyle 只负责 SFC 语义（scoped / :deep() / v-bind()），
	// 预处理器则跑在独立的 VM 里 —— 两者的全局环境互不污染。
	styles := make([]sfcStyle, 0, len(meta.Styles))
	for _, st := range meta.Styles {
		css, perr := preprocessStyle(rootDir, modulesDir, st.Lang, st.Content, filename)
		if perr != nil {
			return SFCResult{}, fmt.Errorf("%s: <style>: %w", filename, perr)
		}
		st.Content = css
		styles = append(styles, st)
	}
	stylesJSON, _ := json.Marshal(styles)
	if _, err := vm.RunString("globalThis.__wfStyles = JSON.parse(" + strconv.Quote(string(stylesJSON)) + ");"); err != nil {
		return SFCResult{}, fmt.Errorf("%s: 注入 <style> 内容: %w", filename, err)
	}

	// 6) compileStyle（scoped 变换 + 汇总）
	compileStylesCall := fmt.Sprintf(`
(function () {
  var list = globalThis.__wfStyles || [], css = '', errs = [];
  for (var i = 0; i < list.length; i++) {
    var s = list[i];
    var res = compilerSfc.compileStyle({ source: s.content, id: %s, scoped: !!s.scoped, filename: %s });
    if (res.code) { css += res.code + '\n'; }
    if (res.errors) {
      for (var j = 0; j < res.errors.length; j++) {
        var e = res.errors[j];
        errs.push((e && e.message) ? e.message : String(e));
      }
    }
  }
  globalThis.__css = css;
  globalThis.__serr = errs.join('; ');
})();`, strconv.Quote(scopeId), strconv.Quote(filename))
	if _, err := vm.RunString(compileStylesCall); err != nil {
		return SFCResult{}, fmt.Errorf("%s: compileStyle: %w", filename, err)
	}
	if sv, err := vm.RunString(`globalThis.__serr || ''`); err == nil && sv != nil && sv.String() != "" {
		return SFCResult{}, fmt.Errorf("%s: <style>: %s", filename, sv.String())
	}

	var tmpl, script, css string
	if tv, err := vm.RunString(`globalThis.__t ? globalThis.__t.code : ''`); err == nil && tv != nil {
		tmpl = tv.String()
	}
	if sv, err := vm.RunString(`globalThis.__s ? globalThis.__s.content : ''`); err == nil && sv != nil {
		script = sv.String()
	}
	if cv, err := vm.RunString(`globalThis.__css || ''`); err == nil && cv != nil {
		css = cv.String()
	}

	// 组合为单模块：模板提供 render，脚本提供组件选项，样式按需注入 DOM
	if tmpl != "" {
		tmpl = strings.Replace(tmpl, "export function render", "function render", 1)
	}
	if strings.TrimSpace(script) == "" {
		script = "const __script = {};"
	}

	var extra strings.Builder
	if tmpl != "" {
		extra.WriteString("__script.render = render;\n")
	}
	if meta.HasScoped {
		extra.WriteString(fmt.Sprintf("__script.__scopeId = %s;\n", strconv.Quote(scopeId)))
	}
	if strings.TrimSpace(css) != "" {
		cssJSON, _ := json.Marshal(css)
		extra.WriteString(fmt.Sprintf(`if (typeof document !== 'undefined') {
  let __el = document.querySelector('style[data-wf-id=%s]');
  if (!__el) {
    __el = document.createElement('style');
    __el.setAttribute('type', 'text/css');
    __el.setAttribute('data-wf-id', %s);
    document.head.appendChild(__el);
  }
  __el.textContent = %s;
} else if (typeof globalThis !== 'undefined' && Array.isArray(globalThis.__WF_STYLES__)) {
  globalThis.__WF_STYLES__.push(%s);
}
`, strconv.Quote(scopeId), strconv.Quote(scopeId), string(cssJSON), string(cssJSON)))
	}
	extra.WriteString("export default __script;\n")

	out := ""
	if tmpl != "" {
		out += tmpl + "\n"
	}
	out += script + "\n" + extra.String()
	return SFCResult{Code: out, Lang: strings.ToLower(lang)}, nil
}

func compileSFC(rootDir, modulesDir, source, filename string) (string, error) {
	return CompileSFC(rootDir, modulesDir, source, filename)
}

// VuePlugin 返回 esbuild 插件：将 .vue 文件在构建期编译为 JS。
func VuePlugin(rootDir, modulesDir string) api.Plugin {
	return api.Plugin{
		Name: "webfront-vue",
		Setup: func(b api.PluginBuild) {
			b.OnLoad(api.OnLoadOptions{Filter: `\.vue$`}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				src, err := os.ReadFile(args.Path)
				if err != nil {
					return api.OnLoadResult{}, err
				}
				res, err := CompileSFCResult(rootDir, modulesDir, string(src), args.Path)
				if err != nil {
					return api.OnLoadResult{}, err
				}
				// lang="ts" 的产物仍含 TS 语法，交给 esbuild 的 TS Loader 擦除类型；
				// 普通 JS 走 LoaderJS，保持原有行为。
				return api.OnLoadResult{Contents: &res.Code, Loader: SFCLoader(res.Lang)}, nil
			})
		},
	}
}
