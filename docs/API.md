# API 行为契约

> 这篇不介绍“怎么用”，只回答**调用之后到底发生了什么**：谁写响应、状态码是什么、错误返回后还能不能写、缓存头是什么。
> 面向已经能跑通、但想精确控制行为的人。第一次接入看 [接入指南](GUIDE.md)。

下文按当前源码（v0.1.2 工作区）描述。版本差异见 [§10](#10-版本差异)。

## 1. `New(Config)` 与生命周期

```go
func New(cfg Config) (*Engine, error)
```

`New` 只做组装，不启动服务。唯一的 I/O 是自动模式下的产物探测，且只在 `Renderer == nil` 时发生；探测失败一律当作“没有”，不会让 `New` 报错。

| 步骤 | 行为 |
| --- | --- |
| `BasePath` | 经 `NormalizeBase` 归一化：`""` / `"/"` → `""`；`"site/"` → `"/site"`；`"/site/"` → `"/site"` |
| `Mode` | 零值 = 自动解析：`Renderer != nil` → `ModeSSR`；否则 `pages/**.html` → `ModeSSR`（静态）；否则 `index.html` → `ModeSPA`；都没有 → `ModeSSR` 兜底。显式值原样保留 |
| `Dev` | `cfg.Dev || os.Getenv("WF_DEV") != ""`；后者由 `wf dev` 注入 |
| `EmbedFS` | 非 nil 时，`DistDir` 为空则用 `"dist"`；执行 `fs.Sub(EmbedFS, DistDir)` 得到 `clientFS` |
| `DistDir` | **相对 `EmbedFS` 根**，不是磁盘路径。`//go:embed all:web/dist` → `DistDir: "web/dist"`；embed 在 `web/embed.go` 里嵌 `portal/dist` → `DistDir: "portal/dist"` |
| 校验 | `Renderer == nil && EmbedFS == nil` → 返回 error；其余组合都允许 |
| 存在性 | `fs.Sub` 对 `embed.FS` 是惰性的；`DistDir` 写错或产物为空时通常不会在 `New` 报错，到请求时才 404。自动探测会 `WalkDir("pages")` 并 `Stat("index.html")`，找不到就是没有 |

`Engine` 持有 `Renderer`；`Close()` 会调用 `Renderer.Close()`。不要在一个已关闭的池上继续渲染。

```go
eng, _ := webfront.New(cfg)
defer eng.Close()
```

## 2. 模式矩阵

`Mode` 可以省略。`New` 在组装时解析出有效模式（`Engine.Mode()` 可读），显式值永远优先。`Mode` 只影响 `ServeHTTP` 的分派；`Render` / `RenderHTML` / `RenderTemplate*` 不看 `Mode`，只看有没有 `Renderer`。

**自动解析顺序（仅当 `Mode == ""`）**

1. `Renderer != nil` → `ModeSSR`（动态 SSR）
2. `clientFS` 里存在任意 `pages/**.html` → `ModeSSR`（预渲染 / 静态）
3. `clientFS` 里存在 `index.html` → `ModeSPA`
4. 都没有 → `ModeSSR`（请求期 404，不提前报错）

| 配置 | 有效模式 | `ServeHTTP` 行为 |
| --- | --- | --- |
| 零值 + `Renderer` | `ModeSSR` | 默认 Props 调 `Render`，动态渲染 |
| 零值 + 无 Renderer + `pages/` | `ModeSSR` | 默认 Props 调 `Render`，服务预渲染页 |
| 零值 + 无 Renderer + 仅 `index.html` | `ModeSPA` | 文件直出 + `index.html` history fallback |
| 零值 + 空 dist | `ModeSSR` | 调 `Render` → 404 |
| 显式 `ModeSSR` | `ModeSSR` | 不论产物，走 `Render`（有 Renderer 动态，否则找预渲染页） |
| 显式 `ModeSPA` | `ModeSPA` | 不论产物，走 SPA（没有 `EmbedFS` / `index.html` 时 404） |
| 显式 `ModeIslands` / 未知值 | 原值 | 未知值退化为 `Render`（SSR 分支） |

> `ModeSPA` + `Renderer` 现在是有效组合：`ServeHTTP` 出 SPA 外壳，显式调用 `Render` / `RenderTemplateTo` 仍然可用 goja。
> 想在同一个 engine 上同时提供 SSR 和某个 SPA 子路径，用 `Engine.SPAHandler()`（§3.4），不要靠自动探测按路由猜。
> 自动识别依赖 `dist` 里的产物。`wf build` 会清掉本次不再产出的 `ssr.js` / `pages/`；手动塞进 `dist` 的旧 HTML 仍需自己清理，或显式写 `Mode` 钉死。

## 3. `Engine` 方法逐个

### 3.1 `ServeHTTP(w, r)`

实现 `http.Handler`。

**BasePath 检查（最先执行）**

- `BasePath == ""`：不检查。
- 请求路径不是 `BasePath` 本身，也不以 `BasePath + "/"` 开头 → `http.NotFound`（纯文本 404），直接返回。

**SPA 分支（有效模式为 `ModeSPA` 时）**

1. `clientFS == nil` → 文本 404：`SPA 模式需要 EmbedFS`。
2. `name = stripBase(r.URL.Path)` 去掉首 `/`；空或 `index.html` 时跳过文件查找。
3. `name` 非空且不是 `index.html`：在 `clientFS` 里找同名文件；找到 → `serveAsset`（200，`Cache-Control: public, max-age=3600`）。
4. 找不到，或路径是 `/` / `/index.html`：回 `index.html`，`Cache-Control: no-cache`；`index.html` 也不存在 → 文本 404。
5. 注意：它服务的是 **整个 dist**，包括 `ssr.js`、`pages/*.html`，不只是 `assets/`。

> SPA 模式下，缺失的 `/assets/foo.js` 也会回 `index.html`（200 + HTML），而不是 404。资源路径写错时，浏览器通常报 MIME 类型 / 语法错误，而不是“资源 404”；排查时先看响应的 `Content-Type`。

**SSR / 静态分支（其他有效模式）**

```go
_ = e.Render(w, r, Page{
    URL:   r.URL.Path,
    Props: map[string]any{"title": "webfront SSR", "path": e.stripBase(r.URL.Path)},
})
```

- 默认 Props 固定，不查库、不鉴权。
- `Render` 的错误被丢弃（`_ =`）；失败响应由 `Render` 自己写。
- 不限制 HTTP 方法：POST / PUT 也会渲染页面。需要方法限制就自己包一层。

### 3.2 `Render(w, r, page) error`

**谁写响应：总是它写。** 成功和失败都写。

URL 解析：

```go
url := page.URL
if url == "" && r != nil { url = r.URL.Path }
url = e.stripBase(url) // /site/about → /about
```

**有 Renderer（动态）**

1. 调 `RenderHTML(url, page)`。
2. 失败：
   - 删除 `Content-Length`，设 `Cache-Control: no-store`；
   - 生产态（`dev == false`）：`http.Error(..., 500)`，纯文本，不泄露栈；
   - 开发态（`Config.Dev` / `WF_DEV`）：整页 HTML 错误页，500，带 goja 栈；
   - 返回原始 `error`。
3. 成功：
   - `Content-Type: text/html; charset=utf-8`；
   - **显式 `WriteHeader(200)`**（覆盖 Gin `NoRoute` 预设的 404）；
   - 写完整 HTML；
   - 返回 `nil`。

**无 Renderer（静态）**

委托 `servePrerendered`：

1. `p := strings.Trim(url, "/")`。
2. `p == ""` → 候选只有 `pages/index.html`；否则候选 `pages/<p>.html` 和 `pages/<p>/index.html`。
3. 命中 → `serveAsset`，200，`Cache-Control: public, max-age=60`（ETag / 304 / gzip 照常）。
4. 全不命中 → `http.Error("404 page not found: ...", 404)`，**返回 `nil`**。

> **静态模式的 404 返回的是 `nil`**。调用方不能靠 `err != nil` 判断失败；此时响应已经写成 404。如果要在 miss 时做自定义 fallback，只能在调用前自己判断文件是否存在（没有公开 API），或者直接改用 `ServeHTTP` / SPA。需要“渲染失败换一张页面”的场景，请用 `RenderHTML`。

**调用约定**

- `Render` 返回 err 时，**不要再写响应**。常见错误是：

  ```go
  if err := eng.Render(w, r, page); err != nil {
      http.Error(w, "fallback", 500) // 响应已经写过，这里只会追加/无效
  }
  ```

- 要自定义错误页 / fallback → `RenderHTML` + 自己写。
- `page.Props` 不可 JSON 序列化时，`RenderHTML` 会返回 marshal error；`Render` 把它当渲染失败，写 500。

### 3.3 `RenderHTML(url, page) (string, error)`

**不写响应。** 只返回拼装好的完整 HTML。

| 项目 | 行为 |
| --- | --- |
| `Renderer == nil` | 返回 error：`静态模式无渲染器（页面应由 wf build -prerender 预先生成）` |
| `url` | 为空时用 `page.URL`；**不做 `stripBase`** |
| `page.Props` | `json.Marshal`；nil map → `null`；channel/func/NaN → error |
| 渲染 | `Renderer.RenderHTML(url, propsJSON)`；错误原样返回 |
| 拼装 | `assemble(head, html, page)`，见 §5 |
| 响应头 | 不设置；调用方自己设 `Content-Type` / 状态码 |

**BasePath 注意**：挂在 `/site` 下时，`Render` 会自动剥前缀；`RenderHTML` 不会。直接调用时传前端视角的路径（`/about`），不要传 `r.URL.Path`（`/site/about`）。

**模板引擎快捷方式**

```go
html, err := eng.RenderTemplate("Home", HomeData{Title: "首页"})
err = eng.RenderTemplateTo(w, r, "Home", HomeData{Title: "首页"})
```

| 方法 | 写响应 | 行为 |
| --- | --- | --- |
| `RenderTemplate(name, data) (string, error)` | 否 | `data` 为 nil → `{}`；struct / map → JSON 对象；其他类型（数组、字符串等）→ error。内部调 `RenderHTML(name, Page{URL: name, Props: props})` |
| `RenderTemplateTo(w, r, name, data) error` | 是 | 先序列化；失败时自己写 500（开发态错误页）并返回 error；成功时走 `Render`，写 200 + HTML |

`name` 就是传给 SSR 入口 `render(name, props)` 的第一个参数，可以是组件名、文件路径或任意模板键。纯 HTML 模板用法只需要 `Config{Renderer: pool}`：不设 `EmbedFS` 时自动判为动态 SSR，也不会注入客户端脚本；`Mode` / `ClientScript` 都不用写。

### 3.4 `StaticHandler()` / `SPAHandler()`

**谁写响应：它们写。**

`StaticHandler()` 用于服务 `EmbedFS` 里的客户端产物。

| 情况 | 行为 |
| --- | --- |
| `clientFS == nil` | 文本 404：`no embedded client filesystem` |
| 路径 | `stripBase(r.URL.Path)` 去首 `/`；空 → `index.html` |
| 文件不存在 | 文本 404：`not found: <name>` |
| 命中 | 200；`Cache-Control: public, max-age=3600`；ETag / 304 / gzip |
| 目录 | 不列目录；打开目录后读取失败 → 404 |
| `HEAD` | 写头，不写 body |
| `Range` | 不支持（没有走 `http.ServeContent`） |

它服务整个 `clientFS`，所以 `mux.Handle("/assets/", eng.StaticHandler())` 之外，也可以挂 `/` 来直出所有嵌入文件。SPA 形态通常直接用 `ServeHTTP`，不需要额外挂 `StaticHandler`。

**`SPAHandler()`** 只做 SPA 分派，与有效 `Mode`、`Renderer` 都无关。它让同一个 engine 可以默认走 SSR，再把某个子路径交给纯客户端路由：

```go
mux.Handle("/assets/", eng.StaticHandler())
mux.Handle("/app/", eng.SPAHandler()) // 子路径 SPA
mux.Handle("/", eng)                  // 其余走 SSR / 静态
```

契约与 `ServeHTTP` 的 SPA 分支完全相同：

- 先做 BasePath 检查；前缀外 → 404。
- 路径剥 BasePath 后查 `clientFS`；命中真实文件 → 200 + `Cache-Control: public, max-age=3600`（ETag / 304 / gzip 照常）。
- 未命中 → 回 `index.html`，`Cache-Control: no-cache`；`index.html` 或 `clientFS` 不存在 → 文本 404。
- 服务的是整个 `dist`（包括 `ssr.js`、`pages/*.html`），子路径挂载时注意暴露面。
- 调用方不要把 `Config.BasePath` 用 `http.StripPrefix` 再剥一遍；handler 内部已经处理。

### 3.5 `Static() bool` / `Mode() Mode` / `BasePath() string` / `Close() error`

- `Static()`：`Renderer == nil` 时为 true。它只表示“没有 JS 渲染器”；自动模式可能把这种 engine 解析成 SPA，判形态请用 `Mode()`。
- `Mode()`：返回 `New` 解析后的有效模式。`ModeSSR + Static()` = 预渲染/静态；`ModeSSR + !Static()` = 动态 SSR。
- `BasePath()`：返回归一化后的前缀（挂根时 `""`）。
- `Close()`：有 Renderer 时调用其 `Close()`；否则返回 nil。

## 4. URL 与 BasePath 规则

| 场景 | 是否剥前缀 |
| --- | --- |
| `ServeHTTP` 前缀检查 | 不匹配直接 404；匹配后交给后续逻辑 |
| `Render` 的 `page.URL` / `r.URL.Path` | 剥 |
| `RenderHTML` 的 `url` | 不剥 |
| `StaticHandler` / `SPAHandler` 取文件 | 剥 |
| SPA 文件 / `index.html` 查找 | 剥 |
| 预渲染页面查找 | 剥（因为来自 `Render`） |

前端代码的约定：**路由始终写前端视角的路径**。挂在 `/site` 时，前端仍然是 `/about`、`/users/1`；不要写 `/site/about`。

自动补前缀的资源：

- 默认客户端脚本：`ClientScript` 为空，且 `EmbedFS` 里确实存在 `assets/client.js` 时，注入 `<BasePath>/assets/client.js`。
- 显式 `ClientScript`：以 `/` 开头且尚未带 `BasePath + "/"` 时，前面补 `BasePath`；`"-"` 关闭注入。
- 自动注入的 CSS：`<BasePath>/assets/client.css`。
- 前端 `entry-server` 自己返回的 `<link href="/assets/...">` **不会**被改写。子路径部署时，入口里不要写死绝对路径；用 `import './x.css'` 让库自动注入，或写相对路径。

`wf build -base /site` 只影响构建产物里的资源 URL；它不会创建目录、不会改路由、不会影响 Go 的挂载点。

## 5. HTML 拼装（`assemble`）

最终结构：

```html
<!doctype html><html><head>
  {head}
  <script id="__WF_DATA__" type="application/json">{propsJSON}</script>
</head><body>
  {html}
  <script type="module" src="{clientScript}"></script>
</body></html>
```

规则按顺序：

1. **Title**：`page.Head.Title != ""` 且 `head` 中不含子串 `<title` → 在前面插入 `<title>转义后的标题</title>`。
2. **Description**：`page.Head.Description != ""` 且 `head` 中不含 `name="description"` / `name='description'` → 追加 `<meta name="description" content="...">`。
3. **CSS**：`clientFS` 中存在 `assets/client.css`，且 `head` 不含子串 `assets/client.css` → 追加 `<link rel="stylesheet" href="{BasePath}/assets/client.css">`。
4. **数据**：`json.Marshal(page.Props)` 写入 `__WF_DATA__`。`encoding/json` 默认把 `<`、`>`、`&` 转义成 `\u003c`、`\u003e`、`\u0026`，防止 `</script>` 注入。
5. **客户端脚本**：`clientScript()` 非空时，用 `strings.Replace` 在第一个 `</body>` 前插入 `<script type="module" src="...">`。

补充：

- `assemble` **不添加** `<div id="root">`；hydration 根节点由 `entry-server` 的 `html` 提供。
- `assemble` 固定输出无属性的 `<html>`，也不添加 charset / viewport meta；需要这两类标签就在 `entry-server` 返回的 `head` 里写。`lang` 是 `<html>` 的属性，当前没有 API 可以设置（只能在外层包装/替换最终 HTML）。
- `props` 为 nil → `__WF_DATA__` 内容是 `null`，不是 `{}`。
- 客户端脚本是 `type="module"`（天然 defer）；无论客户端产物是 ESM 拆分还是单文件 IIFE，都能执行。

## 6. 静态资源、缓存与压缩

`serveAsset` 是所有嵌入资源（静态文件、预渲染页、SPA 外壳）的统一出口。

| 内容 | `Cache-Control` | 备注 |
| --- | --- | --- |
| `StaticHandler` 命中 | `public, max-age=3600` | 带 content hash 的 chunk 适合长缓存 |
| SPA 模式命中真实文件 | `public, max-age=3600` | 同上 |
| SPA 外壳 `index.html` | `no-cache` | 保证发版后能拿到新资源 |
| 预渲染页面 | `public, max-age=60` | 页面内容可能随发版变 |
| 动态 SSR 页面 | 不设置 | 由调用方决定 |
| `Render` 失败响应 | `no-store` | 先删 `Content-Length` 再写 |

ETag / 304：

- ETag = 内容 sha1 前 8 字节的十六进制，带双引号，如 `"a1b2c3d4e5f60708"`。
- 请求带 `If-None-Match` 且值包含该 ETag（子串匹配，兼容 `W/"..."`）→ 304，无 body。
- 不支持 `If-Modified-Since` / `Last-Modified`。

gzip：

- 仅当 MIME 可压缩（`text/*`、javascript、json、xml、svg）且原始体积 ≥ 1024 字节，并且压缩后确实更小。
- 命中 `Accept-Encoding: gzip` → 发压缩体，设 `Content-Encoding: gzip`，并设 `Vary: Accept-Encoding`。
- 资源在进程内首次读取时缓存（`sync.Map`），gzip 副本也只生成一次；嵌入资源在进程生命周期内不会变。

MIME：先查系统 `mime.TypeByExtension`，补了 `.js/.mjs`、`.css`、`.html`、`.json/.map`、`.svg`、`.woff2` 的兜底。

## 7. `routes.json` 与预渲染

### 7.1 格式

```go
type Route struct {
    Path  string         `json:"path"`
    Props map[string]any `json:"props"`
}
```

```json
[
  { "path": "/", "props": { "title": "首页" } },
  { "path": "/about", "props": {} }
]
```

- 顶层必须是数组；BOM 会被去掉。
- `path` 必填；**空字符串会被静默跳过**（`if rt.Path == "" { continue }`）。
- 未知字段被 `encoding/json` 忽略；写成 `"url"` 不会报错，但 `Path` 为空 → 不生成页面。
- `props` 可省略；省略时传给 `render` 的是 `null`。

### 7.2 输出映射

```go
func PageFile(url string) string {
    p := strings.Trim(url, "/")
    if p == "" { return "pages/index.html" }
    return "pages/" + p + ".html"
}
```

| URL | 文件 |
| --- | --- |
| `/` | `pages/index.html` |
| `/about` | `pages/about.html` |
| `/docs/intro` | `pages/docs/intro.html` |
| `/a/b/` | `pages/a/b.html` |

同名路由后写覆盖前写；没有重复检测。

### 7.3 构建期行为

- `PrerenderWithBase` 用 `jsruntime.New(bundle, 2)`（2 台 VM）逐条渲染；单次 5s 超时。
- 任一条失败 → 返回已写文件和 error；CLI 打印错误并以 1 退出。**可能留下部分 `pages/*.html`**。
- `clientScript` 默认 `/assets/client.js`；CLI 传的是 `<base>/assets/client.js`。
- 若 `outDir/assets/client.css` 存在，会把 `outDir` 挂成临时 `EmbedFS`，让 `assemble` 自动注入 CSS `<link>`。
- `-base` 会作为 `Config.BasePath` 传给预渲染引擎，影响自动注入的脚本/CSS 前缀；页面文件名不受影响。

### 7.4 运行期查找

`servePrerendered` 的候选顺序：

1. `/` → `pages/index.html`
2. 非根 → `pages/<trim 后的路径>.html`
3. 再尝试 `pages/<trim 后的路径>/index.html`
4. 都没有 → 404

预渲染产物里没有“通配 / fallback”；这是静态 `Render` 的硬边界。要在未命中时回 SPA 外壳，必须在路由层显式挂 `Engine.SPAHandler()`（§3.4），`Render` 本身不会自动回退。

## 8. 构建入口与产物

### 8.1 入口探测

| 入口 | 顺序 |
| --- | --- |
| SSR | `src/entry-server.tsx` → `.ts` → `.jsx` → `.js` → `src/entry-server.vue.tsx` → `.vue.ts` → `.vue.jsx` → `.vue.js` |
| 客户端 | `src/entry-client.tsx` → `.ts` → `.jsx` → `.js` → `src/main.tsx` → `.ts` → `.jsx` → `.js` |

- 客户端入口找不到 → `wf build` 报错退出（退出码 1）。
- SSR 入口找不到 → `hasServer = false`：跳过 `ssr.js` 和预渲染，产出纯 SPA；即使没传 `-mode spa`。
- `-mode spa` → 无论有没有 SSR 入口都跳过 SSR。
- `-mode template` → 只要求 SSR 入口；不检查客户端入口，不产 `index.html` / `assets/` / `pages/`，只写 `ssr.js`。未知 `-mode` → 退出码 1。

### 8.2 产物布局

```
<distDir>/
├── ssr.js                  # 仅 SSR bundle 存在时
├── assets/
│   ├── client.js           # 入口
│   ├── client.css          # 入口 import 了 CSS 时
│   └── chunks/*.js         # -split=true（默认）时的 vendor / 动态 chunk
├── index.html              # 非 template 模式生成
└── pages/*.html            # 预渲染时（template 模式不产）
```

- `index.html` 固定包含 `<div id="root"></div>` 和 `<script type="module" src="<base>/assets/client.js">`；存在 `assets/client.css` 时加 `<link>`。
- `-split=false` → 单文件 IIFE：`assets/client.js`，没有 chunks。注意这条路径只取 esbuild 的第一个输出文件：入口 import 了 CSS 时 CSS 会被丢弃（不报错），有样式就保持默认 `-split=true`。
- `-mode template` → 只产 `ssr.js`；没有 `assets/` / `index.html` / `pages/`，也不要求客户端入口。`-split` 与 `-prerender` 在这条路径下不生效。
- `-out` 指定输出目录；相对路径相对**当前工作目录**（不是 `webDir`）。
- **构建只清理自己管理的固定产物**：成功后按需删除本次不再产出的 `ssr.js`、`pages/`，template 模式还会删 `index.html` / `assets/`；本次没产 CSS 时，旧的 `assets/client.css` 也会先删掉，避免外壳继续引用过期样式。清理只在产物目录位于前端工程内部时执行——`-out web`（工程根）或工程外目录会跳过，宁可留下旧文件也不删源码。删除路由后遗留的旧 chunk、旧 hash 文件、用户自己放进 `dist` 的静态文件不会被清理，仍会被 `//go:embed` 打进二进制；发布构建前建议 `rm -rf <distDir>`。

### 8.3 `wf install` 的输入

- 只读 `<webDir>/package-lock.json`；不读 `package.json` 的依赖声明。
- lockfile 必须有 `packages` 映射（npm v7+ 生成的格式）；没有 → `lockfile has no packages (run npm install first)`。
- 读取每个包的 `resolved` + `integrity`，16 并发下载 `.tgz`，校验 sha512，解压到 `<webDir>/<modulesDir>/.store/<name>@<version>/`。
- 已存在 `package.json` 的包会跳过（不重复下载）。
- 最后**覆盖**写 `<modulesDir>/.store/manifest.json`，记录本次 lockfile 的包与根依赖；不做多 lockfile 合并。
- 不做版本解析：`package.json` 里的 `^` / `~` 由 npm 生成 lockfile 时解析，`wf install` 只负责精确拉取。

## 9. CLI 契约

### 9.1 退出码

`cli.Run(args) int` 不调用 `os.Exit`：

| 情况 | 退出码 |
| --- | --- |
| `help` / `-h` / `--help` | 0 |
| 无参数 | 1（打印 usage） |
| 未知命令 | 1 |
| flag 解析失败 | 2 |
| 子命令成功 | 0 |
| 子命令业务失败 | 1 |

`cmd/wf` 只是 `os.Exit(cli.Run(os.Args[1:]))`。

### 9.2 目录解析

- `rootDir` 默认 `.`。
- `webDirOf(root)`：`root/web` 存在且是目录 → 用它；否则用 `root`。
- `-modules auto`：优先选已有 `.store/manifest.json` 的 `wf_modules` / `node_modules`；否则选已存在的目录；都没有 → `wf_modules`。
- `-prerender auto`：`<webDir>/routes.json` 存在 → 启用；`-` / `off` / `none` → 关闭；显式路径 → 原样使用。
- 自动预渲染但没有 SSR bundle → 打印提示并跳过；显式 `-prerender` 但没有 SSR bundle → 报错退出 1。

### 9.3 `wf build` 的依赖检查

依赖目录为空且存在 `package-lock.json` → 打印“先 install”并退出 1。没有 lockfile 时不在这里拦截，后续 esbuild 会报解析错误。

### 9.4 `wf dev`

- `rootDir` 是应用目录（`main.go` 所在目录），不是 `web/`。
- 默认 `go build -tags dev <dir>`，然后运行 `<binary> dev`。
- 监听 `-watch`（默认 `go.mod` 所在目录），忽略 `node_modules` / `wf_modules` / `dist` / `vendor` / `.git` / `*_test.go`。
- 子进程环境追加 `WF_DEV=1`。
- 编译失败不杀旧进程；子进程 panic 退出后，改代码会重新拉起。

## 10. 版本差异

### v0.1.1 → v0.1.2（工作区）：配置简化 + SSR/SPA 共存

- `Config.Mode` 可省略：按 `Renderer` → `pages/*.html` → `index.html` 自动解析；显式值仍然优先。
- 新增 `Engine.SPAHandler()`：同一个 engine 可以 SSR 为主，同时给某个子路径提供 SPA fallback。
- 新增 `Engine.Mode()`：自动解析结果可见。
- `ModeSPA` + `Renderer` 从“矛盾组合”改为有效组合；`ServeHTTP` 出外壳，显式 `RenderTemplate*` 仍可用。
- `wf build` 成功后清理本次不再产出的 `ssr.js` / `pages/`（预渲染 0 个页面也算未产出；template 模式还会清理 `index.html` / `assets/`），避免旧产物把自动识别带偏。清理只在前端工程内部的输出目录执行。
- `wf build -mode template` 只产 `ssr.js`；新增 `RenderTemplate` / `RenderTemplateTo`。

### v0.1.0 → v0.1.1：`Render` 的错误响应

v0.1.0 的 `Render` 在渲染失败时只返回 error，不写响应；调用方普遍写 `_ = eng.Render(...)`，浏览器因此可能收到空白的 200。v0.1.1 起：

- `Render` 失败时自己写 500（生产纯文本 / 开发 HTML 错误页），再返回 error；
- 升级后**删掉**调用方在 `err != nil` 分支里再写响应的代码；
- 需要自定义 fallback 的，改用 `RenderHTML`。

### v0.1.1 新增

- Vite 同款开发错误覆盖层（前端编译 / 浏览器异常 / Go 重启 / SSR 失败）；
- `wf dev`：Go 源码变更自动重编译重启，浏览器自动恢复；
- 构建期错误使用 esbuild 的 code frame。

## 11. 已知边界

| 边界 | 说明 |
| --- | --- |
| 无流式 SSR | 只有同步 `renderToString`，不支持 `renderToPipeableStream` + Suspense |
| goja 语言能力 | 不支持 `async function*` / `Intl`；SSR 产物目标 ES2017 |
| `ModeIslands` | 未实现；显式设置时 `ServeHTTP` 退化为 SSR 分支，自动识别不会选它 |
| 静态模式无自动 fallback | 纯静态 / `ModeSSR` + `Renderer == nil` 时未命中就是 404；需要回退请在路由层显式挂 `SPAHandler`，库不会自动把 404 变成 200 |
| `Render` 不看 `Mode` | 配了 Renderer 就会动态渲染，哪怕有效模式是 `ModeSPA` |
| 自动识别靠产物 | `dist` 里残留的 `pages/*.html` 会把形态判成静态；`wf build` 会清理固定产物，手动塞入的文件需自己清，或显式写 `Mode` |
| 构建只清理固定产物 | 在前端工程内部的输出目录里，`ssr.js` / `pages/` / template 下的 `index.html` / `assets/` 会被清理；`-out` 指到工程根或工程外时跳过清理，用户自己放进 `dist` 的其他旧文件仍会被 embed |
| 无 `public/` 拷贝 | 静态文件需构建后自己放进 `dist/` |
| 无 Vite 式 aliases / `import.meta.env` / PostCSS | `tsconfig` 的 `paths` 只在 `wf build` 生效，`wf dev` 不解析；见 [接入指南 §8](GUIDE.md) |
| 无目录列表 / Range | `StaticHandler` 只做整文件 + ETag + gzip |
| 单次渲染默认 5s 超时 | `jsruntime.NewWithTimeout` / `Pool.SetTimeout` 可调 |
| 预渲染内容固定 | 动态数据只能客户端补或改用动态 SSR |
| `-mode template` 不产 CSS / hydration | 只输出 `ssr.js`；页面是纯 HTML，样式与交互需自行处理 |
