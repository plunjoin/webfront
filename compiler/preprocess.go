package compiler

// preprocess.go：<style lang="scss|sass|less"> 的纯 Go 支持。
//
// 思路和 vue.go 完全一致：预处理器本体（dart-sass / less 核心）从项目自己的依赖目录
// 解析，用 esbuild 打成 IIFE 后在 goja 里跑；读文件这一步交给 Go 注入的 __wfRead。
// 于是既不需要 Node，也不需要 fs 模块或 DOM —— 整条链路仍然是一个静态二进制。
//
// 分工：本文件只负责"预处理器 → 纯 CSS"，scoped / :deep() / v-bind() 这些 SFC
// 专属变换仍然交给 @vue/compiler-sfc 的 compileStyle（它拿到的已经是合法 CSS）。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
)

// preprocessStub 预处理器 VM 需要的全局垫片。
// 注意 versions 里**不能**有 node：dart-sass 用 `process.versions.node` 判断"这是不是
// 真的 Node"，一旦判定为真就会走 Node 分支去 require('url')，而 goja 里没有 require。
// 留一个空的 versions 对象既让判断落到浏览器分支，又不至于让别处读 versions 时炸掉。
const preprocessStub = `globalThis.global=globalThis;` +
	`if(typeof globalThis.process==='undefined'){globalThis.process={env:{NODE_ENV:'production'},argv:[],platform:'linux',browser:false,version:'v20.0.0',versions:{},cwd:function(){return '/';},nextTick:function(f){f();},on:function(){},emit:function(){}};}`

// sassDriver 在 goja 里驱动 dart-sass（浏览器构建，无 fs / 无 DOM）。
// 依赖 Go 注入：__wfSass（sass 命名空间）、__wfRead / __wfExists / __wfJoin /
// __wfDirname / __wfModulesDirs。
const sassDriver = `(function () {
  var S = globalThis.__wfSass;

  function WfURL(href) { this.href = String(href); }
  WfURL.prototype.toString = function () { return this.href; };
  WfURL.prototype.toJSON = function () { return this.href; };

  globalThis.URL = WfURL;
  if (typeof globalThis.location === 'undefined') {
    globalThis.location = { href: 'file:///' };
  }

  function toHref(p) {
    p = String(p).replace(/\\/g, '/');
    if (p.indexOf('file://') === 0) { return p; }
    if (p.charAt(0) !== '/') { p = '/' + p; }
    return 'file://' + p;
  }

  function fromHref(u) {
    var s = String(u);
    if (s.indexOf('file://') === 0) { s = s.slice(7); }
    if (/^\/[A-Za-z]:\//.test(s)) { s = s.slice(1); }
    try { s = decodeURIComponent(s); } catch (e) { /* 保持原样 */ }
    return s;
  }

  var EXTS = ['', '.scss', '.sass', '.css'];

  function join(a, b) {
    if (!a) { return b; }
    return globalThis.__wfJoin(a, b);
  }

  // 解析一个 @use / @import 目标：原样 → 补扩展名 → 下划线私有 → 目录 index。
  function tryFile(p) {
    var slash = p.lastIndexOf('/');
    var head = slash >= 0 ? p.slice(0, slash + 1) : '';
    var tail = slash >= 0 ? p.slice(slash + 1) : p;
    var names = [tail, '_' + tail];
    var i, j;
    for (i = 0; i < names.length; i++) {
      for (j = 0; j < EXTS.length; j++) {
        var cand = head + names[i] + EXTS[j];
        if (globalThis.__wfExists(cand)) { return cand; }
      }
    }
    for (j = 1; j < EXTS.length; j++) {
      var idx = p + '/index' + EXTS[j];
      if (globalThis.__wfExists(idx)) { return idx; }
      idx = p + '/_index' + EXTS[j];
      if (globalThis.__wfExists(idx)) { return idx; }
    }
    return null;
  }

  var importer = {
    canonicalize: function (url, ctx) {
      var base = globalThis.__wfBaseDir;
      if (ctx && ctx.containingUrl) {
        var c = fromHref(ctx.containingUrl);
        var i = c.lastIndexOf('/');
        if (i >= 0) { base = c.slice(0, i); }
      }
      var p = String(url);
      var hit = tryFile(join(base, p));
      if (!hit) {
        var dirs = globalThis.__wfModulesDirs || [];
        for (var k = 0; k < dirs.length && !hit; k++) {
          hit = tryFile(join(dirs[k], p));
        }
      }
      if (!hit) { return null; }
      return new WfURL(toHref(hit));
    },
    load: function (canonicalUrl) {
      var p = fromHref(canonicalUrl);
      var src = globalThis.__wfRead(p);
      if (src === null || src === undefined) { return null; }
      var syntax = (p.length > 5 && p.slice(-5) === '.sass') ? 'indented' : 'scss';
      return { contents: src, syntax: syntax };
    }
  };

  globalThis.__wfSassRender = function (src, filename, lang) {
    var dir = globalThis.__wfDirname(filename);
    globalThis.__wfBaseDir = dir;
    globalThis.location.href = toHref(dir);
    try {
      var res = S.compileString(src, {
        syntax: (lang === 'sass') ? 'indented' : 'scss',
        importers: [importer],
        style: 'expanded',
        logger: { warn: function () {}, debug: function () {} }
      });
      return JSON.stringify({ css: res && res.css ? String(res.css) : '' });
    } catch (e) {
      // dart-sass 的 e.message 往往只有一句 "Undefined variable."，具体位置在
      // e.span 里（0 基）。这里补上"行:列"，外层会再补文件名与 <style> 阶段。
      var msg = (e && (e.sassMessage || e.message)) ? (e.sassMessage || e.message) : String(e);
      if (e && e.span && e.span.start && e.span.start.line != null) {
        msg = (e.span.start.line + 1) + ':' + (e.span.start.column + 1) + ': ' + msg;
      }
      return JSON.stringify({ error: msg });
    }
  };
})();`

