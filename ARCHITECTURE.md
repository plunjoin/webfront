# webfront 架构设计文档

> 一个 100% 纯 Go 编写、可嵌入任意 `net/http` / Gin 路由的前端编译器与 SSR 运行时库（配极薄 CLI），单仓库同进程搞定 SPA、SSR、SSG 与 Islands 孤岛架构。对标并替代 Vite + Astro，完全摒弃 Node.js。
>
> **状态：v0.1.0 已实现并通过验证**（不再是设计草案）。最后更新：2026-10-31。
> 验证环境：Go 1.27.0 / windows-amd64，全部 `CGO_ENABLED=0`。
> 本文档描述的是**代码里真实存在的实现**，所有体积、字节数均为实测值；与草案不一致之处见 §0。

---

## 0. 与设计草案（v0）的差异速览

| 维度 | 草案 v0 | 现状 v0.1.0 |
| --- | --- | --- |
| 包结构 | 单包 `pkg` | **5 个包按需付费**（`webfront` / `compiler` / `jsruntime` / `dev` / `cmd/wf`） |
| JS 引擎 | go-quickjs 双后端 | **单一 goja**（+9.5MB）；完整 ES2023 引擎已从代码移除，仅作文档备选（§5.6） |
| 渲染时机 | 仅运行期 | **运行期动态 SSR** 或 **构建期预渲染（SSG）**，后者运行期零引擎 |
| `Config.SSRBundle` | 有此字段 | **已删除**，改为注入 `Renderer` 接口实现（解耦引擎与引擎二进制） |
| 客户端产物 | 单文件 IIFE | **ESM 入口 + vendor chunk**（Vite 式代码分割），IIFE 作降级 |
| 依赖安装布局 | `node_modules` 平铺 | **`.store/<name>@<ver>/` 版本化 store**（唯一布局，多版本共存） |
| 安装器 API | `InstallFromLock` / `InstallFromLockDir` | 合并为 `InstallStore` |
| 框架适配 | 库内置 `WrapGin`（硬依赖 gin） | **移除**，`Engine` 即 `http.Handler`，使用者一行 `gin.WrapH`（§3.6） |
| 体积 | 未评估 | 三档实测：**6.89 / 15.62 / 11.36 MB**（另：备选引擎形态 33.14 MB，代码已移除） |
| dev 下的 `.vue` | 计划支持 | **已完整支持**（含 SFC 编译、`<style>` 提取与 HMR 热更，见 §7.1） |

---

## 1. 顶层定位与硬约束

### 1.1 一句话定义

一个纯 Go 的前端**编译器 + SSR 运行时**库，单仓库同进程搞定 SPA / SSR / SSG / Islands，开发热重载、生产内嵌单二进制，**全链路零 Node、零 CGO**。

### 1.2 三大铁律（不可妥协，已实测）

| 铁律 | 含义 | 实测证据 |
| --- | --- | --- |
| **零 Node.js 进程** | 编译 `.tsx`/`.vue`、打包、SSR 渲染、装依赖，全链路无 `node` | 全部产物由 `esbuild` Go API + 内嵌 JS 引擎产生；`wf install` 直连 npm registry |
| **零 CGO** | `CGO_ENABLED=0` 可构建，任意平台交叉编译 | 四个直接依赖（esbuild / goja / fsnotify / gorilla-websocket）`go list -f '{{.CgoFiles}}'` 均为 `[]` |
| **单进程单二进制** | 开发态前端 DevServer 与业务同进程；生产态产物 `//go:embed` 进可执行文件 | `_examples/basic` 构建出的单文件 6.89MB，磁盘上无需 `web/dist` 即可服务 |

### 1.3 第四条原则（v0.0.1 新增）：按需付费（pay-as-you-go）

草案只说了"单二进制"，但没说**单个二进制里该装什么**。实测发现 JS 引擎占 9.5MB（完整 ES2023 引擎要 26.4MB）、esbuild 占 4.4MB；再顺手 import 一个 Web 框架（Gin 就是 6.5MB）——它们对"只想托管一个预渲染页面"的用户全是死重量。

因此引入硬性分层规则：

> **核心包 `webfront` 只允许 import 标准库。** 任何重依赖（esbuild、JS 引擎、fsnotify、websocket）必须落在独立子包里，由使用者按需 import，由 Go linker 决定链不链进去。
>
> **推论：库不依赖任何 Web 框架。** 框架适配是使用者侧的一行代码（`gin.WrapH` 等），不该写进库的 `go.mod`——否则所有 Echo / Chi / 原生 `net/http` 用户都要为 Gin 的 30 多个间接依赖买单。

这条规则可机械校验（`go list -f '{{join .Imports " "}}' .` 的输出里只有 stdlib），也是"同一份代码能打出 6.89MB 和 15.62MB 两种二进制"的根本原因。

### 1.4 成品形态：Go 包 + 极薄 CLI

- **Go 核心库**：实现标准库 `http.Handler`，可挂到任意框架（Gin / Echo / Chi / 原生 `net/http`），接入代码都是一行。
- **极薄 CLI（`cmd/wf`）**：仅构建期使用（`wf build` / `wf install`），不参与运行期，因此不违反"单进程/不跑俩 service"。

> 不做"完整 Web 框架"，也不绑定某个框架。用户已有自己的路由，库只补前端短板；框架级约定（文件路由等）可日后在包之上叠加。

---

## 2. 仓库分层与依赖图

### 2.1 目录结构

```
webfront/
├── webfront.go          核心：Config / Engine / Renderer / HTML 拼装   ← 仅 stdlib
├── static.go            核心：StaticHandler（gzip 预压缩 + sha1 ETag + 304）
├── compiler/            构建期：esbuild 打包 / SFC / store / SSG 预渲染
│   ├── bundler.go         BuildSSRWith · BuildClientWith · BuildClientSplit
│   ├── engine.go          New()：构建 SSR 后起引擎（单机自编译场景）
│   ├── prerender.go       ReadRoutes · Prerender · PageFile
│   ├── install.go         InstallStore（lockfile 驱动的纯 Go 安装器）
│   ├── store.go           版本化 store 解析（manifest.json）
│   ├── vue.go             @vue/compiler-sfc 的 esbuild 插件
│   └── preprocess.go      <style lang="scss|sass|less"> 的纯 Go 预处理器
├── jsruntime/           运行期：JS 引擎池（goja）
│   ├── goja.go            Pool + 宿主 shim + 微任务收口
│   └── quote.go           JS 字符串字面量转义
├── dev/                 开发态：esbuild Transform + fsnotify + WS HMR
│   ├── dev.go            DevServer：按需转译 / @deps / WebSocket
│   └── supervisor.go     wf dev：进程级热重载（重编译 + 换进程 + 探活）
├── cmd/wf/              极薄 CLI
├── _examples/basic/     三形态示例（build tag 切换）+ 前端工程 web/
                         （`_` 前缀：Go 工具链跳过，`go build ./...` 不受示例影响）
├── go.mod / go.sum      4 个直接依赖，全部纯 Go
└── .gitignore           dist/ · wf_modules/ · *.exe 不入库（可再生产物）
```

**入库的只有源码**：整个仓库约 160KB。`web/dist`（`wf build` 产物）、`web/wf_modules`（`wf install` 拉下的依赖 store）、`*.exe` 全在 `.gitignore` 里——它们跑两条命令就能重建，进版本库只会让 clone 变慢、diff 变脏。`dist/` 里保留一个 `.gitkeep`，让 `go build` 在没有产物时也能通过（否则 `//go:embed all:web/dist` 会直接报 `no matching files found`，整个仓库的 `go build ./...` 都会挂）。

