// Package cli 实现 wf 命令行的全部逻辑。
//
// 它被 cmd/wf 用一个几行的 main 包起来（go tool wf / go run .../cmd/wf 走的都是那条路）。
// 之所以抽成可导入的包，是让第三方应用能把它固定在自己的工具入口里，版本随 go.mod 走：
//
//	// tools/wf/main.go
//	package main
//
//	import (
//		"os"
//
//		"github.com/plunjoin/webfront/cli"
//	)
//
//	func main() { os.Exit(cli.Run(os.Args[1:])) }
//
// 然后 `go run ./tools/wf build -modules wf_modules ./web`。
// 注意：import 本包会把 esbuild / fsnotify / websocket 带进这个二进制的依赖图，
// 所以只应该放在 tools 目录（或用 //go:build tools 隔开），不要放进线上服务。
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/plunjoin/webfront"
	"github.com/plunjoin/webfront/compiler"
	"github.com/plunjoin/webfront/dev"
)

func usage() {
	fmt.Print(`wf — 纯 Go 的前端构建 / 开发工具

usage:
  wf install [flags] [rootDir]
  wf build   [flags] [rootDir]
  wf dev     [flags] [appDir]

日常只需要这两条（在含 web/ 的项目根目录执行）：
  go tool wf install    # 依赖目录自动探测：wf_modules → node_modules
  go tool wf build      # 有 web/routes.json 就自动预渲染

可选参数（都有默认值，通常不用写）：
  -modules auto   -split=true   -prerender auto   -base /   -out <web>/dist   -mode ssr
  -prerender -    关闭自动预渲染
  -mode template  只产 ssr.js（把 React/Vue 当 goja 模板引擎用，不需要客户端入口）

dev: 构建并运行 appDir 里的 main 包，Go 源码变更时自动重新编译、重启进程；
     浏览器在进程重启后自动整页重载。前端（web/src）的热更由应用内挂载的
     dev.DevServer 负责，改 tsx/vue/css 不会重启 Go 进程。

在第三方项目里使用（无需安装，版本随 go.mod 走）：
  go get -tool github.com/plunjoin/webfront/cmd/wf
  go tool wf build
`)
}