// lessDriver 在 goja 里驱动 less 核心（无 fs / 无 DOM / 无 Node）。
// 依赖 Go 注入：__wfLessCreate / __wfLessDefaults / __wfLessVersion、
// __wfRead / __wfJoin / __wfBase64 / __wfModulesDirs。
const lessDriver = `(function () {
  var API = globalThis.__wfLessCreate;
  var defaults = globalThis.__wfLessDefaults;

  var environment = {
    encodeBase64: function (s) {
      return globalThis.__wfBase64 ? globalThis.__wfBase64(s) : '';
    },
    mimeLookup: function () { return 'application/octet-stream'; },
    charsetLookup: function () { return 'UTF-8'; }
  };

  var less = API(environment, [], globalThis.__wfLessVersion || '0.0.0');
  less.options = defaults();

  // 文件管理器：@import 的解析规则（前缀 / 扩展名 / paths）全部复用
  // AbstractFileManager，只有"真正读文件"换成 Go 侧的 __wfRead。
  var FileManager = function () {};
  FileManager.prototype = Object.assign(new (less.AbstractFileManager)(), {
    supports: function () { return true; },
    supportsSync: function () { return true; },
    join: function (basePath, laterPath) {
      if (!basePath) { return laterPath; }
      return globalThis.__wfJoin(basePath, laterPath);
    },
    loadFileSync: function (filename, currentDirectory, options, environment) {
      return this.__wfLoad(filename, currentDirectory, options || {});
    },
    loadFile: function (filename, currentDirectory, options, environment, callback) {
      options = options || {};
      var r;
      try {
        r = this.__wfLoad(filename, currentDirectory, options);
      } catch (e) {
        r = { error: e };
      }
      if (options.syncImport) { return r; }
      if (r.error) {
        if (callback) { callback(r.error); }
        return Promise.reject(r.error);
      }
      if (callback) { callback(null, r); }
      return Promise.resolve(r);
    },
    __wfLoad: function (filename, currentDirectory, options) {
      var self = this;
      var isAbs = this.isPathAbsolute(filename);
      var paths = isAbs ? [''] : [currentDirectory || ''];
      if (options.paths) { paths = paths.concat(options.paths); }
      var prefixes = options.prefixes || [''];
      var parts = this.extractUrlParts(filename);
      var tried = [];
      for (var i = 0; i < paths.length; i++) {
        for (var j = 0; j < prefixes.length; j++) {
          var full = parts.rawPath + prefixes[j] + parts.filename;
          if (paths[i]) { full = self.join(paths[i], full); }
          var cand = options.ext ? self.tryAppendExtension(full, options.ext) : full;
          var data = globalThis.__wfRead(cand);
          if (data !== null && data !== undefined) {
            return { contents: data, filename: cand };
          }
          tried.push(cand);
        }
      }
      return {
        error: {
          type: 'File',
          message: "'" + filename + "' wasn't found. Tried - " + tried.join(',')
        }
      };
    }
  });

  // 插件加载器：核心包不带它（Node / 浏览器外壳各自提供），这里给个最小实现，
  // 让 PluginManager 能构造出来；真用到 @plugin 时会得到明确报错而不是崩在构造期。
  var PluginLoader = function (lessApi) {
    this.less = lessApi;
    this.require = function () {
      return function () { return null; };
    };
  };
  PluginLoader.prototype = Object.assign(new (less.AbstractPluginLoader)(), {
    loadPlugin: function (filename, basePath, context, env, fileManager) {
      var prefix = filename.slice(0, 1);
      var explicit = prefix === '.' || prefix === '/' || filename.slice(-3).toLowerCase() === '.js';
      if (!explicit) { context.prefixes = ['less-plugin-', '']; }
      if (context.syncImport) {
        return fileManager.loadFileSync(filename, basePath, context, env);
      }
      return fileManager.loadFile(filename, basePath, context, env);
    },
    loadPluginSync: function (filename, basePath, context, env, fileManager) {
      context.syncImport = true;
      return this.loadPlugin(filename, basePath, context, env, fileManager);
    }
  });

  less.PluginLoader = PluginLoader;
  less.FileManager = FileManager;
  less.environment.addFileManager(new FileManager());

  function fmtErr(e) {
    if (!e) { return 'unknown error'; }
    var loc = '';
    if (e.filename) {
      loc = e.filename;
      if (e.line != null) { loc += ':' + e.line + (e.column != null ? ':' + e.column : ''); }
      loc += ' ';
    }
    return loc + (e.message || String(e));
  }

  // syncImport 让 less 走 loadFileSync，整条链路没有 Promise，可在 goja 里同步取结果。
  globalThis.__wfLessRender = function (src, filename) {
    var out = null, err = null;
    try {
      less.render(src, {
        filename: filename,
        syncImport: true,
        paths: globalThis.__wfModulesDirs || []
      }, function (e, o) {
        err = e; out = o;
      });
    } catch (e) {
      err = e;
    }
    if (err) { return JSON.stringify({ error: fmtErr(err) }); }
    return JSON.stringify({ css: out && out.css ? String(out.css) : '' });
  };
})();`