### 2.2 依赖方向（严格单向，无环）

```
cmd/wf ─────────→ compiler ──┬─→ webfront                    (stdlib only)
                             └─→ jsruntime ──→ goja

_examples/basic ─┬─→ webfront                                (始终)
                 ├─→ jsruntime                               (仅 -tags dynamic)
                 └─→ dev ──→ esbuild / fsnotify / websocket  (仅 -tags dev)
```

三个关键点：

1. **`webfront` 绝不 import `compiler`**——否则 esbuild（+4.35MB）会被无条件链进每个生产二进制。
2. **库的 4 个直接依赖全部藏在子包里**，`import "github.com/plunjoin/webfront"` 本身只带 stdlib，链接成本 ≈ 1.2MB。
3. **没有 `ginadapter` 这类框架适配包**——`Engine` 就是 `http.Handler`，框架桥接由使用者一行完成（§3.6）。

### 2.3 各包链接成本（隔离实测，均为 `-ldflags "-s -w"` 文件体积差值）

| 增量项 | 体积 | 何时付出 |
| --- | --- | --- |
| 空壳 `net/http` 基线 | 5.72 MB | 恒定（Go 运行时 + 网络栈） |
| `webfront` 核心 + 嵌入产物 | +1.17 MB | 恒定（其中前端产物 0.27MB） |
| `esbuild`（Transform 真实调用） | +4.35 MB | 仅 `dev` / `wf` CLI |
| 任何 Web 框架（以 Gin 为例） | +6.53 MB | **库不付这个成本**；用户自己 import 才付 |
| `goja` | **+9.46 MB** | 仅 `-tags dynamic` |

> 完整 ES2023 引擎（备选方案）的评估值为 +26.38 MB，当前代码未接入，见 §5.6。

> 测量方法：构造最小可执行文件，逐一加入依赖并真实调用（不能只写 `_ = api.Build`——Go 编译器会把它优化掉，导致低估 4MB）。符号表求和不可用于体积归因：`crypto/internal/fips140/drbg.memory` 一个符号就报 32MB（BSS，不占文件空间）。

---

## 3. 运行时核心（package webfront）

### 3.1 Config（实际字段）

```go
type Config struct {
    RootDir     string // 前端工作目录，默认 "./web"
    BasePath    string // 路由挂载前缀，默认 "/"
    Mode        Mode   // ModeSPA / ModeSSR / ModeIslands
    Dev         bool   // true: esbuild 实时转译 + fsnotify + HMR
    EmbedFS     fs.FS  // 生产环境嵌入的文件系统（//go:embed）
    DistDir     string // 生产产物在 EmbedFS 中的子路径，默认 "dist"
    ServerEntry string // 默认探测 src/entry-server.(tsx|ts)
    ModulesDir  string // 依赖目录名，默认 "node_modules"（可改 "wf_modules"）

    // Renderer 动态渲染器。nil → 引擎走"预渲染静态模式"：
    // 按 URL 取 <dist>/pages/<path>.html，不再执行任何 JS。
    Renderer Renderer

    // ClientScript 注入页面的客户端脚本路径，默认 "/assets/client.js"。
    // 设为 "-" 关闭注入（纯静态、无需 hydration 的页面）。
    ClientScript string
}
```

`SSRBundle []byte` 字段**已删除**。原因：它把"引擎"和"引擎要执行的代码"耦合进 Config，导致核心包必须认识 SSR 产物格式；改成接口后，核心包对"HTML 从哪来"完全无知。

### 3.2 Renderer 接口：引擎与引擎二进制的解耦点

```go
type Renderer interface {
    RenderHTML(url string, propsJSON []byte) (html, head string, err error)
    Close() error
}
```

这一个接口带来三个能力：

1. **静态部署不链接引擎**——`Renderer` 为 nil，`jsruntime` 包整体不进入编译单元；
2. **引擎可替换**——任何引擎只要实现它就能接进来（当前是 goja），上层代码零改动；
3. **预渲染复用同一套代码**——`wf build -prerender` 在构建期构造一个临时 `Renderer` 跑一遍，与运行期走完全相同的 `assemble` 路径，因此产物逐字节一致（见 §11）。

### 3.3 渲染流水线

```
Render(w, r, Page)
  ├─ renderer != nil → RenderHTML(url, page)
  │     ├─ json.Marshal(props) → propsJSON
  │     ├─ renderer.RenderHTML(url, propsJSON) → (html, head)
  │     └─ assemble(head, html, props)
  └─ renderer == nil → servePrerendered(w, r, url)
```

`assemble` 负责拼装完整文档，且**XSS 防护在这一层统一做**：

```go
dataJSON, _ := json.Marshal(props)   // 默认把 < > & 转成 \u003c 等，防 </script> 注入
doc := "<!doctype html><html><head>" + head +
    `<script id="__WF_DATA__" type="application/json">` + string(dataJSON) + `</script>` +
    "</head><body>" + html + "</body></html>"
```

客户端脚本统一用 `<script type="module">`：代码分割产物是 ESM，模块脚本天然 defer 不阻塞解析；未拆分的 IIFE 在模块里同样可执行，两种产物共用同一标签。

### 3.4 静态资源服务（static.go）

生产态资源在编译期就固定，故采取"首次读、永久缓存、预压缩、强校验"策略：

| 机制 | 实现 | 效果 |
| --- | --- | --- |
| 内存缓存 | `sync.Map[name]*staticAsset` | 磁盘/embed 只读一次 |
| gzip 预压缩 | 首次加载时 `gzip.BestCompression` 生成副本常驻 | 省 CPU，不用每请求压 |
| 压缩阈值 | 小于 1024B 不压（gzip 头尾约 18B，小文件反而变大） | — |
| 强 ETag | 内容 `sha1` 前 8 字节 hex | 内容变则 ETag 必变，可长缓存 |
| 条件请求 | `If-None-Match` 命中 → 304 | 二次访问 0 字节传输 |
| `Vary` | 有 gzip 副本时回 `Vary: Accept-Encoding` | 防中间缓存串味 |

### 3.5 预渲染页面路由（静态模式）

`servePrerendered` 的映射规则与 `compiler.PageFile` 严格对称：

| URL | 候选文件 |
| --- | --- |
| `/` | `pages/index.html` |
| `/about` | `pages/about.html` → `pages/about/index.html` |
| `/a/b` | `pages/a/b.html` → `pages/a/b/index.html` |

命中即返回（带 ETag + `max-age=60`），全不命中返回 404 并明确提示"预渲染产物里没有这个路由"——不静默 fallback 到首页，避免掩盖构建遗漏。

### 3.6 框架接入：库不依赖任何框架

`Engine` 就是一个 `http.Handler`，所以接入不需要库提供适配器——各框架自己都有现成的 `http.Handler` 桥：

```go
// 原生 net/http
mux := http.NewServeMux()
mux.Handle("/assets/", eng.StaticHandler())
mux.Handle("/", eng)

// Gin（gin.WrapH 是 gin 自带的，不需要 webfront 提供）
r := gin.New()
r.GET("/assets/*filepath", gin.WrapH(eng.StaticHandler()))
r.NoRoute(gin.WrapH(eng))          // 兜底接管未匹配路由

// Echo
e.GET("/*", echo.WrapHandler(eng))

// Chi
r.Handle("/assets/*", eng.StaticHandler())
r.Handle("/*", eng)
```

**为什么库不内置 Gin 适配器**：那 4 行代码会让 `go.mod` 硬依赖 Gin——连带 `sonic`、`quic-go`、`protobuf`、`validator` 等 30 多个间接依赖进入每个使用者的模块图，即使他们用的是 Echo 或原生 `net/http`。适配器应该是使用者的 4 行，不是库的 30 个依赖。