// Run 执行一次 wf 命令并返回退出码，不调用 os.Exit。
// 嵌入到别人的 CLI 里时，把返回值交给调用方的退出逻辑即可。
func Run(args []string) int {
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		if len(args) >= 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			return 0
		}
		return 1
	}
	cmd := args[0]
	// ContinueOnError 而不是 ExitOnError：本函数可能跑在别人的进程里，
	// 一个写错的选项不应该把宿主进程直接带走。
	fs := flag.NewFlagSet("wf "+cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = usage
	modules := fs.String("modules", "auto", "依赖目录名（auto=自动探测 wf_modules / node_modules）")
	split := fs.Bool("split", true, "客户端产物做代码分割（ESM + vendor chunk）；-split=false 退回单文件 IIFE")
	prerender := fs.String("prerender", "auto", "预渲染 routes.json（auto=存在 <web>/routes.json 就启用；- 表示关闭）")
	base := fs.String("base", "/", "部署前缀（如 /site）；预渲染注入的资源路径会带上它，与 webfront.Config.BasePath 保持一致")
	out := fs.String("out", "", "产物输出目录（默认 <webDir>/dist）")
	mode := fs.String("mode", "ssr", "构建模式：ssr（默认）、spa（纯客户端 SPA）或 template（只产 ssr.js，给 goja 当模板引擎用）")
	tags := fs.String("tags", "dev", "wf dev：go build -tags（默认 dev；置空则不带 tag）")
	runArgs := fs.String("run", "dev", "wf dev：传给应用的参数（默认 dev，置空则不带参数）")
	watch := fs.String("watch", "", "wf dev：监听根目录（默认取 go.mod 所在目录）")
	ldflags := fs.String("ldflags", "", "wf dev：传给 go build 的 -ldflags")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	// flag 包遇到第一个非选项就停止解析；wf build ./web -mode spa 会让后面的
	// 选项静默失效。这里明确报错，避免"看起来构建了、其实用的是默认值"。
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "✗ 多余的参数: %v\n", fs.Args()[1:])
		fmt.Fprintln(os.Stderr, "  选项要放在目录前面，例如：wf build -mode template ./web")
		return 2
	}
	root := "."
	if fs.NArg() > 0 {
		root = fs.Arg(0)
	}

	switch cmd {
	case "build":
		return runBuild(root, *modules, *split, *prerender, *base, *out, *mode)
	case "install":
		// 纯 Go lockfile 驱动安装器：读 package-lock.json 物化版本化 store（零 Node）。
		// 与 build 一致：rootDir 指"包含 web/ 的目录"，也可直接指向 web 目录。
		webDir := webDirOf(root)
		md := resolveModules(webDir, *modules)
		if err := compiler.InstallStore(webDir, md); err != nil {
			fmt.Println("✗ install error:", err)
			return 1
		}
		fmt.Printf("   ↳ 依赖目录：%s（构建时自动挂载 StoreResolver，同一包的不同版本可共存）\n", md)
		return 0
	case "dev":
		// 进程级热重载：Go 换不掉进程里的机器码，只能重编译 + 换进程。
		// 前端热更仍由应用内挂载的 dev.DevServer 负责，两条链路互不干扰。
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		opt := dev.SuperviseOptions{
			Dir:      root,
			Tags:     *tags,
			RunArgs:  strings.Fields(*runArgs),
			WatchDir: *watch,
		}
		if *ldflags != "" {
			opt.BuildArgs = []string{"-ldflags", *ldflags}
		}
		if err := dev.Supervise(ctx, opt); err != nil {
			fmt.Println("✗ dev:", err)
			return 1
		}
		fmt.Println("👋 dev 已退出")
		return 0
	default:
		fmt.Println("unknown command:", cmd)
		return 1
	}
}