// cssPreprocessor 是一个按 (项目, 预处理器) 缓存的实例：打包后的 JS + goja VM 池。
type cssPreprocessor struct {
	kind       string // "sass" / "less"
	prog       *goja.Program
	init       string // 打包产物的全局变量 → 驱动脚本约定的入口名
	driver     string
	modulesDir string // 绝对路径，供 @import 的 node_modules 回退使用
	pool       chan *goja.Runtime
}

var (
	preprocessMu    sync.Mutex
	preprocessCache = map[string]*cssPreprocessor{}
)

// preprocessKind 把 <style lang> 映射到预处理器种类。
// ok=false 表示该语言不支持；kind="" 表示无需预处理。
func preprocessKind(lang string) (kind string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "", "css":
		return "", true
	case "scss", "sass":
		return "sass", true
	case "less":
		return "less", true
	}
	return "", false
}

// preprocessStyle 把一段 <style lang="..."> 源码编译成纯 CSS。
// lang 为空或 css 时原样返回；不支持的预处理器返回带指引的错误。
//
// 和 CompileSFCResult 一样，这里保证"错误只会以 error 返回"：
// sass / less 的异常由 JS 侧 catch 成 {error} 再回传，外层再兜一层 recover。
func preprocessStyle(rootDir, modulesDir, lang, source, filename string) (css string, err error) {
	defer func() {
		if r := recover(); r != nil {
			css = ""
			err = fmt.Errorf("<style lang=%q> 预处理器内部错误（已恢复）: %v", lang, r)
		}
	}()

	kind, ok := preprocessKind(lang)
	if !ok {
		return "", fmt.Errorf("<style lang=%q> 暂不支持（可用：css / scss / sass / less）", lang)
	}
	if kind == "" {
		return source, nil
	}
	p, err := getPreprocessor(rootDir, modulesDir, kind)
	if err != nil {
		return "", err
	}
	return p.render(source, filename, strings.ToLower(strings.TrimSpace(lang)))
}