> 实测：移除 Gin 适配层后，`go.mod` 的间接依赖从 **32 个降到 5 个**，`go.sum` 从 **106 行降到 23 行**。

---

## 4. 编译器（package compiler）

### 4.1 构建产物三件套

`wf build` 一次产出：

| 产物 | 体积（React 示例） | 用途 |
| --- | --- | --- |
| `dist/ssr.js` | 132,669 B | SSR bundle，IIFE，挂 `globalThis.__WF_APP__ = { render }` |
| `dist/assets/client.js` + `dist/assets/chunks/*.js` | 816 B + 141,787 B | 浏览器 hydration |
| `dist/pages/*.html` | 479 B / 483 B | 可选，预渲染页面 |
| `dist/index.html` | 175 B | SPA 回退 / 静态托管 |

### 4.2 SSR 构建：`Target: ES2017` 的降级策略

```go
Target: api.ES2017,
```

SSR 产物不是给浏览器跑的，是给**我们的 JS 引擎**跑的。goja 不支持 async generator、逻辑赋值等新语法，但 esbuild 能把它们编译成 ES2017 等价代码。这一行让"引擎不必追新语法"，代价是 `ssr.js` 增大 **0.5%**。

客户端产物**不降级**（浏览器原生支持，没必要为老语法付费）。

### 4.3 客户端构建：三层压缩

```go
MinifyWhitespace:  true,
MinifyIdentifiers: true,
MinifySyntax:      true,
```

| 阶段 | 体积 | 说明 |
| --- | --- | --- |
| 原始（未压缩） | 258,323 B | React 18 单页 |
| minify 后 | 142,541 B | −44.8% |
| gzip 后（线上传输） | 45,634 B | 相对原始 −82.3% |

> 注意 `api.BuildOptions` 没有 `Minify bool` 总开关，必须分别置三个 `Minify*`。

### 4.4 代码分割：两遍构建复刻 Vite vendor chunk

esbuild 的 `Splitting` 只在 ESM 下生效，且**没有 `manualChunks`**，只按"模块被几个入口共享"自动切分。直接开 `Splitting` 对单入口应用等于没拆。于是用两遍构建复刻 Vite 的行为：

```
第 1 遍（探路） 普通构建 + Metafile:true
   ↓ 从 metafile 的 outputs[].inputs[] 反推"客户端真实用到的包"
   ↓ pkgFromStorePath: <md>/.store/@vue+runtime-dom@3.5.13/... → "@vue/runtime-dom"
   ↓ filterRootDeps: 只留 store manifest 里的根依赖
第 2 遍（正式） 两个入口一起构建
   入口 A = src/entry-client.tsx
   入口 B = "wf:vendor"（虚拟入口，仅 `import "pkg"` 一遍，本身没人加载）
   ↓ 共享依赖被提升成 chunks/<name>-<hash>.js，业务代码留在 client.js
```

三个必须踩过的坑（都已修）：

1. **虚拟入口的裸导入解析失败** → `OnLoadResult` 必须带 `ResolveDir`，否则 esbuild 不知道从哪个目录找包。
2. **传递依赖写进虚拟入口导致 `Could not resolve "scheduler"`** → 只保留 `manifest.Root` 里登记的根依赖；`scheduler` 由 `react-dom` 自己带进来。
3. **产物误写到 `web/web/dist`** → `Outdir` 是相对 `AbsWorkingDir` 的，CLI 必须传 `"dist/assets"` 而非绝对路径。

产物与收益：

```
dist/assets/client.js                    817 B     ← 业务代码 + 静态 import（小于 1KB，运行期不 gzip）
dist/assets/chunks/chunk-OQ6JHRQW.js 141,787 B     ← React/ReactDOM（content hash）
HTTP 实际传输（Accept-Encoding: gzip）  817 + 45,279 = 46,096 B
```

体积与单文件方案基本持平（142,604 vs 142,541），**收益在缓存粒度**：改业务代码时 vendor chunk 的 hash 不变，老用户只重下 817B 的入口。

入口与 chunk 的符号对齐已验证：chunk 侧 `export{Rn as a, ...}` 与入口侧 `import{a as l, ...}` 一一对应。

> `vendor.js` / `wf_vendor.js` 这两个锚点产物会被过滤掉不落盘——它只是"把依赖逼成独立 chunk"的工具，没人 import 它。

### 4.5 Vue SFC 编译插件与样式系统（vue.go）

在纯 Go 环境下驱动 `@vue/compiler-sfc`，将 `.vue` 文件无缝编译为标准 ESM 模块：
- **字节码预编译与常驻 VM 池**：`stub.js` 与 `@vue/compiler-sfc` 产物一次性预编译为 `*goja.Program`，维护 `compilerPool` 实例池，单文件编译进入毫秒级，彻底消除重复解析 AST 的开销。
- **`<style>` / `<style scoped>` 提取与作用域哈希**：自动调用 `compileStyle` 提取样式，生成确定性 `scopeId`（`data-v-xxxx`）并打入模板标签与组件对象。
- **开发态 HMR 样式去重**：开发态生成的注入脚本带有 `data-wf-id` 属性，热重载时覆盖更新同名 `<style>`，杜绝标签无限堆积。
- **动态 SSR 样式首屏直出（防 FOUC）**：服务端渲染期间将组件样式推入全局 `__WF_STYLES__`，并在渲染完成后聚合成 `<style>` 拼入 HTML `<head>`，彻底消灭首屏无样式闪烁。
- **多工程与并发测试隔离**：以 `absRootDir|modulesDir` 隔离编译器实例，构建失败不入缓存，避免跨工程或单测相互污染。

### 4.5.1 CSS 预处理器（preprocess.go）

`<style lang="scss|sass|less">` 走的是同一条"打包成 IIFE → goja 里跑"的路子，但换了一份本体：

| | 预处理器本体 | 入口 | 单文件耗时 |
| --- | --- | --- | --- |
| `scss` / `sass` | 项目装的 `sass`（dart-sass） | 包名 `sass` → exports 的浏览器构建 `sass.default.js`（6.4MB） | ≈60ms |
| `less` | 项目装的 `less` | `less/lib/less/index.js` + `default-options.js`（核心工厂，413KB） | ≈5ms |

三个设计取舍：

1. **不内置、只按需加载**。预处理器从项目自己的 `node_modules` / `.store` 解析，和 `@vue/compiler-sfc` 完全同源——版本跟着项目走，二进制里不多带 6MB。没装就在**用到的那一刻**报 `npm i -D sass`，不会静默降级。
2. **less 不能用它的浏览器构建**。`less/dist/less.js` 是给 `<script>` 标签用的 IIFE，一加载就摸 `window.less` / `document`；这里改成只打包核心工厂，环境（`Environment` + `FileManager` + `PluginLoader`）由驱动脚本自己拼，`@import` 的解析规则全部复用 `AbstractFileManager`，只把"读文件"换成 Go 的 `__wfRead`。
3. **dart-sass 要避开它的 Node 分支**。`sass.dart.js` 用 `process.versions.node` 判断"这是不是真 Node"，判定为真就去 `require('url')`——goja 里没有 `require`，直接 `Dynamic require of "url" is not supported`。所以垫片里 `process.versions` 是个**空对象**，让它老老实实走浏览器分支。

