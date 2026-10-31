# webfront

纯 Go 写的前端**编译器 + SSR 运行时**库。开发时热更新、构建时打包、生产时单二进制，**全链路零 Node、零 CGO**。

对标 Vite + Astro 的那一套，但跑在你的 Go 进程里：`import "github.com/plunjoin/webfront"`，把前端挂到已有的 `net/http` / Gin / Echo / Chi 路由上。

```go
// 一行接入：Engine 就是 http.Handler
mux.Handle("/", engine)
```

| 特性 | 说明 |
| --- | --- |
| 零 Node.js | 编译 `.tsx` / `.vue`、打包、SSR、装依赖，全程没有 `node` 进程 |
| 零 CGO | `CGO_ENABLED=0` 可构建，Windows 上 `GOOS=linux go build` 直接出 Linux 二进制 |
| 单二进制 | 前端产物 `//go:embed` 进可执行文件，部署只需拷一个文件 |
| 可选引擎 | 不渲染动态页面就不链接 JS 引擎，二进制 7.0MB；要动态 SSR 才 +9.5MB |
| 框架无关 | 库本身不依赖任何 Web 框架（`go.mod` 只有 4 个直接依赖） |

---

## 三种形态，同一份代码

| 形态 | 构建 | 体积 | 适用 |
| --- | --- | --- | --- |
| **预渲染静态（SSG）** | `go build .` | **6.89 MB** | 内容站、文档、营销页——构建期出 HTML，运行期只发文件 |
| **动态 SSR** | `go build -tags dynamic .` | **15.62 MB** | 需要按请求查库、按用户渲染的页面 |
| **开发服务器** | `wf dev`（或 `go run -tags dev . dev`） | 11.36 MB | 本地开发：esbuild 单文件转译 + WebSocket HMR，改 `.go` 自动重启 |

> 三者共用同一个 `main.go`，靠 build tag 切换 `newEngine()` 的实现。引擎有没有被链进二进制，由你 `import` 哪个包决定。

---

## 安装

```bash
# 获取 webfront 核心运行时与编译包
go get github.com/plunjoin/webfront@v0.1.0

# 可选：安装命令行工具 wf（支持 wf install / wf build 等）
go install github.com/plunjoin/webfront/cmd/wf@v0.1.0
```

在 Go 代码中引用：

```go
import "github.com/plunjoin/webfront"
```

要求 Go 1.27+（见 `go.mod`），无 C 工具链依赖，`CGO_ENABLED=0` 即可构建。

---

## 三步上手

### 1. 前端工程（标准 Vite 结构，只多一个约定）

```
web/
├── package.json          # 依赖清单，只用于生成 lockfile
├── package-lock.json     # wf install 读它
├── routes.json           # 可选：预渲染路由 + Props
└── src/
    ├── entry-server.tsx  # 约定：导出 render(url, props)
    ├── entry-client.tsx  # 浏览器端 hydration
    ├── App.tsx
    └── main.tsx          # 开发态入口
```

**唯一的约定**是 SSR 入口导出 `render(url, props)`：

```tsx
// web/src/entry-server.tsx
import React from 'react'
import { renderToString } from 'react-dom/server.browser'
import App from './App'

export function render(url: string, props: Record<string, any>) {
  const html = renderToString(<App {...props} />)
  const head = `<title>${props?.title || 'webfront'}</title>`
  return { html, head }
}
```

返回 Promise 也行（Vue 的 `renderToString` 就是异步的），库内部统一收口：

```tsx
// web/src/entry-server.vue.tsx
import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from './App.vue'

export async function render(url: string, props: Record<string, any>) {
  const app = createSSRApp(App, props)
  return { html: await renderToString(app), head: '<title>Vue</title>' }
}
```

客户端入口读 `__WF_DATA__` 做真 hydration（不是 `innerHTML` 覆盖）：

```tsx
// web/src/entry-client.tsx
import { hydrateRoot } from 'react-dom/client'
import App from './App'

const el = document.getElementById('__WF_DATA__')
const props = el ? JSON.parse(el.textContent || '{}') : {}
hydrateRoot(document.getElementById('root')!, <App {...props} />)
```

### 2. 构建前端产物（纯 Go，零 Node）

