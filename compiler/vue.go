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
	"path/filepath"
	"regexp"
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
	`globalThis.require=function(){return __wfStub();};`

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
		LogLevel:      api.LogLevelError,
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
		return nil, fmt.Errorf("bundle @vue/compiler-sfc: %+v", res.Errors)
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
		_, _ = vm.RunString(`delete globalThis.__d; delete globalThis.__s; delete globalThis.__t; delete globalThis.__css; delete globalThis.__scopeId; delete globalThis.__hasScoped;`)
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

// CompileSFC 用 goja 内嵌的 @vue/compiler-sfc 编译单个 .vue 源为 JS 模块。
// 支持 <template>、<script>、<style scoped>/<style> 自动提取并注入 DOM，复用常驻 VM 池。
func CompileSFC(rootDir, modulesDir, source, filename string) (string, error) {
	vm, cleanup, err := getCompilerVM(rootDir, modulesDir)
	if err != nil {
		return "", err
	}
	defer cleanup()

	hashBytes := sha256.Sum256([]byte(filename))
	scopeId := "data-v-" + fmt.Sprintf("%x", hashBytes)[:8]

	// 1) parse
	if _, err := vm.RunString("globalThis.__d = compilerSfc.parse(" +
		strconv.Quote(source) + ", { filename: " + strconv.Quote(filename) + " });"); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}

	// 2) check scoped styles
	checkScoped := `
var descriptor = globalThis.__d.descriptor;
var hasScoped = false;
if (descriptor.styles && descriptor.styles.length > 0) {
  for (var i = 0; i < descriptor.styles.length; i++) {
    if (descriptor.styles[i].scoped) { hasScoped = true; break; }
  }
}
globalThis.__hasScoped = hasScoped;
`
	if _, err := vm.RunString(checkScoped); err != nil {
		return "", fmt.Errorf("checkScoped: %w", err)
	}

	// 3) compileScript
	compileScriptCall := fmt.Sprintf(`
if (globalThis.__d.descriptor.script || globalThis.__d.descriptor.scriptSetup) {
  globalThis.__s = compilerSfc.compileScript(globalThis.__d.descriptor, { id: %s });
} else {
  globalThis.__s = { content: 'export default {}', bindings: {} };
}
`, strconv.Quote(scopeId))
	if _, err := vm.RunString(compileScriptCall); err != nil {
		return "", fmt.Errorf("compileScript: %w", err)
	}

	// 4) compileTemplate（仅当有 <template>）
	tmplCall := fmt.Sprintf(`
if (globalThis.__d.descriptor.template && globalThis.__d.descriptor.template.content) {
  globalThis.__t = compilerSfc.compileTemplate({
    source: globalThis.__d.descriptor.template.content,
    filename: %s,
    id: %s,
    scoped: globalThis.__hasScoped,
    compilerOptions: {
      scopeId: globalThis.__hasScoped ? %s : undefined,
      bindingMetadata: globalThis.__s.bindings
    }
  });
} else {
  globalThis.__t = null;
}
`, strconv.Quote(filename), strconv.Quote(scopeId), strconv.Quote(scopeId))
	if _, err := vm.RunString(tmplCall); err != nil {
		return "", fmt.Errorf("compileTemplate: %w", err)
	}

	// 5) compileStyle（提取并编译 <style>）
	compileStylesCall := fmt.Sprintf(`
var css = '';
if (globalThis.__d.descriptor.styles && globalThis.__d.descriptor.styles.length > 0) {
  for (var i = 0; i < globalThis.__d.descriptor.styles.length; i++) {
    var s = globalThis.__d.descriptor.styles[i];
    var res = compilerSfc.compileStyle({
      source: s.content,
      id: %s,
      scoped: !!s.scoped,
      filename: %s
    });
    if (res.code) {
      css += res.code + '\n';
    }
  }
}
globalThis.__css = css;
`, strconv.Quote(scopeId), strconv.Quote(filename))
	if _, err := vm.RunString(compileStylesCall); err != nil {
		return "", fmt.Errorf("compileStyle: %w", err)
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
	hasScopedVal := vm.Get("__hasScoped")
	hasScoped := hasScopedVal != nil && hasScopedVal.ToBoolean()

	// 组合为单模块：模板提供 render，脚本提供组件选项，样式按需注入 DOM
	if tmpl != "" {
		tmpl = strings.Replace(tmpl, "export function render", "function render", 1)
	}
	if strings.Contains(script, "export default") {
		script = regexp.MustCompile(`export\s+default\s*`).ReplaceAllString(script, "const __script = ")
	} else if strings.TrimSpace(script) == "" {
		script = "const __script = {};"
	} else {
		script = script + "\nconst __script = {};"
	}

	var extra strings.Builder
	if tmpl != "" {
		extra.WriteString("__script.render = render;\n")
	}
	if hasScoped {
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
	return out, nil
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
				code, err := CompileSFC(rootDir, modulesDir, string(src), args.Path)
				if err != nil {
					return api.OnLoadResult{}, err
				}
				return api.OnLoadResult{Contents: &code, Loader: api.LoaderJS}, nil
			})
		},
	}
}