预处理器跑在**独立 VM** 里（不污染 `@vue/compiler-sfc` 的全局），只负责"预处理器 → 纯 CSS"；`scoped` / `:deep()` / `v-bind()` 仍交给 `compileStyle`，两步之间靠 `__wfStyles` 传一个 `[{content, scoped}]` 数组。`@use` / `@import` 的相对解析、`_partial`、目录 `index`、以及 `node_modules` 回退都在驱动的 importer 里（sass 用 `canonicalize` / `load`，less 用 `loadFileSync` + `syncImport`，因此整条链路同步，不需要在 goja 里处理 Promise）。

### 4.6 版本化 store（store.go / install.go）

`wf install` 读 `package-lock.json`，纯 Go 下载 `.tgz` + `sha512` integrity 校验 + 解压，物化成 pnpm 式 store：

```
web/wf_modules/
├── .store/
│   ├── react@18.3.1/
│   ├── @vue+runtime-dom@3.5.13/      ← scoped 包的 "/" 换成 "+"
│   ├── ...
│   └── manifest.json
│       { "root":     { "react": "18.3.1", ... },
│         "packages": { "@vue+compiler-core@3.5.13": { "@babel/parser": "7.29.11", ... } } }
```

解析规则：看**发起 import 的文件属于哪个包**，再查该包在 manifest 里声明的版本。这样 `a→react@17` 与 `b→react@18` 可以在同一次构建里共存。

示例工程实测：29 个包目录，`react/react-dom/vue/@vue/server-renderer/@vue/compiler-sfc` 5 个根依赖。

> **为什么不做依赖解析**：实测纯 Go 从零解析会把 `scheduler ^0.23.2` 错解析成 `0.28.0-canary`。版本解析交给 npm/pnpm 一次性生成 lockfile，纯 Go 只做"精确拉取 + 校验 + 解压"，替代 `npm install` 的下载角色。
>
> **现代 npm 包 `exports` 解析支持**：针对 Vue 3、Tailwind、React Router 等现代 ESM 包缺失根 `module` 或依赖条件导出的场景，解析器优先读取 `package.json` 中的 `exports` 规范（按 `"import" > "browser" > "module" > "default"` 优先级匹配条件），并支持精确子路径与通配符模式匹配（`./*`）。
>
> **16 协程并发下载加速**：`InstallStore` 采用带缓冲信号量的并发 Worker 池（限制 16 并发），大幅缩短海量依赖下载解压耗时。

### 4.7 SSG 预渲染（prerender.go）

```go
type Route struct {
    Path  string         `json:"path"`
    Props map[string]any `json:"props"`
}
func Prerender(bundle []byte, routes []Route, outDir, clientScript string) ([]string, error)
```

构建期用**同一份 `ssr.js`**（不重复构建）在引擎里跑一遍，写出 `dist/pages/<path>.html`。运行期不再需要引擎——这是体积从 15.62MB 降到 6.89MB 的唯一途径（−8.7MB）。

代价：页面内容在构建期固定，动态数据要靠客户端 hydration 补，或走 API。

---

## 5. JS 引擎层（package jsruntime）

### 5.1 单一引擎：goja

运行期只有一个引擎实现，`jsruntime` 内部没有 build tag 分叉（"要不要引擎"由更上层的 `-tags dynamic` 决定，见 §8.2）：

```go
func New(bundle []byte, size int) (*Pool, error)
func (p *Pool) RenderHTML(url string, propsJSON []byte) (html, head string, err error)
func (p *Pool) Close() error
```

`go list -deps` 实测：`jsruntime` 只依赖 `github.com/dop251/goja`（外加 `fmt`）。净链接成本 **+9.46 MB**，纯 Go、零 cgo。

> 曾经有过 go-quickjs 双后端（`-tags quickjs`），**已从代码移除**。它的价值只在"goja 撞上兼容性硬伤时兜底"，而实测 React 18 / Vue 3 的运行期产物、以及 `@vue/compiler-sfc` 的编译期产物都能在 goja 下跑通——双后端只是多维护一条路径、多一份 CI 组合。备选方案以**文档形式**保留在 §5.6。

### 5.2 为什么是 goja

| 维度 | goja（在用） | 完整 ES2023 引擎（备选，见 §5.6） |
| --- | --- | --- |
| 链接成本 | **+9.46 MB** | +26.38 MB |
| 纯度 | 纯 Go，`CgoFiles == []` | 纯 Go |
| ES 覆盖 | ES5.1 + ES6 主体，接近 ES2022 | ES2023 完整 |
| 实测跑通 | React 18 `renderToString`、Vue 3 SFC 编译 + 异步 `renderToString` | 同左 + private/`using`/顶层 await |
| 代价 | `async generator` / `Intl` 不可用 | 多 16.9 MB |

结论：**"能不能跑通真实框架"比"支持多少条 test262"更重要**。两个框架、两条链路（编译期 SFC + 运行期 SSR）都跑通后，16.9MB 的差价没有理由。

### 5.3 shim：宿主 API 补齐

goja 是纯语言引擎，没有 Web 宿主 API。`react-dom/server.browser` 会引用 `TextEncoder`，缺了直接 `ReferenceError`。shim 只补实际被用到的部分：

```js
if (typeof globalThis === 'undefined') { globalThis = this; }

// 手写 UTF-8 编码（含代理对），够 renderToString 用；流式 API 才会真的编码
function TextEncoder(){}
TextEncoder.prototype.encode = function(str){ /* ... */ return new Uint8Array(out); };
globalThis.TextEncoder = TextEncoder;

function TextDecoder(){}
TextDecoder.prototype.decode = function(){ return ""; };
globalThis.TextDecoder = TextDecoder;

if (typeof globalThis.queueMicrotask === 'undefined') {
  globalThis.queueMicrotask = function(f){ Promise.resolve().then(f); };
}
```

### 5.4 微任务收口与状态隔离

**统一收口**：React 的 `renderToString` 同步返回对象，Vue 的 `renderToString` 返回 Promise。用同一个脚本把两者都拍平：

```js
globalThis.__WF_OUT__ = null;
globalThis.__WF_ERR__ = null;
Promise.resolve(globalThis.__WF_APP__.render(__WF_URL__, __WF_INITIAL_PROPS__))
  .then(v => { __WF_OUT__ = v; }, e => { __WF_ERR__ = String((e && e.stack) || e); });
```

goja 在 `RunString` 结束时会把微任务队列跑干，所以脚本返回后直接读 `__WF_OUT__` 即可，无需自己驱动 event loop。

**状态隔离**（v0 草案里的 🟠 高风险项，已落地）：借出 VM → 注入 → 渲染 → **归还前必删全局**：

```go
defer func() {
    vm.RunString(`delete globalThis.__WF_INITIAL_PROPS__; delete globalThis.__WF_URL__; ` +
        `delete globalThis.__WF_OUT__; delete globalThis.__WF_ERR__;`)
    p.ch <- vm
}()
```

池用带缓冲 channel 实现，`cap = size`，天然保证"一台 VM 同一时刻只服务一个 goroutine"（borrow/return 语义），不需要额外锁。

**字节码预编译与启动加速**：
`New` / `NewWithTimeout` 在初始化阶段一次性将 `shim.js`、`ssr.js` 和收口脚本预编译为只读的 `*goja.Program`，每台 VM 启动无需重复解析 AST，大幅压低初始化延迟。

**请求超时熔断与坏 VM 自动重建**：
默认对单次渲染施加 5s 超时保护（支持 `SetTimeout`）。若前端业务代码发生死循环或阻塞，超时将触发 `vm.Interrupt` 强制中止。捕获到中断或异常 Panic 的 VM 会被直接废弃，并利用预编译的 Program 毫秒级重建全新健康的 VM 补充入池，防止资源泄露或 VM 池枯竭。