```bash
# 拉依赖：读 package-lock.json，下载 .tgz + 校验 sha512，物化成版本化 store
go run ./cmd/wf install -modules wf_modules ./web

# 打包：SSR bundle + 客户端 ESM(含 vendor chunk) + 预渲染页面
go run ./cmd/wf build -modules wf_modules -prerender web/routes.json ./web
```

产物：

```
web/dist/
├── ssr.js                        # IIFE，挂 globalThis.__WF_APP__ = { render }
├── assets/client.js              # 入口（几百字节）
├── assets/chunks/chunk-*.js      # React/Vue 等 vendor，content hash
├── pages/index.html              # 预渲染页面（-prerender 才有）
└── index.html                    # SPA 回退
```

### 3. 服务端

**静态形态**（不链接 JS 引擎）：

```go
//go:embed all:web/dist
var distFS embed.FS

func main() {
    eng, err := webfront.New(webfront.Config{
        Mode:    webfront.ModeSSR,
        EmbedFS: distFS,
        DistDir: "web/dist",
    })
    if err != nil { panic(err) }

    mux := http.NewServeMux()
    mux.Handle("/assets/", eng.StaticHandler()) // gzip + ETag + 304
    mux.Handle("/", eng)                        // / 与 /about 取 dist/pages/*.html
    http.ListenAndServe(":8080", mux)
}
```

**动态 SSR 形态**（带 goja，按请求渲染）：

```go
//go:embed all:web/dist
var distFS embed.FS

func main() {
    ssr, _ := distFS.ReadFile("web/dist/ssr.js")
    pool, err := jsruntime.New(ssr, 4) // 4 台 VM，每台一次服务一个请求
    if err != nil { panic(err) }

    eng, err := webfront.New(webfront.Config{
        Mode:     webfront.ModeSSR, // 必须显式设，否则 ServeHTTP 走 SPA 分支
        Renderer: pool,             // ← 唯一区别：把渲染器交给引擎
        EmbedFS:  distFS,
        DistDir:  "web/dist",
    })
    if err != nil { panic(err) }
    defer eng.Close()

    mux := http.NewServeMux()
    mux.Handle("/assets/", eng.StaticHandler())
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        _ = eng.Render(w, r, webfront.Page{
            URL:   r.URL.Path,
            Props: map[string]any{"title": "首页", "user": currentUser(r)},
            Head:  webfront.Head{Title: "首页 - 我的站点", Description: "..."},
        })
    })
    http.ListenAndServe(":8080", mux)
}
```

> **`Renderer` 为 nil = 静态模式**，引擎包不会进入编译单元，二进制里就没有 JS 引擎。这是 6.89MB ↔ 15.62MB 的唯一开关。

---

## 核心 API

### `webfront.Config`

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `RootDir` | `"./web"` | 前端工程目录（仅 `compiler.New` 用它构建 SSR，运行期不需要） |
| `BasePath` | `"/"` | 挂载前缀；设 `/site` 后页面查找、静态资源、脚本注入都会先剥/补前缀 |
| `Mode` | — | `ModeSSR`（动态或预渲染 SSR）/ `ModeSPA`（纯客户端 SPA，支持 History 路由 fallback）/ `ModeIslands` |
| `Dev` | `false` | 开发态标记 |
| `EmbedFS` | `nil` | `//go:embed` 进来的产物 |
| `DistDir` | `"dist"` | 产物在 `EmbedFS` 里的子路径，**相对 EmbedFS 根**（如 `"web/dist"`） |
| `ServerEntry` | 自动探测 | `src/entry-server.{tsx,ts,jsx,js}`，以及 Vue 约定的 `entry-server.vue.*`（仅 `compiler.New` 构建 SSR 用） |
| `ModulesDir` | `"node_modules"` | 依赖目录名，可用 `"wf_modules"` |
| `Renderer` | `nil` | 见下；`nil` 即静态模式 |
| `ClientScript` | `"/assets/client.js"` | 注入页面的脚本路径（有 `EmbedFS` 时才默认注入）；设为 `"-"` 关闭 |

> 引擎池大小不在 `Config` 里：只有真正创建 VM 池的那一层才需要它 —— `compiler.New(cfg, vmPoolSize)`（自编译入口）或 `jsruntime.New(bundle, poolSize)`（生产入口，见上例）。

### `webfront.Engine`

