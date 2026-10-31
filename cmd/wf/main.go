// 极薄 CLI：wf build / wf install —— 纯 Go 调 esbuild + lockfile 安装器。
// 不参与运行期，不违反"单进程/不跑俩 service"。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/plunjoin/webfront"
	"github.com/plunjoin/webfront/compiler"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Println("usage: wf <build|install> [-modules <dir>] [-split] [-prerender routes.json] [-base <path>] [-out <dir>] [-mode <ssr|spa>] [rootDir]")
		if len(os.Args) >= 2 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help") {
			os.Exit(0)
		}
		os.Exit(1)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	modules := fs.String("modules", "node_modules", "依赖目录名（可改为 wf_modules 等）")
	split := fs.Bool("split", true, "客户端产物做代码分割（ESM + vendor chunk）；-split=false 退回单文件 IIFE")
	prerender := fs.String("prerender", "", "预渲染：routes.json 路径（生成 dist/pages/*.html，运行期可零 JS 引擎）")
	base := fs.String("base", "/", "部署前缀（如 /site）；预渲染注入的资源路径会带上它，与 webfront.Config.BasePath 保持一致")
	out := fs.String("out", "", "产物输出目录（默认 <webDir>/dist）")
	mode := fs.String("mode", "ssr", "构建模式：ssr（默认）或 spa（纯客户端 SPA，跳过 SSR）")
	_ = fs.Parse(os.Args[2:])

	root := "."
	if fs.NArg() > 0 {
		root = fs.Arg(0)
	}

	switch cmd {
	case "build":
		webDir := webDirOf(root)
		distDir := filepath.Join(webDir, "dist")
		if *out != "" {
			distDir = *out
			if !filepath.IsAbs(distDir) {
				if cwd, err := os.Getwd(); err == nil {
					distDir = filepath.Join(cwd, *out)
				}
			}
		}
		if err := os.MkdirAll(filepath.Join(distDir, "assets"), 0o755); err != nil {
			fmt.Println("✗ mkdir:", err)
			os.Exit(1)
		}

		clientEntry := compiler.DetectClientEntry(webDir)
		if clientEntry == "" {
			fmt.Println("✗ 找不到客户端入口（src/entry-client.tsx / src/main.tsx …）")
			os.Exit(1)
		}

		// 1) SSR bundle（喂给 goja VM 池；若指定 -mode spa 或无 entry-server 则跳过）
		hasServer := *mode != "spa"
		serverEntry := compiler.DetectServerEntry(webDir)
		if serverEntry == "" {
			hasServer = false
		}

		var ssr []byte
		if hasServer {
			var err error
			ssr, err = compiler.BuildSSRWith(webDir, serverEntry, *modules)
			if err != nil {
				fmt.Println("✗ SSR build error:", err)
				os.Exit(1)
			}
			if err := os.WriteFile(filepath.Join(distDir, "ssr.js"), ssr, 0o644); err != nil {
				fmt.Println("✗ write ssr.js:", err)
				os.Exit(1)
			}
			fmt.Printf("✅ SSR bundle  -> %s/ssr.js (%d bytes)\n", filepath.ToSlash(distDir), len(ssr))
		} else {
			fmt.Println("ℹ️ 未发现 src/entry-server.tsx（或指定了 -mode spa），跳过 SSR 编译，产出纯客户端 SPA")
		}

		// 2) 客户端 hydration/SPA bundle（纯浏览器执行，零 Node）
		assetsDir := filepath.Join(distDir, "assets")
		relAssets, err := filepath.Rel(webDir, assetsDir)
		if err != nil {
			relAssets = assetsDir
		}
		relAssets = filepath.ToSlash(relAssets)

		if *split {
			// Vite 式拆分：入口 + chunks/<name>-<hash>.js
			files, err := compiler.BuildClientSplit(webDir, clientEntry, relAssets, *modules)
			if err != nil {
				fmt.Println("✗ client split build error:", err)
				os.Exit(1)
			}
			absDist, _ := filepath.Abs(distDir)
			for _, f := range files {
				if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
					fmt.Println("✗ mkdir:", err)
					os.Exit(1)
				}
				if err := os.WriteFile(f.Path, f.Contents, 0o644); err != nil {
					fmt.Println("✗ write:", f.Path, err)
					os.Exit(1)
				}
				rel, _ := filepath.Rel(absDist, f.Path)
				fmt.Printf("✅ Client chunk -> %s/%s (%d bytes)\n", filepath.ToSlash(distDir), filepath.ToSlash(rel), len(f.Contents))
			}
		} else {
			client, err := compiler.BuildClientWith(webDir, clientEntry, filepath.Join(relAssets, "client.js"), *modules)
			if err != nil {
				fmt.Println("✗ client build error:", err)
				os.Exit(1)
			}
			if err := os.WriteFile(filepath.Join(distDir, "assets", "client.js"), client, 0o644); err != nil {
				fmt.Println("✗ write client.js:", err)
				os.Exit(1)
			}
			fmt.Printf("✅ Client bundle -> %s/assets/client.js (%d bytes)\n", filepath.ToSlash(distDir), len(client))
		}

		// 3) index.html（SPA 回退 / 生产托管用）
		// 客户端产物统一用 type="module"：ESM 拆分必须是模块脚本，IIFE 在模块里也照样能跑。
		// 资源路径要带上 -base，否则部署在子路径时外壳页面会去根路径取 JS。
		// 入口 import 了 CSS 时（如 Tailwind），把同名的 client.css 一起 link 上。
		prefix := webfront.NormalizeBase(*base)
		cssLink := ""
		if _, err := os.Stat(filepath.Join(distDir, "assets", "client.css")); err == nil {
			cssLink = `<link rel="stylesheet" href="` + prefix + `/assets/client.css">`
		}
		index := `<!doctype html><html><head><meta charset="utf-8"><title>webfront</title>` + cssLink + `</head>` +
			`<body><div id="root"></div><script type="module" src="` + prefix + `/assets/client.js"></script></body></html>`
		if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte(index), 0o644); err != nil {
			fmt.Println("✗ write index.html:", err)
			os.Exit(1)
		}
		fmt.Printf("✅ index.html  -> %s/index.html\n", filepath.ToSlash(distDir))

		// 4) 可选：预渲染（SSG）。运行期就不用 JS 引擎了，二进制从 ~33MB 降到 ~6MB。
		if *prerender != "" {
			if !hasServer {
				fmt.Println("✗ 预渲染需要 SSR bundle，但当前为 SPA 模式或缺少 src/entry-server.tsx")
				os.Exit(1)
			}
			routes, err := compiler.ReadRoutes(*prerender)
			if err != nil {
				fmt.Println("✗ read routes:", err)
				os.Exit(1)
			}
			files, err := compiler.PrerenderWithBase(ssr, routes, distDir, prefix+"/assets/client.js", prefix)
			if err != nil {
				fmt.Println("✗ prerender:", err)
				os.Exit(1)
			}
			for _, f := range files {
				rel, _ := filepath.Rel(distDir, f)
				fmt.Printf("✅ Prerendered -> %s/%s\n", filepath.ToSlash(distDir), filepath.ToSlash(rel))
			}
			fmt.Println("   ↳ 运行期不再需要 JS 引擎：webfront.New 不设 Renderer 即可")
		}
		fmt.Println("🎁 产物已就绪，可被 //go:embed 打进单文件二进制（见 _examples/basic）")
	case "install":
		// 纯 Go lockfile 驱动安装器：读 package-lock.json 物化版本化 store（零 Node）。
		// 与 build 一致：rootDir 指"包含 web/ 的目录"，也可直接指向 web 目录。
		if err := compiler.InstallStore(webDirOf(root), *modules); err != nil {
			fmt.Println("✗ install error:", err)
			os.Exit(1)
		}
		fmt.Println("   ↳ 构建时自动挂载 StoreResolver：同一包的不同版本可共存")
	default:
		fmt.Println("unknown command:", cmd)
		os.Exit(1)
	}
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