**字节码预编译与启动加速**：
`New` / `NewWithTimeout` 在初始化阶段一次性将 `shim.js`、`ssr.js` 和收口脚本预编译为只读的 `*goja.Program`，每台 VM 启动无需重复解析 AST，大幅压低初始化延迟。

**请求超时熔断与坏 VM 自动重建**：
默认对单次渲染施加 5s 超时保护（支持 `SetTimeout`）。若前端业务代码发生死循环或阻塞，超时将触发 `vm.Interrupt` 强制中止。捕获到中断或异常 Panic 的 VM 会被直接废弃，并利用预编译的 Program 毫秒级重建全新健康的 VM 补充入池，防止资源泄露或 VM 池枯竭。

### 5.5 兼容边界（实测 34 项 ES 特性）

| 类别 | 结论 |
| --- | --- |
| ✅ 支持 | class（含字段 / 私有字段 / 静态块 / getter / setter）、解构、spread、`Symbol`、`Proxy`、`WeakMap/WeakSet`、`Map/Set`、`Promise`（then/catch/finally/all/race）、`async/await`、模板字符串、可选链、空值合并、`Array.flat/flatMap`、`Object.entries/fromEntries`、`String.matchAll`、`globalThis` |
| ❌ 不支持 | async generator（`async function*`）、动态 `import()`、ES modules（`import/export` 语法）、`Intl`、部分 Web 宿主 API（`fetch`/`URL`/`Blob`/`Streams`） |
| 兜底手段 | `Target: api.ES2017` 让 esbuild 把新语法编译掉；SSR 产物是 IIFE，不涉及 ESM；真撞上降不下去的语法，按 §5.6 接回完整引擎 |

> 这解释了为什么"goja 的 ES6 子集不会给新框架不适配"：框架的**运行期产物**（已由 esbuild 打包降级）只用到上述 ✅ 部分；框架的**编译期工具**（`@vue/compiler-sfc`）实测也在 goja 下跑通，不受此限。

### 5.6 备选方案：完整 ES2023 引擎（仅文档，未接入代码）

如果哪天 goja 撞上硬伤（某个依赖在运行期硬用 `Intl`、`async function*` 等），接入成本被刻意压到最低——只需在 `jsruntime` 里新增一个文件实现 `Renderer`：

```go
type Pool struct{ ... }
func New(bundle []byte, size int) (*Pool, error)
func (p *Pool) RenderHTML(url string, propsJSON []byte) (string, string, error)
func (p *Pool) Close() error
```

上层（`webfront` / `compiler` / 示例程序）**一行都不用改**：`Config.Renderer` 收的就是这个接口，静态部署路径完全不受影响。

历史评估数据（2026-10-28 实测，代码已移除）：

| 项 | 数值 |
| --- | --- |
| 候选引擎 | go-quickjs v0.29.0（纯 Go、零 cgo、test262 99,637/0 fail） |
| 净链接成本 | **+26.38 MB**（对应二进制 33.14 MB） |
| 额外能力 | 完整 ES2023（private / `using` / 顶层 await）；`stdlib` 提供 URL / TextEncoder / Streams / Timers |
| 渲染结果 | 与 goja **逐字节一致**（同一路由、同一份 `assemble`） |

选它的唯一理由应是"goja 跑不了"，而不是"它更强"——多付的 16.9MB 换不到任何本项目已用到的能力。

---

## 6. 体积工程（全部实测）

### 6.1 三档体积矩阵

统一 `go build -ldflags "-s -w"`，同一份 `_examples/basic` 代码：

| 形态 | 构建命令 | 体积 | 运行期依赖 |
| --- | --- | --- | --- |
| **A 预渲染静态（SSG）** | `go build` | **6.89 MB** | 无 JS 引擎、无 esbuild |
| **B 动态 SSR** | `-tags dynamic` | **15.62 MB** | goja |
| **C 开发服务器** | `-tags dev` | **11.36 MB** | esbuild + fsnotify + ws |

A 与 B 相差 **2.3 倍**：同一份业务代码，能不能把渲染搬到构建期，就是 8.7MB 的差别。

> 曾经还有第四档"完整 ES2023 引擎形态"（33.14 MB），随引擎后端一起从代码移除；数据保留在 §5.6 备选方案里。

### 6.2 从 45MB 到 15.6MB：分包 + 换引擎的效果

初版把 `webfront` + `compiler` + `dev` + 一个 Gin 适配层全塞在一个包里，示例程序无条件 import 全部，再加上当时的完整 ES2023 引擎 → **45MB**。原因不是"Go 二进制大"，而是"把构建期工具、框架依赖和一台用不满的引擎一起链进了生产二进制"。

两步之后：拆包 → 33.14MB（同引擎）；换 goja → **15.62MB**。剩下的 15.6MB 里，9.5MB 是 goja，5.7MB 是 Go 运行时 + net/http，1.2MB 是核心包与嵌入产物——这已经是这个形态的下限。

### 6.3 客户端体积：252KB → 45KB

| 阶段 | 体积 | 累计优化 |
| --- | --- | --- |
| esbuild 默认输出 | 258,323 B（252 KB） | — |
| + minify（三层） | 142,541 B（139 KB） | −44.8% |
| + 代码分割 + gzip（运行时实测） | **46,096 B（45.0 KB）** | **−82.2%** |
| + ETag/304（二次访问） | 0 B | −100% |

> 归因插曲：初版 `client.js` 254KB 时用 metafile 查过，**90.9% 是 `react-dom.production.min.js`（229.4KB）**。诡异之处在于源文件本身已是压缩过的单行文件——esbuild 把它 pretty-print 回了 229KB。开 minify 后恢复原状。这类问题不看 metafile 是查不出来的。

### 6.4 优化手段清单

| 手段 | 节省 | 代价 |
| --- | --- | --- |
| 分包（按需 import） | −12 MB | 使用者需知道该 import 哪个包 |
| SSG 预渲染（不链引擎） | −8.7 MB | 页面内容构建期固定 |
| 选 goja 而非完整 ES2023 引擎 | −16.9 MB | 失去 async generator / Intl |
| `-ldflags "-s -w"` | −4.7 MB | 无栈符号，panic 栈变弱 |
| 客户端 minify + gzip | −207 KB | 无 |
| 代码分割 | 0（体积持平） | 换来缓存粒度 |
| `go:embed` 未被引用 | 由 linker 自动丢弃 | 无需干预 |

> 反例排除：评估完整引擎时，其 `go-intl` 依赖的 `data.pack` 有 18MB，一度怀疑是体积元凶。实测**未被链接**（只有真正调用 `Intl` 时才会链入），已随引擎后端一并移除。

---

## 7. 开发态（package dev）

### 7.1 DevServer 只做三件事

| 路由 | 行为 |
| --- | --- |
| `GET /` | 返回注入 HMR 客户端（含错误覆盖层）的 HTML 骨架 |
| `GET /src/*` | 读源文件 → `esbuild.Transform`（毫秒级）→ 返回 ESM |
| `GET /@deps/*` | 按需打包 npm 裸模块为 ESM 并内存缓存（配合 ImportMap 极速裸导入） |
| `GET /__wf_hmr` | 升级 WebSocket，推送变更事件 |
| `GET /__wf_ping` | 204 + `no-store`；Go 进程重启期间浏览器靠它判断"服务回来了没有" |
| `GET /__wf_error` | 最近一次编译错误（`{id,path,body,at}`）；`?since=N` 已是最新时 204 |

按需单文件转译的类型支持：