| 方法 | 用途 |
| --- | --- |
| `ServeHTTP(w, r)` | 实现 `http.Handler`，直接挂路由（SPA 模式下提供 History 路由 fallback） |
| `Render(w, r, Page) error` | 显式渲染并写响应（动态跑引擎，静态取预渲染页） |
| `RenderHTML(url, Page) (string, error)` | 只返回完整 HTML 字符串，不写响应 |
| `StaticHandler() http.Handler` | 服务 `EmbedFS` 里的客户端产物（gzip 预压缩 + sha1 ETag + 304） |
| `Static() bool` | 当前是否静态模式（无引擎） |
| `Close() error` | 释放渲染器 |

### `webfront.Renderer`（引擎抽象）

```go
type Renderer interface {
    RenderHTML(url string, propsJSON []byte) (html, head string, err error)
    Close() error
}
```

默认实现是 `jsruntime.New(ssrBundle, poolSize)`。内部预编译字节码 Program 并提供 5s 超时中断与坏 VM 自愈机制。想换引擎（比如接一个完整 ES2023 的实现），只需实现这两个方法，上层代码一行不用改。

### `Page` / `Head`（SEO 与元信息）

```go
type Page struct {
    URL       string         // 当前路由
    Props     map[string]any // 序列化进引擎全局 + __WF_DATA__
    Head      Head           // SEO 元信息（Title / Description）
    Component string         // （预留）组件名
}

type Head struct {
    Title       string
    Description string
}
```

- `Props` 走 `encoding/json`，`< > &` 会被转义成 `\u003c` 等，防 `</script>` 注入。
- `assemble` 拼装时会自动检查：若前端 SSR 入口未返回 `<title>` 或 `description` meta，会自动注入 `Page.Head` 的值；若 `EmbedFS` 中包含 `assets/client.css`，也会自动注入 `<link rel="stylesheet">`。

### 自编译入口：`compiler.New`

```go
eng, err := compiler.New(webfront.Config{RootDir: "./web", Mode: webfront.ModeSSR}, 4)
```

启动时现场构建 SSR bundle 并起引擎，适合"单机自编译"的工具型程序。生产部署不建议用它——那会把 esbuild（+4.35MB）一起链进二进制，正确做法是构建期用 `wf build` 出 `ssr.js`，运行期 `//go:embed` 进来。

---

## 框架接入

`Engine` 就是 `http.Handler`，用各框架自带的桥接即可——**库不提供也不依赖任何框架适配器**。

```go
// net/http
mux.Handle("/assets/", eng.StaticHandler())
mux.Handle("/", eng)

// Gin —— 两行接入即可；挂子路径时加一层 r := r.Group("/site") 再剥前缀，或用 Config.BasePath
r.GET("/assets/*filepath", gin.WrapH(eng.StaticHandler()))
r.NoRoute(gin.WrapH(eng))

// Echo
e.GET("/*", echo.WrapHandler(eng))

// Chi
r.Handle("/assets/*", eng.StaticHandler())
r.Handle("/*", eng)
```

> **NoRoute 的状态码坑（库已内置处理）**：Gin 的 `NoRoute` 在调用处理器**之前**就把响应状态预设成 404（`gin.serveError`）。引擎在渲染成功、写 body 之前会显式 `WriteHeader(200)` 把它覆盖回来——`r.NoRoute(gin.WrapH(eng))` 返回的命中页面就是 200，未命中（静态模式查不到预渲染页）仍是 404。自己包一层 NoRoute 处理器时保持这个语义即可。

> 为什么不自带 `WrapGin`？那 4 行代码会让 `go.mod` 硬依赖 Gin，连带 30 多个间接依赖进入每个使用者的模块图——哪怕他用的是 Echo。

---

## CLI

