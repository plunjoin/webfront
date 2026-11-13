# 接入指南：把 webfront 放进已有的 Go 项目

> README 回答“它是什么”，这篇回答“我的项目到底该怎么写”。
> 每个方法写不写响应、返回什么状态码、缓存头是什么，见 [API 契约](API.md)。
> 遇到报错直接跳到 [§10 排错](#10-排错按现象查)。

> **本文对应 v0.1.2（工作区，尚未发版）**：`-mode template`、`RenderTemplate*`、`SPAHandler()` 和自动 `Mode` 都是 v0.1.2 新增，v0.1.1 没有。这些改动发布到 main 之前，安装命令仍会拿到 v0.1.1。

## 先看这个：React/Vue 当 Go 的模板引擎

如果你的诉求是“像 `html/template` 一样，用 React/Vue 写模板，Go 里传数据、拿 HTML”——不需要客户端 hydration，也不需要 SPA 路由 / 预渲染——那最短路径是下面三步。

### 1. 前端只写一个模板入口

```tsx
// web/src/entry-server.tsx
import { renderToString } from 'react-dom/server.browser'
import Home from './pages/Home'
import About from './pages/About'

const templates = { Home, About }

export function render(name: string, props: Record<string, any>) {
  const T = templates[name]
  if (!T) throw new Error(`未知模板: ${name}`)
  return {
    html: renderToString(
      <div id="root">
        <T {...props} />
      </div>
    ),
    head: '',
  }
}
```

`name` 就是 Go 侧的模板名（`Home` / `About`），`props` 就是 Go 传来的数据。`entry-client.tsx`、`routes.json`、`index.html` 都不是必需的。

### 2. 只构建 SSR bundle

```bash
go tool wf build -mode template
# 产物：web/dist/ssr.js
```

`-mode template` 不要求客户端入口，只产 `ssr.js`；不生成 `index.html`，也不做预渲染。

### 3. Go 侧：像模板引擎一样调用

```go
//go:embed web/dist/ssr.js
var ssrBundle []byte

func main() {
	pool, err := jsruntime.New(ssrBundle, 4)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// 有 Renderer 自动判为动态 SSR；没有 EmbedFS，所以不会注入客户端脚本。
	eng, err := webfront.New(webfront.Config{Renderer: pool})
	if err != nil {
		log.Fatal(err)
	}
	defer eng.Close()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data := HomeData{Title: "首页", User: currentUser(r)}
		if err := eng.RenderTemplateTo(w, r, "Home", data); err != nil {
			log.Printf("render Home: %v", err) // Render 已经写过 500
		}
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

`RenderTemplateTo` 接受 struct / map / nil，自动 JSON 序列化后交给 `render(name, props)`。只需要 HTML 字符串时用 `RenderTemplate(name, data)`（不写响应）。`Mode` / `ClientScript` 都不用配置。

这条路径的边界：

- **Go 负责路由**：一个 `http.Handler` 对应一个模板名；不需要前端路由。
- **没有 hydration**：页面是纯 HTML；需要交互就由前端自己加载 JS，或补 `entry-client` 走完整 SSR。
- **Props 必须是可 JSON 序列化的普通数据**；struct 字段用 `json` tag 控制命名，不要把 `*http.Request`、数据库连接、函数传进去。
- **`import './x.css'` 不产 CSS**：`-mode template` 只输出 `ssr.js`；样式用 CSS-in-JS、内联样式，或单独构建/托管 CSS。
- **模板名是约定**：`render(name, props)` 的第一个参数就是模板键；你可以用组件名、文件路径或任意字符串，只要 Go 侧和前端注册表一致。

## 0. 先选形态：你的页面要不要 SSR？

| 你要的 | 用哪个形态 | 前端入口 | 运行期要 JS 引擎？ | 典型场景 |
| --- | --- | --- | --- | --- |
| 把 React/Vue 当 Go 模板引擎 | **`-mode template`** | `entry-server.*`（组件注册表） | 是（goja） | Go 控制路由、传数据，输出一次性 HTML |
| 纯前端应用，SEO 不重要 | **SPA** | `src/main.tsx`（或 `entry-client.*`） | 否 | 后台管理、登录后工作台、内部工具 |
| 内容页要 SEO，数据构建时已知 | **预渲染（SSG）** | `entry-server.*` + `routes.json` | 否 | 官网、文档、博客、营销页 |
| 每次请求数据不同（用户、数据库） | **动态 SSR** | `entry-server.*` | 是（goja） | 门户首页、个性化列表、详情页 |

这几种可以混在一个 Go 服务里：比如 `/admin` 用 SPA，`/` 用动态 SSR，某些内部页用 template 模式输出 HTML。见 [§5 多前端](#5-一个项目里多个前端admin--portal)。

> **运行期不用手写 `Mode`**：`New` 会按 `Renderer` → `dist/pages/*.html` → `dist/index.html` 自动判断动态 SSR / 静态 / SPA。只有想钉死行为时才显式设置。同一个前端工程里想同时提供 SSR 和某个 SPA 子路径，也不需要第二个 engine，用 `SPAHandler()`（见 §5.6）。

> **第一次接入建议先跑通 SPA**。它只需要一个 `main.tsx`、一个 `main.go`，没有 SSR 入口约定、没有 JS 引擎、没有预渲染，排错面最小。跑通后再按需加 SSR。

---

## 1. 目录与入口约定

### 1.1 CLI 找一个前端工程的方式

```
your-app/
├── go.mod
├── main.go
└── web/                    ← wf 默认找这个目录
    ├── package.json        ← 依赖声明；wf install 只读 lockfile，dev server 会读它补依赖表
    ├── package-lock.json   ← wf install 唯一读取的文件
    ├── routes.json         ← 可选，预渲染路由
    ├── dist/               ← wf build 产物（不进版本库，保留 .gitkeep）
    │   └── .gitkeep
    └── src/
        ├── main.tsx        ← 客户端入口（SPA / 开发态）
        ├── entry-client.tsx← 可选，优先于 main.*
        └── entry-server.tsx← 可选，SSR / 预渲染入口
```

`wf` 的 `rootDir` 参数指“**包含 `web/` 的目录**”；也可以直接指 `web/` 本身（CLI 发现没有 `web/` 子目录就原样使用）。所以下面两种写法等价：

```bash
go tool wf build            # 在项目根，自动用 ./web
go tool wf build ./web      # 直接指前端工程
```

### 1.2 入口文件名是约定，不是配置

| 入口 | 自动探测顺序（`web/src/` 下） |
| --- | --- |
| 客户端 | `entry-client.tsx` → `.ts` → `.jsx` → `.js` → `main.tsx` → `.ts` → `.jsx` → `.js` |
| SSR | `entry-server.tsx` → `.ts` → `.jsx` → `.js` → `entry-server.vue.tsx` → `.vue.ts` → `.vue.jsx` → `.vue.js` |

- **没有客户端入口**：默认 `wf build`（`ssr` / `spa` 模式）直接报错退出；`-mode template` 不检查客户端入口。
- **没有 SSR 入口**：默认 `ssr` 模式会跳过 SSR，产出纯 SPA（即使不传 `-mode spa`）；`-mode template` 则直接报错（它只产 `ssr.js`）。
- 前端工程目录不必叫 `web`，但那样每次都要显式传路径，且 `//go:embed` 的路径也要自己对齐。

### 1.3 SSR 入口的契约

`entry-server.*` 必须导出一个 `render(url, props)`，返回 `{ html, head }`，或返回它们的 Promise：

```tsx
// web/src/entry-server.tsx
import React from 'react'
import { renderToString } from 'react-dom/server.browser'
import App from './App'

export function render(url: string, props: Record<string, any>) {
  const html = renderToString(
    <div id="root">
      <App url={url} {...props} />
    </div>
  )
  const head = `<title>${props?.title ?? '我的站点'}</title>`
  return { html, head }
}
```

三个容易忽略的点：

1. **`html` 里要包含客户端 hydration 的目标节点**。上面的 `<div id="root">` 不是库加的；`entry-client` 用 `hydrateRoot(document.getElementById('root')!, ...)` 时，两边结构必须一致。
2. **`head` 是原样插入 `<head>` 的 HTML 片段**。库只在片段里**没有** `<title>` / `description` 时，才用 `Page.Head` 补；你自己写了就以你为准。
3. **`props` 可能是 `null`**。`routes.json` 里没写 `props` 时，`json.Marshal(nil map)` 得到 `null`；入口里用 `props?.title` 或先兜底成 `{}`。

### 1.4 客户端入口的契约

```tsx
// web/src/entry-client.tsx
import { hydrateRoot } from 'react-dom/client'
import App from './App'

const data = document.getElementById('__WF_DATA__')
const props = data ? JSON.parse(data.textContent || '{}') : {}

hydrateRoot(
  document.getElementById('root')!,
  <App {...props} />
)
```

`__WF_DATA__` 是库注入的 `<script type="application/json">`，内容就是 `Page.Props`。它和 SSR 时传给 `render` 的是同一份数据。

> SSR 形态务必提供 `entry-client.tsx` 做 `hydrateRoot`。没有它时 `wf build` 会退而用 `main.*` 作为客户端入口；如果 `main.tsx` 写的是 `createRoot`，首屏会被整棵重新挂载，SSR 的 DOM 被丢弃（表现为闪烁、表单状态丢失）。

纯 SPA 不需要 `entry-client`，用 `main.tsx` 直接 `createRoot` 即可：

```tsx
// web/src/main.tsx
import { createRoot } from 'react-dom/client'
import App from './App'
import './index.css'

createRoot(document.getElementById('root')!).render(<App />)
```

---

## 2. 最短路径：纯 SPA

### 2.1 前端文件

`web/package.json`：

```json
{
  "private": true,
  "dependencies": {
    "react": "^18.3.1",
    "react-dom": "^18.3.1"
  }
}
```

`web/src/main.tsx` 见上一节。`App.tsx` 里可以用 `react-router`、`history` 等任意客户端路由——webfront 不关心前端怎么路由。

`package.json` 主要给 npm 生成 lockfile 用；`wf install` 本身只读 lockfile。但 `wf dev` 的 dev server 会读 `dependencies` 来补浏览器的依赖表，所以依赖仍要写全（不要只写在 lockfile 里）。

### 2.2 生成 lockfile 并安装依赖

```bash
cd web
npm install --package-lock-only   # 只生成 package-lock.json，不装 node_modules
cd ..
```

> Node/npm 只在**生成 lockfile** 这一步用到。之后 `wf install` / `wf build` / 运行期都不需要 Node。
> `--package-lock-only` 不会创建 `node_modules`，正合适；如果你已经 `npm install` 过，直接复用现有 lockfile 也行。

```bash
# v0.1.2 尚未发版：先用 main；发版后换成 @v0.1.2
go get github.com/plunjoin/webfront@main
go get -tool github.com/plunjoin/webfront/cmd/wf

go tool wf install          # 读 web/package-lock.json，物化到 web/wf_modules/.store/
go tool wf build -mode spa  # 产物：web/dist/index.html + assets/
```

`go tool wf install` 在项目根执行即可；它自动发现 `web/`。依赖目录默认 `wf_modules`；如果项目里已经有 `node_modules/`，会自动沿用它，不需要 `-modules`。

### 2.3 Go 侧：一个文件

```go
// main.go
package main

import (
	"embed"
	"log"
	"net/http"

	"github.com/plunjoin/webfront"
)

//go:embed all:web/dist
var distFS embed.FS

func main() {
	// 不写 Mode：dist 里没有 pages/、只有 index.html → 自动识别为 SPA。
	eng, err := webfront.New(webfront.Config{
		EmbedFS: distFS,
		DistDir: "web/dist", // 相对于 distFS 的根，不是相对于磁盘
	})
	if err != nil {
		log.Fatal(err)
	}

	// 自动识别为 SPA 时，ServeHTTP 同时做两件事：
	//   1. 命中 dist 里的真实文件就直出（带 ETag / gzip / 304）；
	//   2. 没命中就回 index.html，交给前端路由做 history fallback。
	http.Handle("/", eng)

	log.Println("http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

**不要**在 SPA 模式下调 `eng.Render` 做 fallback：`Render` 只服务预渲染页面，不会回 `index.html`。SPA 的 fallback 在 `ServeHTTP`（或子路径的 `SPAHandler`）里。这是接入时最常见的一个错。

### 2.4 `dist` 要占位，`embed` 才能过编译

`//go:embed` 在**编译期**展开。`web/dist` 被 `.gitignore` 忽略后，新克隆的仓库里没有这个目录，`go build` 会报：

```
pattern all:web/dist: no matching files found
```

解决方法是提交一个占位文件，并让 `all:` 把它算进去：

```bash
mkdir -p web/dist
touch web/dist/.gitkeep
```

`.gitignore` 用“忽略目录内容、但放行占位文件”的写法：

```gitignore
web/dist/*
!web/dist/.gitkeep
```

必须用 `all:` 前缀（`//go:embed all:web/dist`），否则以 `.` 开头的 `.gitkeep` 不会被匹配。仓库根 `.gitignore` 已经内置了这套规则，照抄即可。

### 2.5 挂到子路径（例如 `/admin`）

构建时和运行时必须用同一个前缀：

```bash
go tool wf build -mode spa -base /admin
```

```go
eng, _ := webfront.New(webfront.Config{
	BasePath: "/admin", // dist 无 pages/、有 index.html → 自动 SPA
	EmbedFS:  distFS,
	DistDir:  "web/dist",
})
mux.Handle("/admin/", eng)
```

规则：

- 前端代码里的路由始终写 `/users`，不要写 `/admin/users`；引擎会把请求路径的前缀剥掉再找文件 / 交给前端。
- 构建产物里的 `<script src>`、CSS 链接会自动带上 `-base`。
- `-base` 和 `Config.BasePath` 不一致时，典型现象是页面能出来但 JS/CSS 404。

---

## 3. 预渲染（SSG）：构建期出 HTML，运行期零引擎

### 3.1 前端多一个 `routes.json`

```json
[
  { "path": "/", "props": { "title": "首页", "name": "世界" } },
  { "path": "/about", "props": { "title": "关于" } },
  { "path": "/docs/intro", "props": {} }
]
```

**字段名是 `path`，不是 `url`。** 写成 `url` 不会报错：`encoding/json` 忽略未知字段，`path` 取到空串后被跳过，最后 `dist/pages/` 里一个页面都没有。这是最隐蔽的坑之一。

`path` 写**前端视角**的路径：挂在 `/site` 下也写 `/about`，不要写 `/site/about`。`-base` 只影响产物里的资源 URL，不会给路由加前缀。

### 3.2 构建

```bash
go tool wf install
go tool wf build
```

只要 `web/routes.json` 存在，`wf build` 会自动预渲染，不需要额外参数。产物：

```
web/dist/
├── ssr.js                 # 给构建期预渲染用，运行期不需要
├── assets/client.js       # 客户端 hydration
├── assets/chunks/*.js
├── pages/index.html       # /
├── pages/about.html       # /about
├── pages/docs/intro.html  # /docs/intro
└── index.html             # SPA 外壳（这个形态下用不到）
```

### 3.3 Go 侧：不设 `Renderer`

```go
// 不写 Mode：dist 里有 pages/ → 自动识别为预渲染/静态。
eng, err := webfront.New(webfront.Config{
	EmbedFS: distFS,
	DistDir: "web/dist",
})
if err != nil {
	log.Fatal(err)
}

mux := http.NewServeMux()
mux.Handle("/assets/", eng.StaticHandler()) // gzip + ETag + 304
mux.Handle("/", eng)                        // / → pages/index.html，/about → pages/about.html
```

`Renderer == nil` + `pages/*.html` 就是静态模式：`webfront` 包不会 import `jsruntime`，二进制里没有 goja。

### 3.4 静态模式的 404 语义

预渲染形态没有“未知路径回 SPA 外壳”这回事：

- `/about` → 找 `pages/about.html`，再找 `pages/about/index.html`；
- `/contact` 不在 `routes.json` 里 → **404**；
- 根路径 `/` → `pages/index.html`。

要覆盖的动态路径，要么列进 `routes.json`，要么让客户端接管（切 SPA），要么在路由层把该子路径显式挂到 `eng.SPAHandler()`——库不会自动把预渲染 404 变成 SPA 外壳。自己在 Go 里做响应缓冲 fallback 不推荐，见 [API 契约 §3.2](API.md)。

> 预渲染页面在运行期是固定内容。用户相关数据只能客户端 hydration 后请求 `/api` 补，或者改用动态 SSR。

---

## 4. 动态 SSR：每个请求跑一次 `render`

### 4.1 和预渲染相同的入口，多一个渲染器

```go
//go:embed all:web/dist
var distFS embed.FS

func main() {
	ssr, err := distFS.ReadFile("web/dist/ssr.js")
	if err != nil {
		log.Fatal("先执行 wf build 生成 web/dist/ssr.js: ", err)
	}

	// 4 台 VM：最多 4 个请求同时渲染，其余在池里排队。
	// 单次渲染默认 5s 超时；超时/死循环的 VM 会被销毁重建。
	pool, err := jsruntime.New(ssr, 4)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// 有 Renderer → 自动识别为动态 SSR，Mode 可省略。
	eng, err := webfront.New(webfront.Config{
		Renderer: pool,
		EmbedFS:  distFS,
		DistDir:  "web/dist",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer eng.Close()

	mux := http.NewServeMux()
	mux.Handle("/assets/", eng.StaticHandler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		page, err := loadPage(r) // 你的数据库 / 鉴权逻辑
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Render 成功写 200 + HTML；失败会自己写 500（生产）或错误页（开发），
		// 并返回 err。调用方只记日志，不要再写响应。
		if err := eng.Render(w, r, page); err != nil {
			log.Printf("render %s: %v", r.URL.Path, err)
		}
	})

	log.Fatal(http.ListenAndServe(":8080", mux))
}
```

`loadPage` 返回的 `Page`：

```go
webfront.Page{
	URL:   r.URL.Path, // Render 会自动剥掉 BasePath
	Props: map[string]any{"title": "首页", "user": currentUser(r)},
	Head:  webfront.Head{Title: "首页 - 我的站点", Description: "..."},
}
```

- `Props` 必须能被 `encoding/json` 序列化（`chan`、`func`、`NaN` 会出错）。
- `Props` 同时进入 `__WF_DATA__`，客户端 hydration 拿到的是同一份数据。
- 不想用默认的 500 页面、想自己渲染一个 fallback？用 `RenderHTML`，它不写响应：

```go
html, err := eng.RenderHTML(frontendPath, page)
if err != nil {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, "<h1>页面暂时不可用</h1>")
	return
}
w.Header().Set("Content-Type", "text/html; charset=utf-8")
w.WriteHeader(http.StatusOK)
_, _ = io.WriteString(w, html)
```

> `RenderHTML` **不剥 BasePath**。挂在 `/site` 下时，传 `strings.TrimPrefix(r.URL.Path, "/site")`，或直接用前端视角的路由（如 `/about`）。

### 4.2 `ServeHTTP` 什么时候够用？

`eng.ServeHTTP` 会用一份默认 Props 渲染，不查库、不鉴权：

```go
Props: map[string]any{"title": "webfront SSR", "path": stripBase(r.URL.Path)}
```

只适合“页面内容与请求无关”的 SSR。一旦要按用户/路由查数据，就必须自己写 handler 调 `eng.Render`（如上一节）。

### 4.3 池大小怎么定

- 一次渲染占一台 VM；`size = N` 意味着同时最多 N 个 SSR 请求，第 N+1 个在 channel 上等。
- VM 里常驻着整个 `ssr.js` bundle，开太大浪费内存。
- 从 `GOMAXPROCS` 或压测后的并发峰值起步（常见 2–8），不要按 QPS 开。
- 默认超时 5s，可用 `jsruntime.NewWithTimeout(bundle, size, d)` 或 `pool.SetTimeout(d)` 调整。

---

## 5. 一个项目里多个前端（admin + portal）

真实项目常常是：`/admin` 是 Vue/React 后台（纯 SPA），`/` 是 React 门户（动态 SSR）。这是两个独立前端：**每个前端要有自己的构建产物和 `EmbedFS`**。

> 先分清两种“混合”：
>
> - **多个独立前端**（admin 和 portal 是两套 `package.json` / 两个 `dist`）：各自一个 Engine，见本节。
> - **同一个前端里的 SSR + SPA 子路径**：一份产物、一个 Engine，挂 `SPAHandler()` 即可，见 [§5.6](#56-同一份产物ssr-为主--spa-子路径)。

### 5.1 目录

```
your-app/
├── main.go
└── web/
    ├── embed.go              ← 建议把 embed 变量集中在这里（package webui）
    ├── admin/
    │   ├── package.json
    │   ├── package-lock.json
    │   ├── src/main.tsx
    │   ├── wf_modules/       ← 各自独立，最稳
    │   └── dist/.gitkeep
    └── portal/
        ├── package.json
        ├── package-lock.json
        ├── routes.json
        ├── src/entry-server.tsx
        ├── src/entry-client.tsx
        ├── wf_modules/
        └── dist/.gitkeep
```

> **依赖目录建议每个前端各一个**。`wf install` 写的 `<modules>/.store/manifest.json` 是**每次覆盖**的，不是多项目合并的。两个前端共用一个 `wf_modules` 时，后一次 install 会覆盖前一次的 manifest；包文件还在，但 vendor 分包和根依赖判断会按最后一次的清单来，容易出现“明明装过却解析不到”。共享目录只适合**一个 lockfile 覆盖所有前端依赖**的情况（把依赖合并到一个根 `package.json`，只 install 一次）。

### 5.2 构建

```bash
cd web

go tool wf install ./admin
go tool wf install ./portal

# admin：SPA + /admin 前缀
go tool wf build -mode spa -base /admin ./admin

# portal：动态 SSR，挂根路径
go tool wf build ./portal
```

产物分别是 `web/admin/dist` 和 `web/portal/dist`。

### 5.3 `web/embed.go`

```go
package webui

import "embed"

//go:embed all:admin/dist
var AdminFS embed.FS

//go:embed all:portal/dist
var PortalFS embed.FS
```

`embed.go` 在 `web/` 目录里，所以模式写 `admin/dist`、`portal/dist`（相对于 `web/`）。**`//go:embed` 的模式相对于放 embed 语句的包目录，且不能出现 `..`**；因此把 embed 变量放在产物目录的祖先目录里最省事。

### 5.4 两个 Handler

```go
package webui

import (
	"net/http"

	"github.com/plunjoin/webfront"
	"github.com/plunjoin/webfront/jsruntime"
)

// AdminHandler：纯 SPA，挂在 /admin/。
func AdminHandler() (http.Handler, error) {
	return webfront.New(webfront.Config{
		BasePath: "/admin", // 无 pages/、有 index.html → 自动 SPA
		EmbedFS:  AdminFS,
		DistDir:  "admin/dist",
	})
}

// PortalHandler：动态 SSR，挂在 /。
func PortalHandler(loadPage func(*http.Request) webfront.Page) (http.Handler, error) {
	ssr, err := PortalFS.ReadFile("portal/dist/ssr.js")
	if err != nil {
		return nil, err
	}
	pool, err := jsruntime.New(ssr, 4)
	if err != nil {
		return nil, err
	}
	eng, err := webfront.New(webfront.Config{
		Renderer: pool, // 有 Renderer → 自动动态 SSR
		EmbedFS:  PortalFS,
		DistDir:  "portal/dist",
	})
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/assets/", eng.StaticHandler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := eng.Render(w, r, loadPage(r)); err != nil {
			// Render 已经写过 500，这里只记日志。
			_ = err
		}
	})
	return mux, nil
}
```

挂载：

```go
admin, _ := webui.AdminHandler()
portal, _ := webui.PortalHandler(loadPage)

mux.Handle("/admin/", admin)
mux.Handle("/", portal)
```

`net/http.ServeMux` 按最长前缀匹配，所以 `/admin/...` 不会落到 portal；注册顺序不影响。

> 上面创建的 `pool` / `eng` 生命周期与进程一致，进程退出时由操作系统回收。如果你的应用有优雅关闭流程，建议把创建逻辑改成返回 `(http.Handler, func(), error)`，退出时显式 `eng.Close()` 释放 VM 池。

> 如果 portal 也挂在子路径（比如 `/portal`），构建时加 `-base /portal`，引擎加 `BasePath: "/portal"`。两个前端的前缀不能互相包含，否则最长匹配会把请求分错。

### 5.5 静态文件与 API 的边界

- `/admin/`、`/` 由各自的 Engine 接管；
- `/api/` 注册在同一个 mux 上，优先级高于 `/`（更长前缀），不会被前端引擎吃掉；
- 鉴权中间件包在 Engine 外层，按请求决定放行或返回 401；
- 不要把业务查询写进 `webui` 包：Handler 只负责把 `Page.Props` 交给引擎，查询逻辑留在你的 service 层（`loadPage` 回调就是为此留的）。

### 5.6 同一份产物：SSR 为主 + SPA 子路径

如果 `/app/*` 只是同一个前端工程里的纯客户端路由，不需要第二个前端、第二个 `EmbedFS`：

```go
eng, _ := webfront.New(webfront.Config{
	Renderer: pool, // 默认走 SSR
	EmbedFS:  distFS,
	DistDir:  "web/dist",
})

mux := http.NewServeMux()
mux.Handle("/assets/", eng.StaticHandler())
mux.Handle("/app/", eng.SPAHandler()) // 命中文件直出，否则回 index.html
mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	_ = eng.Render(w, r, loadPage(r))
})
```

规则：

- `SPAHandler()` 与 `Mode`、`Renderer` 无关，不调用 goja；`ServeHTTP` / `Render` 仍按 SSR 工作。
- 它服务整个 `dist`（包括 `ssr.js`、`pages/*.html`），子路径挂载时注意暴露面；要更窄的范围就单独构建一个 SPA 产物和 Engine。
- 自动探测不会按路由猜：SSR 渲染失败或预渲染未命中**不会**自动回 SPA 外壳。回退只由你挂的路由决定。
- 前缀仍由 `Config.BasePath` 处理，不要再用 `http.StripPrefix` 剥一遍。
- 前端入口的坑：如果 `entry-client` 用 `hydrateRoot`，而 SPA 外壳里 `__WF_DATA__` 是空的，子路径页面可能 hydrate 一个空 root。让该子路径用 `createRoot`，或在入口里判断 `__WF_DATA__` 是否存在。

---

## 6. 开发模式：HMR + Go 热重启

### 6.1 应用里要有一段 dev server

开发态由 `dev` 包提供进程内转译 + HMR。用 build tag 把它隔离在开发构建里：

```go
// dev_mode.go
//go:build dev

package main

import (
	"net/http"

	"github.com/plunjoin/webfront/dev"
)

func runDev() {
	ds, err := dev.NewDevServer("./web") // 指前端工程目录，不是 dist
	if err != nil {
		panic(err)
	}
	http.ListenAndServe(":8080", ds.Handler())
}
```

生产构建里没有 `runDev`，但 `main.go` 仍然引用了它，所以还需要一个 `!dev` 的桩文件：

```go
// dev_stub.go
//go:build !dev

package main

import (
	"fmt"
	"os"
)

func runDev() {
	fmt.Println("这是生产构建（不含开发服务器）。开发模式请用：go tool wf dev")
	os.Exit(1)
}
```

```go
// main.go
func main() {
	if len(os.Args) > 1 && os.Args[1] == "dev" {
		runDev()
		return
	}
	runProd()
}
```

默认构建（不带 tag）不 import `dev`，所以 esbuild / fsnotify / websocket 不会进生产二进制。

### 6.2 `wf dev` 帮你做 Go 热重启

```bash
go tool wf dev
```

它等价于：

```bash
go build -tags dev .
./app dev
```

但多了三件事：

1. 监听整个 Go 模块（`go.mod` 所在目录），改 `.go` 自动重编译、重启进程；
2. 编译失败时旧进程继续服务，错误打在终端；改好自动拉起；
3. 给子进程注入 `WF_DEV=1`，SSR 渲染失败时浏览器里显示错误页而不是空白 500。

关键参数：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-tags` | `dev` | 传给 `go build` 的构建标签；置空则不带 |
| `-run` | `dev` | 传给应用的第一个参数；置空则不带 |
| `-watch` | `go.mod` 所在目录 | 监听范围 |
| `-ldflags` | — | 透传 |

`wf dev` 的 `rootDir` 是**应用目录**（`main.go` 所在目录），不是 `web/`。`web/src` 里的改动由应用内的 `dev.DevServer` 负责 HMR，不会触发 Go 重编译。

> 两个 “dev” 不要混淆：`-tags dev` 是**编译标签**（决定 `dev_mode.go` 进不进二进制），`dev` 是**程序参数**（决定 `main` 走不走 `runDev`）。`wf dev` 两个都替你填好了。

### 6.3 开发态错误覆盖层

以下四类错误会直接盖在浏览器页面上，不用开 DevTools：

- 前端编译失败（tsx/vue/css）；
- 浏览器运行期异常（`window.onerror` / `unhandledrejection`）；
- Go 进程重启（琥珀色遮罩，服务回来自动刷新）；
- SSR / goja 渲染失败（仅 `WF_DEV=1` 或 `Config.Dev = true` 时）。

生产态 SSR 失败只回干净的 500，不泄露栈。

---

## 7. 部署

### 7.1 构建顺序不能反

`//go:embed` 在编译期展开，所以必须是：

```
wf install → wf build → go build
```

`go build` 在前端产物生成之前执行，就会遇到 `no matching files found`。

### 7.2 多阶段 Dockerfile

```dockerfile
FROM golang:1.27-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 每个前端各自安装依赖（共享目录的坑见 §5.1）
RUN go tool wf install ./web/admin && go tool wf install ./web/portal

# 清掉本地 COPY 进来的旧产物，避免把过期页面/旧 chunk 一起 embed
RUN rm -rf web/admin/dist web/portal/dist

# admin: SPA + /admin 前缀；portal: 动态 SSR
RUN go tool wf build -mode spa -base /admin ./web/admin
RUN go tool wf build ./web/portal

# 动态 SSR 带 goja；纯 SPA / 预渲染去掉 -tags dynamic，镜像更小
RUN CGO_ENABLED=0 GOOS=linux go build -tags dynamic -trimpath -ldflags "-s -w" -o /out/app .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot
ENTRYPOINT ["/app"]
```

注意：

- `package-lock.json` 必须提交；Docker 里不需要 Node（lockfile 已经生成好）。
- CI 如果重新生成 lockfile，才需要 Node 步骤；生成后照常 `wf install`。
- `dist/` 不需要提交，但 `.gitkeep` 要提交，否则 `go build` 在“没跑过 wf build”的环境里会失败。
- `go tool wf` 依赖 `go.mod` 里的 `tool` 指令；`go mod download` 不会删它。
- 使用 `-out` 时注意：它的相对路径是**当前工作目录**，不是 `webDir`。能不用就不用，默认 `<webDir>/dist` 最省心。
- `-tags dynamic` 是**示例工程的约定**（`engine_dynamic.go` 上的 build tag），不是 webfront 的内置开关。你的应用如果用别的方式决定是否 import `jsruntime`，按自己的构建组织即可；要点是生产二进制只 import 需要的包。

### 7.3 纯 SPA / 预渲染的体积

| 构建 | 链接的包 | 大致体积 |
| --- | --- | --- |
| `go build .` + 预渲染 | 核心包 | ~6.9 MB |
| `go build .` + SPA | 核心包 | ~6.9 MB |
| `go build -tags dynamic .` | + jsruntime（goja） | ~15.6 MB |
| `wf dev` / `-tags dev` | + dev（esbuild 等） | ~11.4 MB |

体积差异来自**有没有 import**，不是运行期开关。生产构建里不要 import `jsruntime`、`compiler`、`dev`。

---

## 8. 从 Vite 迁移：哪些有、哪些没有

| Vite 里的东西 | webfront 对应 | 说明 |
| --- | --- | --- |
| `vite.config.ts` | **没有** | 构建参数走 CLI flags；运行期参数走 `webfront.Config` |
| `resolve.alias` | **没有 Vite 式配置** | `tsconfig.json` 的 `paths` / `baseUrl` 在 `wf build` 里会被 esbuild 自动识别并生效；但 `wf dev` 的逐文件 Transform 不解析别名。为了开发/生产一致，建议用相对路径或包名 |
| `define` / `import.meta.env` | 仅 `process.env.NODE_ENV` | 没有 `.env` 注入；运行期配置用 `Props` 或后端注入 |
| `public/` | **没有自动拷贝** | 构建后把静态文件复制进 `dist/`（`embed` 会一起带上） |
| PostCSS / Tailwind | **没有内置管线** | 先用 Tailwind CLI / PostCSS 生成 `.css`，再当普通 CSS import |
| 任意 Vite / Rollup 插件 | **不支持** | 内置的只有 Vue SFC 编译、store 解析、CSS 提取 |
| `server.proxy` | **没有** | API 由同一个 Go 服务提供；前端用相对路径请求 |
| `build.outDir` | `-out` / `Config.DistDir` | `-out` 相对当前工作目录 |
| `build.assetsDir` | 固定 `assets/` | 产物布局见 [API 契约 §8](API.md) |
| `base` | `-base` + `Config.BasePath` | 两边必须一致 |
| HMR | `wf dev` + `dev.DevServer` | 不是 Vite 的 HMR 协议，体验类似 |
| `entry-server` | `src/entry-server.*` 导出 `render` | 约定不同，见 §1.3 |
| `import.meta.glob` | **没有这个 API** | 改成显式 import 列表 |
| 动态 `import()` | 仅有限支持 | 表达式能被静态分析时（如模板字符串 glob）esbuild 会打包匹配文件；完全动态的表达式可能保留到运行期。不要依赖 Vite 的 glob 语义 |
| TypeScript 类型检查 | **不做** | esbuild 只剥类型；需要时自己跑 `tsc --noEmit` |

`tsconfig.json` 值得保留：IDE 和 `tsc --noEmit` 靠它做类型检查；`wf build` 也会让 esbuild 自动读取其中的 `paths` / `baseUrl`。但 `wf dev` 不解析这些别名，**跨开发/生产最稳的做法仍是相对路径或包名**。类型检查本身不由 `wf build` 执行，需要单独跑 `tsc --noEmit`。

---

## 9. 和 Go 路由 / 中间件的边界

### 9.1 挂载顺序

```go
mux.Handle("/api/", apiHandler)        // 最长前缀，优先
mux.Handle("/assets/", eng.StaticHandler())
mux.Handle("/", eng)                   // 兜底
```

`http.ServeMux` 按最长前缀匹配，与注册顺序无关。Gin 用 `r.NoRoute(gin.WrapH(eng))` 做兜底即可；引擎内部已经把 Gin 预设的 404 覆盖成 200（命中页面时）。

### 9.2 鉴权与 per-request 数据

```go
mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	user, ok := auth(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	_ = eng.Render(w, r, webfront.Page{
		URL:   r.URL.Path,
		Props: map[string]any{"user": user, "title": "工作台"},
	})
})
```

SSR 不是“把 Go 的 session 塞进前端”——`Props` 必须是可 JSON 序列化的普通数据；不要把 `*http.Request`、数据库连接、函数传进去。

### 9.3 静态资源不要走业务中间件

`eng.StaticHandler()` 自己处理 ETag / gzip / 304。如果外面再包一层日志或鉴权中间件，304 仍能工作，但注意不要重复设置 `Content-Type` / `Content-Length`。

---

## 10. 排错（按现象查）

| 现象 | 最可能的原因 | 处理 |
| --- | --- | --- |
| `go build` 报 `pattern all:web/dist: no matching files found` | `dist` 为空或被 gitignore 后没占位 | `mkdir -p web/dist && touch web/dist/.gitkeep`，并用 `//go:embed all:web/dist` |
| 嵌入后所有页面 / 资源 404 或空白 | `DistDir` 没对齐 `EmbedFS` 根 | `//go:embed all:web/dist` → `DistDir: "web/dist"`；embed 在 `web/` 里嵌 `admin/dist` → `DistDir: "admin/dist"` |
| SPA 首页正常，刷新 `/users` 404 | 用 `eng.Render` 做 fallback | SPA 用 `eng.ServeHTTP`（`mux.Handle("/", eng)`）或子路径 `eng.SPAHandler()`，不要用 `Render` |
| SSR 正常，`/app/*` 却 404 | SPA 子路径没挂 handler | `mux.Handle("/app/", eng.SPAHandler())`，见 §5.6 |
| 只建了 SPA，刷新却进了预渲染页 | `dist` 里残留旧 `pages/` | 重新 `wf build`（默认 `web/dist` 会清理），或手动删 `dist/pages` |
| `Render` 返回 err，自己写的 fallback 没生效 / 页面尾部多一段 | `Render` 失败时已经写了 500 | 改用 `eng.RenderHTML`，自己写响应 |
| `routes.json` 有内容，但 `dist/pages/` 是空的 | 字段写成了 `"url"` | 改成 `"path"`；空 `path` 会被静默跳过 |
| 预渲染站点除 `routes.json` 里的路径外全部 404 | 静态模式没有自动 SPA fallback | 补路由，或把该子路径挂到 `eng.SPAHandler()`，或改用 SPA / 动态 SSR |
| 子路径部署后 JS/CSS 404 | `-base` 和 `Config.BasePath` 不一致 | 两边用同一个前缀；前端路由仍写不含前缀的路径 |
| 资源路径写错，浏览器报 MIME / 语法错误而不是 404 | SPA 对未命中路径统一回 `index.html`（200 HTML） | 看响应的 `Content-Type`；核对 `-base` 与文件名 |
| `go run .../cmd/wf` 报 `missing go.sum entry` | 只 `go get` 了库，没登记 CLI 依赖 | `go get -tool .../cmd/wf`，或 `go install`，或 `tools/wf` 包一层 |
| `jsruntime.New` 报读不到 `ssr.js` | `wf build -mode spa` 或没有 `entry-server` | 去掉 `-mode spa`，补 `src/entry-server.tsx`，重新 build |
| `wf build` 报“找不到客户端入口” | 入口不在 `src/` 下，或文件名不匹配 | 用 `entry-client.*` / `main.*`，见 §1.2 |
| 新克隆跑 `go test ./...` 在 `webui` 测试上失败 | 测试依赖 `dist`，但产物没构建 | CI 先 `wf build`；或给测试加 build tag 隔离 |
| Docker 镜像里还是旧页面 | 层缓存 / `go build` 早于 `wf build` | 保证顺序；`COPY` 前端源码后再 build；必要时 `--no-cache` |
| 动态 SSR 首屏没样式 | `assets/client.css` 不在 `EmbedFS` / `DistDir` 下 | 看 `dist/assets/client.css` 是否存在；存在才会自动注入 `<link>` |
| `Page.Head.Title` 不生效 | `entry-server` 已经返回了 `<title>` | 库只在 head 片段没有 `<title>` 时补；删掉入口里的 title 或用 `Head` |
| SSR 报 `render timeout` | 单次渲染超过 5s | 优化查询 / 缩小 props；`jsruntime.NewWithTimeout` 调整 |
| SSR 报语法错误 `async function*` / `Intl` | goja 不支持部分 ES 特性 | 避免这些语法；SSR 产物已降到 ES2017 |
| Gin `NoRoute` 返回内容正确但状态码 404 | 自己包了一层处理器，没覆盖预设状态 | 命中后显式 `WriteHeader(200)`；直接用 `gin.WrapH(eng)` 时引擎已处理 |
| 改 `.go` 后 `wf dev` 没重启 | `rootDir` 指错 / `-watch` 指错 / 改的是 `_test.go` | 在 `main.go` 所在目录跑；`_test.go` 被有意忽略 |
| 共享 `wf_modules` 后某个前端解析不到包 | 最后一次 `install` 覆盖了 `manifest.json` | 每个前端独立依赖目录，或合并成一个 lockfile 只 install 一次 |
| 删掉某条路由后旧页面还在 | `wf build` 只清理固定产物（`ssr.js` / `pages/` 等），不清理旧 chunk / 自定义文件 | 发布构建前 `rm -rf web/dist`，或先清空产物目录 |

---

## 11. 最小检查清单

上线前过一遍：

- [ ] `web/package-lock.json` 已提交，`wf install` 能在干净环境跑通
- [ ] `web/dist/.gitkeep` 已提交，`go build` 在前端构建前也不会因 embed 失败
- [ ] `wf build` 在 `go build` 之前执行（本地脚本 / Dockerfile / CI 都是）
- [ ] `-base` 与 `Config.BasePath` 一致（如果挂在子路径）
- [ ] SPA 用 `ServeHTTP` / `SPAHandler` 做 fallback，没有误用 `Render`
- [ ] 混合部署时，SPA 子路径挂在 `SPAHandler()` 上；SSR 失败不会被静默降级成 SPA
- [ ] 需要自定义错误页时用 `RenderHTML`，没有在 `Render` 返回 err 后再写响应
- [ ] `routes.json` 用的是 `path`，且 `dist/pages/` 里能看到对应文件
- [ ] 生产构建没有 import `jsruntime` / `compiler` / `dev`（除非确实要动态 SSR）
- [ ] 每个前端的依赖目录独立，或共享目录只由一个 lockfile 驱动
- [ ] 发布构建清空了 `dist/`，没有残留旧路由 / 旧 chunk（`wf build` 会清理它自己的固定产物）