// getPreprocessor 取（必要时创建）某个项目下的预处理器实例。
// 打包 + goja 编译都不便宜（dart-sass 约 6.4MB），所以进程内只做一次。
func getPreprocessor(rootDir, modulesDir, kind string) (*cssPreprocessor, error) {
	abs, _ := filepath.Abs(rootDir)
	md := modulesDir
	if md == "" {
		md = "node_modules"
	}
	key := filepath.Clean(abs) + "|" + md + "|" + kind

	preprocessMu.Lock()
	defer preprocessMu.Unlock()
	if p, ok := preprocessCache[key]; ok {
		return p, nil
	}
	p, err := createPreprocessor(abs, md, kind)
	if err != nil {
		return nil, err // 不缓存失败，装好依赖后重试能立刻生效
	}
	preprocessCache[key] = p
	return p, nil
}

func createPreprocessor(absRoot, md, kind string) (*cssPreprocessor, error) {
	pkgName := kind // "sass" / "less"
	pkgDir := StorePackageDir(absRoot, md, pkgName)
	if pkgDir == "" {
		pkgDir = filepath.Join(resolveModulesDir(absRoot, md), pkgName)
	}
	if st, err := os.Stat(pkgDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("<style lang=%q> 需要 %s：请在项目里 `npm i -D %s` 后执行 `wf install`（已有 node_modules 则直接可用）", kind, pkgName, pkgName)
	}

	opts := api.BuildOptions{
		Bundle:        true,
		Format:        api.FormatIIFE,
		Platform:      api.PlatformBrowser, // 浏览器构建：不碰 fs / path / node:*
		AbsWorkingDir: absRoot,
		NodePaths:     modulesNodePaths(absRoot, md),
		LogLevel:      api.LogLevelSilent,
		Plugins:       storePlugins(absRoot, md),
	}
	init := ""
	switch kind {
	case "sass":
		// 入口用包名，让 esbuild / StoreResolver 按各自的 exports 规则选浏览器构建
		// （sass.default.js），它不依赖 Node 的 fs。
		opts.EntryPoints = []string{"sass"}
		opts.GlobalName = "__wfSass"
	case "less":
		// less 的浏览器构建（dist/less.js）一加载就摸 window，不能直接用；
		// 这里只要核心工厂 + 默认选项，环境（文件管理器 / 插件加载器）由驱动脚本自己补。
		opts.Stdin = &api.StdinOptions{
			Contents: `export { default as create } from "less/lib/less/index.js";
export { default as defaultOptions } from "less/lib/less/default-options.js";`,
			ResolveDir: absRoot,
			Sourcefile: "wf-less-entry.js",
			Loader:     api.LoaderJS,
		}
		opts.GlobalName = "__wfLessCore"
		ver := packageVersion(pkgDir)
		init = "globalThis.__wfLessCreate=__wfLessCore.create;" +
			"globalThis.__wfLessDefaults=__wfLessCore.defaultOptions;" +
			"globalThis.__wfLessVersion=" + strconv.Quote(ver) + ";"
	default:
		return nil, fmt.Errorf("未知的 CSS 预处理器 %q", kind)
	}

	res := api.Build(opts)
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("打包 %s（CSS 预处理器）失败:\n%s", pkgName, FormatMessages(res.Errors))
	}
	prog, err := goja.Compile(pkgName+".js", string(res.OutputFiles[0].Contents), false)
	if err != nil {
		return nil, fmt.Errorf("编译 %s 预处理器失败: %w", pkgName, err)
	}

	driver := sassDriver
	if kind == "less" {
		driver = lessDriver
	}
	return &cssPreprocessor{
		kind:       kind,
		prog:       prog,
		init:       init,
		driver:     driver,
		modulesDir: filepath.ToSlash(resolveModulesDir(absRoot, md)),
		pool:       make(chan *goja.Runtime, 2),
	}, nil
}