| 扩展名 | 处理 |
| --- | --- |
| `.ts` / `.tsx` / `.js` / `.jsx` | `api.Transform` → ESM，`process.env.NODE_ENV="development"` |
| `.css` | Transform 出 CSS 后包成"导入即注入 `<style>`"的 ESM 模块 |
| `.json` | `LoaderJSON` + `FormatESModule` → `export default {...}` |
| `.vue` | `compiler.CompileSFCResult`（SFC 编译 + 预处理器 + 作用域样式提取注入 + 预编译 VM 池） |

**扩展名补全**：浏览器不做解析，`import App from './App'` 会原样请求 `/src/App`。构建期由
esbuild 的 `resolveExtensions` 代劳，dev 期没有这层，所以 `readSrc` 自己按
「原路径 → 路径+`.tsx|.ts|.jsx|.js|.vue|.css|.json` → 路径/`index.*`」补全。
不补的话，任何带相对导入的工程一进 dev 就 404 —— 而且浏览器只会回一句
`Failed to fetch dynamically imported module`，看不出是哪个文件。

### 7.2 HMR 协议

服务端 → 客户端只有一种消息：

```json
{ "type": "update", "path": "/src/App.tsx" }
```

客户端收到后根据模块类型自适应处理：
1. **主入口模块**（包含 `render` 导出）：直接调用最新 `mod.render()` 实现原地热替换；
2. **`.css` 样式模块**：执行 `import(path + '?t=...')` 动态更新覆盖 DOM 中的 `<style>` 标签，无需整页刷新；
3. **普通子组件**（如 `App.tsx`、`App.vue`）：触发轻量整页刷新（依赖已被内存缓存，耗时 < 50ms），彻底穿透浏览器原生静态 ESM 缓存。
4. **编译失败**：`import()` reject，此时**不能刷新**（刷新只会白屏）。客户端回读那个 500 的响应体，把真正的错误盖在页面上，等下一次成功的 update 自动收场（见 §7.4）。

> 错误一律是 `error` 返回值而不是 panic：goja 的 JS 异常本来就以 `*goja.Exception` 返回，各阶段再包上文件名与阶段名（`parse` / `compileScript` / `compileTemplate` / `compileStyle` / 预处理器）；`CompileSFCResult` 与 `preprocessStyle` 外面还各有一层 `recover`，保证即使 VM 或宿主回调出了意外，也不会把 dev server / 构建进程带走。

**Windows 上的坑**：编辑器"改名式保存"（写 `.tmp` 再 rename）会先发 `Remove` 再发 `Create`。若在 `Remove` 时就广播，浏览器 `import()` 会撞上文件短暂不存在的 404。因此只监听 `Write|Create`，跳过 `Remove|Rename|Chmod`。

### 7.3 Go 侧热重载：Supervise（supervisor.go）

Go 没有热替换：进程里的机器码换不掉，`plugin` 只在 Linux 上可用且不能卸载。所以
"Go HMR"只能是**重编译 + 换进程**，工程上真正要解决的是"换得没有存在感"。

`wf dev` 的时序：

```
文件变更（fsnotify：*.go / go.mod / go.sum）
      │  debounce 120ms
      ▼
   go build -o <tmp>/app-<n>.exe .        ← 旧进程继续服务
      │
      ├─ 失败 → 打印编译器错误，保留旧进程，回到监听
      ▼ 成功
   kill 旧进程 → wait → 删掉旧二进制
      ▼
   启动新进程（继承 stdout/stderr，注入 WF_DEV=1）
```

四个关键取舍：

1. **先编译、后杀进程**。反过来会把一次语法错误变成"页面打不开"，这是开发体验最差的一步。
2. **每次编译到新的临时文件**。Windows 下运行中的 exe 不能覆盖、也不能改名；进程退出后立刻删，长时间开发不会堆出几十个 10MB 的二进制。
3. **`Remove` / `Rename` 也要响应**——和前端 HMR 正好相反：前端删文件不影响已加载的模块，而删掉一个 `.go` 会改变编译结果。
4. **子进程退出不终止监听**。panic / 端口占用之后，改好代码就能自动拉起，不用重开终端。

浏览器侧的配合：

| 环节 | 行为 |
| --- | --- |
| WebSocket 断开 | 600ms 后盖上覆盖层"后端重启中"（实测重启只有 300~500ms，太早弹层会闪） |
| 恢复探测 | 每 300ms `fetch('/__wf_ping')`；只把**网络错误**当作"还没起来" |
| 服务回来 | `location.reload()`，拿到新编译的后端与新 importmap |

用"任何 HTTP 响应都算活"而不是"必须 200"，是为了让自定义 404 页 / 反向代理下的应用也能正常恢复。

### 7.4 错误反馈：四类错误，一个前台的落点

Vite 之所以"好用"，很大一部分是**报错永远在你看得见的地方**。本项目对齐这条，落点是
`devui` 包（纯标准库，≈12KB）：

```
devui.overlayCSS      覆盖层与错误页共用的一套样式
devui.ClientScript()  注入 dev 页面的 window.__wfOverlay = { show, hide, kind }
devui.ErrorPage()     独立错误页（SSR/goja 失败时当响应体整页返回）
```

四类错误、四条通道：

| 来源 | 通道 | 前台表现 |
| --- | --- | --- |
| 前端编译失败（tsx/vue/css） | `import()` reject → 回读 500 响应体 → `__wfOverlay.show` | 红色"编译失败" + `文件:行:列` + 代码框 |
| 嵌套依赖坏了 | 直接请求该模块返回 200 → `GET /__wf_error?since=N` 取最近一条 | 同上，`file` 指向真正坏掉的文件 |
| 浏览器运行期异常 | `window.onerror` / `unhandledrejection` | "运行期错误" + JS 栈（栈帧压暗） |
| SSR / goja 渲染失败 | `webfront.Engine.writeRenderError` | 整页错误页（`Config.Dev` 或 `WF_DEV=1`），生产态只回干净 500 |
| Go 进程重启 | WebSocket `onclose` → 轮询 `/__wf_ping` | 琥珀色"后端重启中"，服务回来自动刷新 |

几个不显眼但必要的决定：

1. **Shadow DOM，不是普通 div**。宿主页面一句 `* { color: #fff }` 或 `body > div { display:none }`
   就能让错误看不见。覆盖层挂在 `<html>` 下、样式塞进 `attachShadow({mode:'open'})`，
   页面 CSS 改不动它，`z-index` 固定 `2147483647`。
2. **`/__wf_error` 是给"嵌套依赖"用的**。浏览器对 `import('./App.tsx')` 里再 import 的
   子模块失败，只会把错误算在**外层模块**头上，且消息固定是
   `Failed to fetch dynamically imported module` —— 完全无法定位。服务端在每次 500 / 404
   时记一条 `{id,path,body}`，客户端用 `?since=` 取回，于是能看到 `Broken.tsx:1:17` 的代码框。
3. **失败不 `reload()`**。这是最容易写错的一步：`location.reload()` 会让页面变白，
   用户以为整个应用挂了。正确做法是留在原地展示错误，等下一次成功的 HMR 自然收场。
4. **SSR 错误由引擎自己写响应**。调用方几乎都写成 `_ = eng.Render(w, r, page)`，
   只 `return err` 等于把错误扔了 —— 浏览器收到的是一个**空白的 200**。
   所以 `Render` 在返回 error 之前就把错误页写进 `ResponseWriter`。
5. **生产态不泄露栈**。`writeRenderError` 分两路：开发态给完整 goja 栈并 `log.Printf` 到终端，
   生产态只回 `webfront: 页面渲染失败（详情见服务端日志）`。