```bash
wf build   [-modules <dir>] [-split] [-prerender routes.json] [-base <path>] [-out <dir>] [-mode <ssr|spa>] [rootDir]
wf install [-modules <dir>] [rootDir]
wf dev     [-tags dev] [-run "dev"] [-watch <dir>] [-ldflags "..."] [appDir]
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `rootDir` | `.` | 包含 `web/` 的目录；直接指向 `web/` 也行 |
| `-modules` | `node_modules` | 依赖目录名；非 `node_modules` 时自动作为 esbuild 的 `NodePaths` |
| `-split` | `true` | 客户端做代码分割（ESM + vendor chunk）；`-split=false` 退回单文件 IIFE |
| `-prerender` | — | `routes.json` 路径，额外产出 `dist/pages/*.html`（运行期可零 JS 引擎） |
| `-base` | `/` | 部署前缀（如 `/site` 或 `/admin`）；资源路径与外壳 HTML 会自动带上 |
| `-out` | `<webDir>/dist` | 产物输出目录（支持多前端子工程输出到独立子目录） |
| `-mode` | `ssr` | 构建模式：`ssr`（默认全功能输出）或 `spa`（纯客户端 SPA，跳过 SSR 编译） |
| `-tags` | `dev` | `wf dev`：`go build -tags`；置空则不带 tag |
| `-run` | `dev` | `wf dev`：传给应用的参数（即 `go run -tags dev . dev` 里的那个 `dev`），置空则不带参数 |
| `-watch` | `go.mod` 所在目录 | `wf dev`：监听根目录；默认整个模块，改子包也会重启 |
| `-ldflags` | — | `wf dev`：透传给 `go build` |

`routes.json`：

```json
[
  { "path": "/", "props": { "title": "首页", "name": "世界" } },
  { "path": "/about", "props": { "title": "关于" } }
]
```

`wf install` 是 lockfile 驱动的纯 Go 安装器：读 `package-lock.json` 的 `resolved` + `integrity`，以 **16 并发**池极速下载 `.tgz`、校验 `sha512`、解压到版本化 store：

```
web/wf_modules/.store/react@18.3.1/
web/wf_modules/.store/@vue+runtime-dom@3.5.13/
web/wf_modules/.store/manifest.json
```

**不做版本解析**——`npm install` 生成 lockfile 那一步仍交给 npm/pnpm（实测纯 Go 从零解析会把 `scheduler ^0.23.2` 错解成 `0.28.0-canary`）。`wf install` 只负责"精确拉取 + 校验 + 解压"，替代 `npm install` 的下载角色。

### `wf dev`：Go 侧也能热更

```bash
cd _examples/basic
wf dev            # 等价于 go run -tags dev . dev，但改 .go 会自动重启
```

Go 进程里的机器码换不掉，所以这里不是"热替换"，而是**重编译 + 换进程**——但把换的过程做得没有存在感：

- **先编译，编译通过才动旧进程**。写错一个分号时页面还活着、继续用上一版代码，编译器报错直接打在终端；改好自动重启。
- **浏览器自动恢复**。进程重启时 WebSocket 断开，客户端不立刻 `reload()`（那会撞上"无法连接"的错误页），而是盖上"后端重启中"的覆盖层，轮询 `/__wf_ping`，等服务回来再整页重载。
- **前端不重启**。`web/src` 下的 tsx / vue / css 仍由应用内的 `dev.DevServer` 走进程内 HMR，改样式、改组件不会触发 Go 重编译。
- **崩了也能自己起来**。子进程 panic 退出后监听不中断，改完代码自动拉起。
- 监听范围是整个模块（`go.mod` 所在目录，含子包），跳过 `node_modules` / `wf_modules` / `dist` / `vendor` / `.git`，也跳过 `*_test.go` 和编辑器临时文件。

> **报错怎么呈现**：编译错误永远是 `error` 返回值，不是 panic —— 一次语法错误不会把 dev server 带走。开发态一律摆到**前台**（Vite 同款错误覆盖层：标题 + `文件:行:列` + 带行号/插入符的代码框 + Esc/点击关闭 + 重新加载按钮），构建态由 esbuild 打印带 code frame 的错误并以非 0 退出。
>
> 覆盖层覆盖四类错误，不用开 DevTools 猜：
> - **前端编译失败**（tsx/vue/css）—— 回读 500 响应体拿原文；嵌套依赖坏了也能定位到真正出错的文件（浏览器只会说一句 `Failed to fetch dynamically imported module`，服务端用 `/__wf_error` 补上）；
> - **浏览器运行期异常** —— `window.onerror` 与 `unhandledrejection` 都接，带 JS 栈；
> - **Go 进程重启** —— 琥珀色"后端重启中"，服务回来自动刷新；
> - **SSR / goja 渲染失败** —— 调用方普遍写 `_ = eng.Render(...)`，所以引擎自己把响应写成错误页（`Config.Dev` 或 `WF_DEV=1` 时），生产态只回干净的 500，不泄露内部栈。

代价是重启窗口内（通常 2–3s）的请求会失败——这是进程级重启绕不开的；需要零中断就得自己做多进程热切，不在本项目的取舍范围内。

---

## 构建标签

| 构建 | 命令 | 链接了什么 |
| --- | --- | --- |
| 静态 | `go build .` | 只有核心包（stdlib） |
| 动态 | `go build -tags dynamic .` | + `jsruntime`（goja，+9.5MB） |
| 开发 | `wf dev`（或 `go run -tags dev . dev`） | + `dev`（esbuild + fsnotify + websocket，+4.5MB） |

`go build ./...` 不会碰示例目录（`_examples` 前缀被 Go 工具链跳过），所以库本身永远能干净编译。

---

## 客户端产物

```
assets/client.js              816 B      ← 业务代码，改它不影响 vendor 缓存
assets/chunks/chunk-XXXX.js  141,787 B   ← React/ReactDOM，content hash
assets/client.css             按 entry 实际 import 的样式而定（如 Tailwind 产物）
```

客户端入口 `import './xxx.css'` 时，esbuild 会把样式一并产出为 `assets/<入口名>.css`（`wf build` 原样落盘）。在服务端渲染或预渲染时，`webfront.Engine` 会自动检测并把 `assets/client.css` 注入到 HTML `<head>` 中，无需开发者在 `entry-server` 里手动拼写 `<link>` 标签。

minify + gzip 两道下来：原始 258KB → 139KB → **45KB**（−82%），二次访问 ETag 命中 **304，0 字节**。

代码分割是 Vite 式的：入口 + vendor chunk 分离，改业务代码时 vendor 的 hash 不变，老用户只重下几百字节的入口。（esbuild 没有 `manualChunks`，这里用"虚拟 vendor 入口 + 两遍构建"复刻。）

---

## Vue 3 支持

`.vue` 单文件组件通过 esbuild 插件在构建期全自动编译：
- **零 Node 纯 Go 编译**：通过预编译的 `*goja.Program` 字节码与常驻 VM 池调用 `@vue/compiler-sfc`，单文件编译仅需数毫秒；
- **`<script setup lang="ts">`**：SFC 编译产物仍带类型语法，插件会自动切到 esbuild 的 TS Loader 再转译一遍；`defineProps<ImportedType>()` 也能通过内建 fs 垫片解析导入的类型文件；
- **`<style>` 与 Scoped 样式**：自动调用 `compileStyle` 提取样式，支持 `scoped` 作用域哈希（`data-v-xxxx`）；
- **`<style lang="scss|sass|less">`**：同样零 Node——预处理器本体（dart-sass / less 核心）从项目自己的依赖目录解析，esbuild 打成 IIFE 后在 goja 里跑，`@use` / `@import` 的文件读取由 Go 注入的回调完成，相对路径、`_partial`、`node_modules` 回退都能解析；
- **动态 SSR 防闪烁（FOUC）**：服务端渲染时将组件样式汇集注入 HTML `<head>`，首屏即带有完整样式；
- **开发态（Dev）实时热更与样式去重**：开发态通过 `data-wf-id` 精确覆盖样式标签，杜绝 HMR 重复堆积；修改子组件自动轻量刷新穿透原生 ESM 缓存。

```tsx
// web/src/entry-server.vue.tsx
import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from './App.vue'

export async function render(url: string, props: Record<string, any>) {
  return { html: await renderToString(createSSRApp(App, props)), head: '<title>Vue SSR</title>' }
}
```

> **预处理器要自己装**：`<style lang="scss">` 需要项目里有 `sass`（`npm i -D sass`），`<style lang="less">` 需要 `less`，然后照常 `wf install`。没装时不会静默降级，而是报 `需要 sass：请在项目里 npm i -D sass 后执行 wf install`。首次用到时有一次约 1 秒的打包 + 字节码编译开销（之后常驻复用），单文件编译 scss ≈ 60ms、less ≈ 5ms。

> `lang="stylus"`、`<template lang="pug">`、`lang="tsx"` 仍不支持（`<style src>` 也不支持，请内联）：遇到时直接报错，而不是产出坏样式/坏模块。

> 开发态（`dev` 服务器）与生产构建均已完整支持 `.vue` 单文件组件编译、`<style>` / `<style scoped>` 提取与实时 HMR 热更新。

---

## 已知限制

| 限制 | 影响 | 缓解 |
| --- | --- | --- |
| goja 不支持 `async function*` / `Intl` | 个别依赖可能跑不动 | SSR 产物统一降级到 ES2017（esbuild `Target`），`ssr.js` 只大 0.5% |
| 无流式 SSR | 不支持 `renderToPipeableStream` + Suspense | 当前全走同步 `renderToString` |
| `ModeIslands` 尚未实现 | 孤岛水合不可用（`ModeSPA` 的文件直出 + index.html fallback 已实现） | 用 `ModeSSR` + 预渲染替代 |
| 预渲染内容构建期固定 | 动态数据进不去 | 动态页面用 `-tags dynamic`，或客户端补 |
| `lang="stylus"` / `lang="pug"` 无对应预处理器 | 这两类 SFC 编译不过 | 先手动编译成 css / html，或换 scss、less |
| `wf dev` 的 Go 热更是"重启进程" | 重启窗口（2–3s）内请求会失败 | 只用于开发；线上靠多副本滚动更新 |
| 错误页里的栈指向打包产物 | SSR 报错显示 `ssr.js:1482:19`，不是 `App.tsx:12` | 生产构建不输出栈；需要精确行号时用 `wf build` 的报错（esbuild 有 code frame） |
| 开发态错误页只在 `Config.Dev` / `WF_DEV` 下出现 | 直接 `go run .` 起服务时看不到可视化错误 | `wf dev` 会自动注入 `WF_DEV=1` |

---

## 仓库结构

```
webfront/
├── webfront.go · static.go   核心运行时（只依赖标准库）
├── compiler/                 构建期：esbuild / SFC / store / SSG
├── jsruntime/                运行期：goja 引擎池
├── dev/                      开发态：转译 + HMR
├── devui/                    开发态错误界面：覆盖层 + 错误页（纯标准库）
├── cmd/wf/                   极薄 CLI
├── _examples/basic/          可运行示例（三形态）
└── ARCHITECTURE.md           设计文档：为什么这么选、怎么验证的
```

**入库的只有源码**（约 160KB）。`dist/`、`wf_modules/`、`*.exe` 都是可再生产物，在 `.gitignore` 里。

---

## 跑一遍示例

```bash
# 1) 构建前端产物
go run ./cmd/wf install -modules wf_modules ./_examples/basic
go run ./cmd/wf build -modules wf_modules \
    -prerender _examples/basic/web/routes.json ./_examples/basic

# 2) 三种形态任选
cd _examples/basic
go run .                       # 静态，7.0MB
go run -tags dynamic .         # 动态 SSR，15.6MB
go run -tags dev . dev         # 开发服务器（改 web/src 即热更）

# 3) 开发模式（推荐）：前端 HMR + 改 .go 自动重启
go run ../cmd/wf dev .         # 或安装后直接 wf dev
```

细节见 [`_examples/basic/README.md`](_examples/basic/README.md)。

---

## 状态

v0.1.0，已实现并端到端验证（Go 1.27 / windows-amd64，`CGO_ENABLED=0`）：

- 三种构建组合（默认 / `dynamic` / `dev`）`go build` + `go vet` 全通过
- 静态 / 动态两种生产构建的依赖图里，esbuild / fsnotify / 任意 Web 框架计数为 **0**
- 同一路由：goja 动态渲染输出 与 构建期预渲染产物 **逐字节一致**
- 客户端 258KB → 45KB（gzip），二次访问 304

### v0.1.0 相对 v0.0.1

- **`<script setup lang="ts">` 真正转译**：SFC 产物按 `lang` 切 esbuild Loader，`defineProps<ImportedType>()` 与模板里的 `x as T` 一并支持
- **CSS 预处理器**：`<style lang="scss|sass|less">` 零 Node 编译（dart-sass / less 核心跑在 goja 里，`@use` / `@import` 的文件读取交给 Go）
- **入口自动探测**：`entry-server.{tsx,ts,jsx,js}` / `entry-client.*` / `main.*`，含 Vue 约定的 `entry-server.vue.*`
- **dev server**：递归监听新建目录、`.vue` 的 TS / scss 实时转译、静态资源路径穿越防护
- **修复**：SSR 首屏样式丢失（模块加载期样式被重置）、goja `Interrupt` 与 VM 归池的竞态、预渲染 CSS 注入与 `-base` 前缀、`wf install` 解压路径穿越
- **测试入库**：`.gitignore` 不再忽略 `*_test.go`

设计与取舍见 [ARCHITECTURE.md](ARCHITECTURE.md)。