// packageVersion 读包 package.json 的 version（less 内部会用它做版本解析）。
func packageVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "0.0.0"
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &pkg) != nil || pkg.Version == "" {
		return "0.0.0"
	}
	return pkg.Version
}

// acquire 从池里取一个已初始化的 VM（没有就现建一个）。
func (p *cssPreprocessor) acquire() (*goja.Runtime, func(), error) {
	var vm *goja.Runtime
	select {
	case vm = <-p.pool:
	default:
		vm = goja.New()
		installPreprocessShims(vm, p.modulesDir)
		if _, err := vm.RunString(preprocessStub); err != nil {
			return nil, nil, fmt.Errorf("预处理器全局垫片: %w", err)
		}
		if _, err := vm.RunProgram(p.prog); err != nil {
			return nil, nil, fmt.Errorf("加载 %s 预处理器: %w", p.kind, err)
		}
		if p.init != "" {
			if _, err := vm.RunString(p.init); err != nil {
				return nil, nil, fmt.Errorf("初始化 %s 预处理器: %w", p.kind, err)
			}
		}
		if _, err := vm.RunString(p.driver); err != nil {
			return nil, nil, fmt.Errorf("安装 %s 驱动: %w", p.kind, err)
		}
	}
	release := func() {
		select {
		case p.pool <- vm:
		default:
		}
	}
	return vm, release, nil
}

// installPreprocessShims 给预处理器 VM 注入最小文件系统与路径工具。
// 路径一律走正斜杠（编译期各处已 ToSlash），进 Go 前再 FromSlash。
func installPreprocessShims(vm *goja.Runtime, modulesDir string) {
	_ = vm.Set("__wfRead", func(call goja.FunctionCall) goja.Value {
		b, err := os.ReadFile(filepath.FromSlash(call.Argument(0).String()))
		if err != nil {
			return goja.Null()
		}
		return vm.ToValue(string(b))
	})
	_ = vm.Set("__wfExists", func(call goja.FunctionCall) goja.Value {
		st, err := os.Stat(filepath.FromSlash(call.Argument(0).String()))
		return vm.ToValue(err == nil && !st.IsDir())
	})
	_ = vm.Set("__wfJoin", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(filepath.ToSlash(filepath.Join(call.Argument(0).String(), call.Argument(1).String())))
	})
	_ = vm.Set("__wfDirname", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(filepath.ToSlash(filepath.Dir(call.Argument(0).String())))
	})
	_ = vm.Set("__wfBase64", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(base64.StdEncoding.EncodeToString([]byte(call.Argument(0).String())))
	})
	dirs := []string{}
	if modulesDir != "" {
		dirs = append(dirs, filepath.ToSlash(modulesDir))
	}
	_ = vm.Set("__wfModulesDirs", dirs)
}

// render 把一段源码编译成 CSS。驱动脚本统一返回 {css} 或 {error} 的 JSON。
func (p *cssPreprocessor) render(source, filename, lang string) (string, error) {
	vm, release, err := p.acquire()
	if err != nil {
		return "", err
	}
	defer release()

	var call string
	if p.kind == "sass" {
		call = "__wfSassRender(" + strconv.Quote(source) + "," + strconv.Quote(filename) + "," + strconv.Quote(lang) + ")"
	} else {
		call = "__wfLessRender(" + strconv.Quote(source) + "," + strconv.Quote(filename) + ")"
	}
	v, err := vm.RunString(call)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p.kind, err)
	}
	var out struct {
		CSS   string `json:"css"`
		Error string `json:"error"`
	}
	if v == nil || v.String() == "" {
		return "", fmt.Errorf("%s: 预处理器没有返回结果", p.kind)
	}
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		return "", fmt.Errorf("%s: 解析预处理器输出: %w", p.kind, err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("%s", out.Error)
	}
	return out.CSS, nil
}