实测（Chromium，`_examples/basic`）：改坏 `main.tsx` → 覆盖层出现，标题"编译失败"、
副标题 `/src/main.tsx`、代码框带行号与插入符；`Esc` 可关；点"重新加载"仍在（错误还在）；
改回原文件 → 自动消失；`throw new Error('boom-runtime')` → "运行期错误" + 栈；
`Promise.reject` → "未处理的 Promise 拒绝"；`import './Broken'`（坏文件）→ 定位到
`/src/Broken.tsx` 而不是那句 `Failed to fetch`。

---

## 8. CLI 与构建矩阵

### 8.1 命令

```bash
wf build   [-modules <dir>] [-split=true] [-prerender routes.json] [rootDir]
wf install [-modules <dir>] [rootDir]
wf dev     [-tags dev] [-run "dev"] [-watch <dir>] [-ldflags "..."] [appDir]
```

- `-split`（默认 `true`）：客户端做代码分割（ESM + vendor chunk）；`-split=false` 退回单文件 IIFE。
- `-prerender`：读 `routes.json`，额外产出 `dist/pages/*.html`，运行期可零引擎。
- `-modules`：依赖目录名，默认 `node_modules`（示例用 `wf_modules`）。非 `node_modules` 时自动作为 esbuild 的 `NodePaths`（等价 `NODE_PATH` 语义）。
- `rootDir`：指**包含 `web/` 的目录**；直接指向 `web/` 目录也能用（两个子命令走同一套归一化，见 `webDirOf`）。
- `wf dev`：`appDir` 是要构建的 main 包；`-tags` 默认 `dev`、`-run` 默认 `dev`（即 `go run -tags dev . dev` 的等价物），`-watch` 默认取 `go.mod` 所在目录。它只负责"重编译 + 换进程"，前端 HMR 仍由应用内的 `dev.DevServer` 提供。

### 8.2 build tag 矩阵

| 构建 | 命令 | 说明 |
| --- | --- | --- |
| 静态 | `go build .` | `engine_static.go`（`!dynamic`），不 import jsruntime |
| 动态 | `go build -tags dynamic .` | `engine_dynamic.go` + `jsruntime/goja.go` |
| 开发 | `wf dev`（或 `go run -tags dev . dev`） | `dev_mode.go`；`dev_stub.go`（`!dev`）保证生产二进制无 esbuild |

三种组合均已 `go build` + `go vet` 通过。

依赖图干净度实测（`go list -deps | grep -c "gin-gonic|evanw/esbuild|fsnotify"`）：静态 / 动态两种生产构建**均为 0**。

### 8.3 生产发布流程

```bash
# 1) 构建前端产物（纯 Go，零 Node）。rootDir 是"包含 web/ 的那个目录"
go run ./cmd/wf install -modules wf_modules ./_examples/basic
go run ./cmd/wf build -modules wf_modules -prerender _examples/basic/web/routes.json ./_examples/basic

# 2) 打进单二进制（零 CGO，可交叉编译）
cd _examples/basic && CGO_ENABLED=0 go build -ldflags "-s -w" -o app .
```

`//go:embed all:web/dist` 要求产物先于编译存在，所以顺序不能反。用 Makefile 比 `go:generate go run <远程模块>` 稳（后者要求模块在构建图内）。

---

## 9. 前端约定

前端目录与标准 Vite 项目几乎一致，唯一规范是 SSR 入口导出 `render(url, props)`：

```tsx
// web/src/entry-server.tsx
import React from 'react'
import { renderToString } from 'react-dom/server.browser'
import App from './App'

export function render(url: string, props: Record<string, any>) {
  const html = renderToString(<App {...props} />)
  const head = `<title>${props?.title || 'webfront'}</title>`
  return { html, head }   // 同步返回
}
```

```tsx
// web/src/entry-server.vue.tsx —— 异步同样支持，库已统一 Promise 收口
export async function render(url: string, props: Record<string, any>) {
  const app = createSSRApp(App, props)
  const html = await renderToString(app)
  return { html, head: `<title>webfront Vue SSR</title>` }
}
```

客户端入口读 `__WF_DATA__` 完成真 Hydration（不是 `innerHTML` 覆盖）：

```tsx
const props = JSON.parse(document.getElementById('__WF_DATA__')?.textContent || '{}')
hydrateRoot(document.getElementById('root')!, <App {...props} />)
```

**为什么 `render(url, props)` 这个约定极关键**：若只做路由转发，SSR 阶段前端必须 `fetch` 自己的接口拿数据，凭空多一次 HTTP 回环 + Cookie/Token 透传麻烦。现在 Go 直接把 Props 塞进引擎内存，零网络开销，首屏无需再发 API 请求。

---

## 10. 已知限制与风险

| # | 严重度 | 限制 | 说明与缓解 |
| --- | --- | --- | --- |
| 1 | 🟢 已解 | ~~**dev 下 `.vue` 未接入**~~ | `dev/serveSrc` 已全面接入 `compiler.CompileSFC`，支持 SFC 模板、脚本、`<style>` / `<style scoped>` 提取并自动注入 DOM，HMR 广播实时热更。 |
| 2 | 🟡 中 | **goja 不支持 async generator / Intl** | 靠 `Target: ES2017` 兜底；若某依赖运行期硬用 `Intl`（如 i18n 格式化），按 §5.6 接回完整 ES2023 引擎（只需实现 `Renderer`，上层零改动）。 |
| 3 | 🟡 中 | **SSG 页面内容构建期固定** | 动态数据只能靠客户端 hydration 或 API。需要真动态 SSR 的场景请用 `-tags dynamic`。 |
| 4 | 🟢 低 | **`__WF_DATA__` 必须走 `json.Marshal`** | 已在 `assemble` 统一处理（默认转义 `<>&`）。**禁止**用 `fmt.Sprintf` 拼接。 |
| 5 | 🟢 低 | **流式 SSR 未实现** | `renderToPipeableStream` + Suspense 需要 Streams + async 协作，引擎里复杂。当前全部走同步 `renderToString`。 |
| 6 | 🟢 低 | **`ModeIslands` 仅占位** | 枚举值已定义，`client:load` / `client:visible` 指令尚未实现。 |
| 7 | 🟢 已解 | ~~**`ModeSPA` 仅占位**~~ | 已完整实现：静态资源直出 + `EmbedFS` 的 `index.html` History 路由 fallback 机制，单测通过。 |
| 8 | 🟢 低 | **（使用侧）Gin `*` 模式不匹配裸前缀** | 这是 Gin 的行为不是库的问题：`r.Any("/admin/*filepath")` 不含 `/admin`，需补精确路由或建 group。 |
| 9 | 🟢 低 | **未用 `-race` 检测** | `-race` 要求 cgo，与零 CGO 铁律冲突。并发安全靠"池单 VM 单 goroutine"语义保证，已用 4 VM / 500 请求压测零串台。 |
| 10 | 🟢 低 | **`wf dev` 的 Go 热更是重启进程** | Go 无法热替换机器码，重启窗口（2–3s）内请求会失败。只用于开发；线上靠多副本滚动更新，不依赖它。 |
| 11 | 🟢 低 | **SSR 错误页的栈指向打包产物** | goja 报的是 `ssr.js:1482:19`，不是 `App.tsx:12`。要精确到源码行，得给 SSR bundle 生成 source map 并做映射（esbuild 能出，尚未接）。生产构建不输出栈，所以不影响线上。 |
| 12 | 🟢 低 | **开发态错误页依赖 `Config.Dev` / `WF_DEV`** | 直接 `go run .` 起服务时只有干净的 500。`wf dev` 会自动注入 `WF_DEV=1`，这也是推荐用法。 |

---

## 11. 验证记录