// runBuild 是 `wf build` 的实现，返回退出码。
func runBuild(root, modulesFlag string, split bool, prerenderFlag, base, out, mode string) int {
	if !validMode(mode) {
		fmt.Printf("✗ 未知 -mode %q（可选 ssr / spa / template）\n", mode)
		return 1
	}
	webDir := webDirOf(root)
	modules := resolveModules(webDir, modulesFlag)
	prerender, prerenderAuto := resolvePrerender(webDir, prerenderFlag)
	if !hasInstalledDeps(webDir, modules) {
		if _, err := os.Stat(filepath.Join(webDir, "package-lock.json")); err == nil {
			fmt.Printf("✗ 依赖目录 %s 还不存在或为空\n", modules)
			fmt.Printf("  先执行：wf install%s（或 go tool wf install%s）\n", argSuffix(root), argSuffix(root))
			return 1
		}
	}
	distDir := filepath.Join(webDir, "dist")
	if out != "" {
		distDir = out
		if !filepath.IsAbs(distDir) {
			if cwd, err := os.Getwd(); err == nil {
				distDir = filepath.Join(cwd, out)
			}
		}
	}
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		fmt.Println("✗ mkdir:", err)
		return 1
	}
	// 只清理位于前端工程内部的输出目录。-out 指到 web/ 本身或工程外时，
	// 里面可能是源码 / 手写静态文件，宁可留下旧产物也不删用户文件。
	cleanOutput := canCleanOutput(webDir, distDir)

	// template 模式：只把 SSR 入口打成 ssr.js，给 goja 当模板引擎用。
	// 不要求客户端入口，不产 index.html / assets / pages —— 这条路径没有 hydration。
	if mode == "template" {
		serverEntry := compiler.DetectServerEntry(webDir)
		if serverEntry == "" {
			fmt.Println("✗ template 模式需要 SSR 入口（src/entry-server.tsx / entry-server.vue.tsx …）")
			return 1
		}
		ssr, err := compiler.BuildSSRWith(webDir, serverEntry, modules)
		if err != nil {
			fmt.Println("✗ SSR build error:", err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(distDir, "ssr.js"), ssr, 0o644); err != nil {
			fmt.Println("✗ write ssr.js:", err)
			return 1
		}
		// 上一次 SSR/SPA 构建留下的 index.html / assets / pages 与 template
		// 产物无关，清掉才能保证 dist 的形态和自动探测不被旧产物误导。
		if cleanOutput {
			removeStale(distDir, "index.html", "pages", "assets")
		} else {
			fmt.Println("ℹ️ 产物目录不在前端工程内，跳过过期产物清理（请自行确认目录内容）")
		}
		fmt.Printf("✅ SSR bundle  -> %s/ssr.js (%d bytes)\n", filepath.ToSlash(distDir), len(ssr))
		fmt.Println("   ↳ template 模式：只产 ssr.js；运行期用 jsruntime.New + RenderTemplate，不需要客户端入口")
		return 0
	}

	if err := os.MkdirAll(filepath.Join(distDir, "assets"), 0o755); err != nil {
		fmt.Println("✗ mkdir:", err)
		return 1
	}

	clientEntry := compiler.DetectClientEntry(webDir)
	if clientEntry == "" {
		fmt.Println("✗ 找不到客户端入口（src/entry-client.tsx / src/main.tsx …）")
		return 1
	}

	// 1) SSR bundle（喂给 goja VM 池；若指定 -mode spa 或无 entry-server 则跳过）
	hasServer := mode != "spa"
	serverEntry := compiler.DetectServerEntry(webDir)
	if serverEntry == "" {
		hasServer = false
	}

	prerendered := 0
	var ssr []byte
	if hasServer {
		var err error
		ssr, err = compiler.BuildSSRWith(webDir, serverEntry, modules)
		if err != nil {
			fmt.Println("✗ SSR build error:", err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(distDir, "ssr.js"), ssr, 0o644); err != nil {
			fmt.Println("✗ write ssr.js:", err)
			return 1
		}
		fmt.Printf("✅ SSR bundle  -> %s/ssr.js (%d bytes)\n", filepath.ToSlash(distDir), len(ssr))
	} else {
		fmt.Println("ℹ️ 未发现 src/entry-server.tsx（或指定了 -mode spa），跳过 SSR 编译，产出纯客户端 SPA")
	}

	// 2) 客户端 hydration/SPA bundle（纯浏览器执行，零 Node）
	assetsDir := filepath.Join(distDir, "assets")
	// 固定路径的 client.css 先删掉：本次没产 CSS 时，上次的旧样式会继续被
	// index.html / assemble 引用。只有受管输出目录才动它。
	if cleanOutput {
		_ = os.Remove(filepath.Join(assetsDir, "client.css"))
	}
	relAssets, err := filepath.Rel(webDir, assetsDir)
	if err != nil {
		relAssets = assetsDir
	}
	relAssets = filepath.ToSlash(relAssets)

	if split {
		// Vite 式拆分：入口 + chunks/<name>-<hash>.js
		files, err := compiler.BuildClientSplit(webDir, clientEntry, relAssets, modules)
		if err != nil {
			fmt.Println("✗ client split build error:", err)
			return 1
		}
		absDist, _ := filepath.Abs(distDir)
		for _, f := range files {
			if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
				fmt.Println("✗ mkdir:", err)
				return 1
			}
			if err := os.WriteFile(f.Path, f.Contents, 0o644); err != nil {
				fmt.Println("✗ write:", f.Path, err)
				return 1
			}
			rel, _ := filepath.Rel(absDist, f.Path)
			fmt.Printf("✅ Client chunk -> %s/%s (%d bytes)\n", filepath.ToSlash(distDir), filepath.ToSlash(rel), len(f.Contents))
		}
	} else {
		client, err := compiler.BuildClientWith(webDir, clientEntry, filepath.Join(relAssets, "client.js"), modules)
		if err != nil {
			fmt.Println("✗ client build error:", err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(distDir, "assets", "client.js"), client, 0o644); err != nil {
			fmt.Println("✗ write client.js:", err)
			return 1
		}
		fmt.Printf("✅ Client bundle -> %s/assets/client.js (%d bytes)\n", filepath.ToSlash(distDir), len(client))
	}

	// 3) index.html（SPA 回退 / 生产托管用）
	// 客户端产物统一用 type="module"：ESM 拆分必须是模块脚本，IIFE 在模块里也照样能跑。
	// 资源路径要带上 -base，否则部署在子路径时外壳页面会去根路径取 JS。
	// 入口 import 了 CSS 时（如 Tailwind），把同名的 client.css 一起 link 上。
	prefix := webfront.NormalizeBase(base)
	cssLink := ""
	if _, err := os.Stat(filepath.Join(distDir, "assets", "client.css")); err == nil {
		cssLink = `<link rel="stylesheet" href="` + prefix + `/assets/client.css">`
	}
	index := `<!doctype html><html><head><meta charset="utf-8"><title>webfront</title>` + cssLink + `</head>` +
		`<body><div id="root"></div><script type="module" src="` + prefix + `/assets/client.js"></script></body></html>`
	if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte(index), 0o644); err != nil {
		fmt.Println("✗ write index.html:", err)
		return 1
	}
	fmt.Printf("✅ index.html  -> %s/index.html\n", filepath.ToSlash(distDir))

	// 4) 可选：预渲染（SSG）。运行期就不用 JS 引擎了，二进制从 ~33MB 降到 ~6MB。
	//    -prerender 默认 auto：<web>/routes.json 存在就自动启用，显式传 "-" 可关闭。
	if prerender != "" {
		if !hasServer {
			if prerenderAuto {
				fmt.Println("ℹ️ 发现 routes.json，但当前没有 SSR bundle（-mode spa 或缺 entry-server），跳过预渲染")
			} else {
				fmt.Println("✗ 预渲染需要 SSR bundle，但当前为 SPA 模式或缺少 src/entry-server.tsx")
				return 1
			}
		} else {
			if prerenderAuto {
				fmt.Printf("ℹ️ 发现 %s，自动预渲染\n", filepath.ToSlash(prerender))
			}
			routes, err := compiler.ReadRoutes(prerender)
			if err != nil {
				fmt.Println("✗ read routes:", err)
				return 1
			}
			files, err := compiler.PrerenderWithBase(ssr, routes, distDir, prefix+"/assets/client.js", prefix)
			if err != nil {
				fmt.Println("✗ prerender:", err)
				return 1
			}
			prerendered = len(files)
			for _, f := range files {
				rel, _ := filepath.Rel(distDir, f)
				fmt.Printf("✅ Prerendered -> %s/%s\n", filepath.ToSlash(distDir), filepath.ToSlash(rel))
			}
			fmt.Println("   ↳ 运行期不再需要 JS 引擎：webfront.New 不设 Renderer 即可")
		}
	}
	// 本次没有产出的旧文件必须清掉：运行期自动探测靠产物判断 SSR/SPA，
	// 上一次构建留下的 pages/ 或 ssr.js 会把形态判错。只在构建成功后清理，
	// 构建失败时保留上次可用产物。
	if cleanOutput {
		if !hasServer {
			removeStale(distDir, "ssr.js")
		}
		if pagesStale(prerender, hasServer, prerendered) {
			removeStale(distDir, "pages")
		}
	} else {
		fmt.Println("ℹ️ 产物目录不在前端工程内，跳过过期产物清理（请自行确认目录内容）")
	}
	fmt.Println("🎁 产物已就绪，可被 //go:embed 打进单文件二进制（见 _examples/basic）")
	return 0
}