### 11.1 构建与静态检查

| 项 | 结果 |
| --- | --- |
| `go build` × 3 组合（默认 / `dynamic` / `dev`） | ✅ 全通过 |
| `go vet` × 3 组合 | ✅ 全通过 |
| `CGO_ENABLED=0` 构建 | ✅ 通过（esbuild / goja / fsnotify / websocket 四个直接依赖 `CgoFiles` 均为 `[]`） |
| 依赖图干净度（esbuild / fsnotify / 任意 Web 框架计数） | ✅ 静态、动态两种生产构建均为 **0** |
| 库模块不依赖任何 Web 框架 | ✅ `go.mod` 仅 4 个直接依赖；间接依赖 32 → **5** |
| 全仓库无引擎后端残留 | ✅ `grep -ri quickjs` 源码零命中（仅文档 §5.6 保留备选记录） |

### 11.2 运行期行为

| 项 | 结果 |
| --- | --- |
| dev server `GET /` | ✅ 200 / 15,331 B（内联了错误覆盖层，见 §7.4；dev 不压缩，换取零依赖注入），含 `new WebSocket(...+'/__wf_hmr')` 与 `window.__wfOverlay` |
| dev server `GET /src/App.tsx` | ✅ 200 / 633 B，正确转译为 `React.createElement("div", { className: "app", "data-ssr": "true" }` |
| 动态 SSR `GET /` | ✅ 200 / 473 B / 毫秒级（goja 单次渲染 1.6–4.2 ms） |
| 静态模式（移走磁盘 `web/dist`，纯 embed） | ✅ `/` 200/473B、`/about` 200/477B、`/nope` 404、`client.js` 200/816B、chunk 200/45,279B（gzip） |
| Vue 3 SFC 编译 + SSR（Proxy + 异步 `renderToString`） | ✅ 全在 goja 下跑通（编译期 `@vue/compiler-sfc` 也在 goja 里执行） |
| 条件请求 | ✅ `If-None-Match` 命中返回 304 |

### 11.3 错误前台化（Chromium 实测）

| 场景 | 结果 |
| --- | --- |
| 改坏 `src/main.tsx`（`const broken = ;`） | ✅ 覆盖层弹出：标题"编译失败"、`/src/main.tsx`、代码框 `12 │ const broken = ;` + 红色插入符 |
| 按 `Esc` | ✅ 关闭 |
| 关掉后 `location.reload()` | ✅ 覆盖层重新出现（首屏 import 失败同样前台可见） |
| 点"重新加载" | ✅ 按钮生效 |
| 改回原文件 | ✅ 覆盖层自动消失，无需手动刷新 |
| `throw new Error('boom-runtime')` | ✅ "运行期错误" + `Error: boom-runtime` + 压暗的栈帧 |
| `Promise.reject(...)` | ✅ "未处理的 Promise 拒绝" |
| `import './Broken'`（`Broken.tsx` 有语法错） | ✅ `file` 定位到 `/src/Broken.tsx`，而不是 `Failed to fetch dynamically imported module` |
| SSR 渲染失败（goja 抛错） | ✅ 500 + `text/html` 错误页（标题"SSR 渲染失败"、goja 栈、`no-store`），终端同时打印 |
| 同上但生产态（`Dev=false`） | ✅ 500 + 一句话，栈不出现在响应体里 |
| 覆盖层被页面 CSS 影响 | ✅ 无（Shadow DOM + `z-index: 2147483647`） |

### 11.4 交叉验证：渲染输出逐字节一致

同一路由 `/` 由不同路径产出，用 `cmp` 比对**逐字节相同**：

```
goja 动态 SSR 输出   ==   wf build -prerender 产物（运行期静态服务）
```

（引擎后端移除前还做过一次三方交叉：`完整 ES2023 引擎 == goja == 预渲染产物`，三条路径同样逐字节一致——这也正是后来敢删掉那个后端、只留 goja 的依据。）

这是最关键的一条验证：它同时证明了 ① 构建期预渲染与运行期渲染走的是同一套 `assemble` 逻辑（否则 head/data/script 标签的拼装顺序必然漂移）；② 静态部署可以放心替换动态 SSR；③ 换引擎不会改变任何字节输出。

---

## 12. 关键设计取舍

- **包 vs 框架**：选 Go 包 + 极薄 CLI，且**不绑定任何框架**。用户已有自己的路由，库补前端短板而非替换；框架级约定（文件路由等）可日后在包之上叠加。
- **按需付费分包**：核心包只许 import stdlib，且不内置任何框架适配。这是"同一份代码 6.89MB ↔ 15.62MB"的前提，比任何单点优化都有效。
- **`Renderer` 接口而非 `SSRBundle []byte`**：把"执行 JS"从引擎里解耦，静态部署才能不链引擎；同时让引擎可替换、让预渲染复用同一套拼装代码。
- **只留 goja，砍掉双后端**：两个引擎都纯 Go，差价 16.9MB。"能跑通真实框架"优先于"test262 分数"，框架运行期产物经 esbuild 降级后不需要新语法。双后端只增加维护面，价值仅在"万一 goja 不行"——那属于文档里的备选方案，不属于主干代码。
- **`Target: ES2017` 只作用于 SSR**：为引擎降级，不为浏览器降级。代价 0.5%，换来引擎不必追新语法。
- **代码分割用两遍构建**：esbuild 无 `manualChunks`，与其 fork 或换打包器，不如用虚拟 vendor 入口把共享依赖逼出来。收益是缓存粒度而非体积。
- **SSG 而非"模板引擎式 SSR"**：想在运行期把体积压到模板引擎级别，就得放弃通用 JS 执行——那等于不支持 React/Vue。真正的答案是把渲染搬到构建期（SSG），运行期只发静态文件。
- **lockfile 驱动安装**：不重造依赖解析。实测纯 Go 解析会把 `scheduler ^0.23.2` 错解成 `0.28.0-canary`。
- **引擎内核基于 `http.Handler`**：框架无关，Gin/Echo/Chi/net/http 通用。**适配器不进库**——`gin.WrapH` / `echo.WrapHandler` 都是框架自带的，库为此付出 30 个间接依赖不划算（§3.6）。

---

## 13. Roadmap

- **v1.1（补齐开发态与 SPA）**
  - [x] `dev/serveSrc` 接入 `@vue/compiler-sfc` 与 `importmap` 裸模块加载，消除 `.vue` 的 501。
  - [x] `ModeSPA` 落地：History 路由 fallback + `EmbedFS` 的 `index.html`。
  - 单文件转译结果加内存缓存（当前每次请求都重新 Transform）。
- **v1.2（内容与孤岛）**
  - 集成 `goldmark`，支持 `web/content/*.md` + Frontmatter 注入 Props。
  - `ModeIslands`：识别 `client:load` / `client:visible`，服务端只出纯静态 HTML，仅对标记组件生成微型 hydration 脚本。
- **v1.3（性能）**
  - 流式 SSR（`renderToPipeableStream` + Suspense），需要引擎侧 Streams 支持。
  - 预渲染并行化（当前单 goroutine 串行跑路由）。

---

## 14. 环境

- Go **1.27.0**（windows/amd64 实测；全部代码仅用标准库 + 4 个纯 Go 依赖）。
- 直接依赖共 4 个，全部纯 Go 无 cgo：`esbuild`（构建期 + 开发态）、`goja`（动态 SSR）、`fsnotify` + `gorilla/websocket`（开发态）。**不含任何 Web 框架**——Gin / Echo / Chi 由使用者在自己的模块里引入。
- 零 CGO 为前提，`go build` 无需 C 工具链，任意平台交叉编译。