// canCleanOutput 判断产物目录是否位于前端工程内部（且不是工程根本身）。
// 默认的 <web>/dist 与 <web>/public 这类子目录可以清理；-out web 或工程外目录
// 可能装着源码，跳过清理。
func canCleanOutput(webDir, distDir string) bool {
	w, err := filepath.Abs(webDir)
	if err != nil {
		return false
	}
	d, err := filepath.Abs(distDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(w, d)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pagesStale 判断本次构建后 pages/ 是否应清理：
// 没启用预渲染、没有 SSR 入口、或预渲染实际写了 0 个页面（空 routes.json /
// 全是空 path），旧 pages/*.html 都必须删掉，否则会被自动探测当成静态站。
func pagesStale(prerender string, hasServer bool, prerendered int) bool {
	return prerender == "" || !hasServer || prerendered == 0
}

// removeStale 删除 dist 里本次构建不再产出的旧产物。
// 只碰 wf build 自己管理的固定路径，不动用户放进 dist 的其他文件。
func removeStale(distDir string, names ...string) {
	for _, name := range names {
		p := filepath.Join(distDir, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			fmt.Printf("⚠️ 清理过期产物 %s 失败: %v\n", filepath.ToSlash(p), err)
			continue
		}
		fmt.Printf("🧹 清理过期产物 %s\n", filepath.ToSlash(p))
	}
}

// resolveModules 决定这次用哪个依赖目录。
// 显式传 -modules 就照用；auto（默认）按以下顺序探测：
//  1. 已有版本化 store（<md>/.store/manifest.json）的目录；
//  2. 已存在且有内容的目录；
//  3. 都没有则落到 wf_modules（本项目的推荐布局）。
func resolveModules(webDir, flag string) string {
	if flag != "" && flag != "auto" {
		return flag
	}
	dirs := []string{"wf_modules", "node_modules"}
	for _, md := range dirs {
		if _, err := os.Stat(filepath.Join(webDir, md, ".store", "manifest.json")); err == nil {
			return md
		}
	}
	for _, md := range dirs {
		p := filepath.Join(webDir, md)
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		// 中断 install 会留下只有 .store 的目录（manifest 还没写）。它不算
		// 可用依赖，不要遮住旁边填好的 node_modules。
		entries, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Name() != ".store" {
				return md
			}
		}
	}
	return "wf_modules"
}

// resolvePrerender 决定这次是否预渲染。
// 显式路径照用；"-"（也接受 off/none）关闭；auto（默认）时若
// <web>/routes.json 存在就启用，并用 auto 标记"这是自动发现的"——
// 没 SSR bundle 时只提示跳过，不像显式指定那样报错。
func resolvePrerender(webDir, flag string) (path string, auto bool) {
	switch flag {
	case "-", "off", "none":
		return "", false
	}
	if flag != "" && flag != "auto" {
		return flag, false
	}
	p := filepath.Join(webDir, "routes.json")
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p, true
	}
	return "", false
}

// hasInstalledDeps 只判断依赖目录里有没有东西，不区分 store 与平铺的
// node_modules：有内容就交给 esbuild，让它报真正的解析错误。
func validMode(mode string) bool {
	switch mode {
	case "ssr", "spa", "template":
		return true
	}
	return false
}

func hasInstalledDeps(webDir, modules string) bool {
	dir := modules
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(webDir, dir)
	}
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// argSuffix 在 rootDir 不是当前目录时补一个参数，让提示里的命令能直接照抄。
func argSuffix(root string) string {
	if root == "" || root == "." {
		return ""
	}
	return " " + root
}

// webDirOf 把 rootDir 归一化成"前端工程目录"：
// 传仓库根（含 web/）或直接传 web 目录都能用。
func webDirOf(root string) string {
	w := filepath.Join(root, "web")
	if st, err := os.Stat(w); err == nil && st.IsDir() {
		return w
	}
	return root
}
