# basic —— 四形态最小示例

同一份前端工程（`web/`），用 build tag + 构建产物切出四种运行形态：

| 形态 | 命令 | 体积 | 说明 |
| --- | --- | --- | --- |
| 预渲染静态（SSG） | `wf build` + `go build .` | 6.89 MB | 不链接 JS 引擎，只发预生成的 HTML |
| 纯客户端 SPA | `wf build -mode spa` + `go build .` | 6.89 MB | 不链接 JS 引擎，`index.html` + History fallback |
| 动态 SSR | `wf build` + `go build -tags dynamic .` | 15.62 MB | 带 goja，每请求实时渲染 |
| 开发服务器 | `wf dev`（`go run -tags dev .` 只有前端 HMR） | 11.36 MB | esbuild 单文件转译 + WebSocket HMR；`wf dev` 额外负责改 `.go` 自动重编译重启 |

`main.go` 不再手写 `Mode`：有 Renderer 就是动态 SSR；`dist/pages/` 存在就是静态；只有 `index.html` 就是 SPA。

## 先构建前端产物

产物不进版本库（`dist/` 里只有 `.gitkeep` 占位，好让生产 `go build` 在没有产物时也能通过；
`-tags dev` 构建根本不展开 embed，见下面的 `embed_dev.go`）。
在**仓库根目录**执行：

```bash
go run ./cmd/wf install ./_examples/basic
go run ./cmd/wf build   ./_examples/basic           # SSG / 动态 SSR 产物
# 或者只建纯客户端 SPA：
go run ./cmd/wf build -mode spa ./_examples/basic   # 只有 index.html + assets/
```

两条命令都是纯 Go：`install` 读 `web/package-lock.json` 拉依赖到 `web/wf_modules/.store/`，
`build` 调 esbuild 出 `web/dist/{ssr.js, assets/*, pages/*.html}`。

> 在你自己的项目里不需要克隆这个仓库：`go get -tool github.com/plunjoin/webfront/cmd/wf`
> 之后在项目根目录直接 `go tool wf install` / `go tool wf build`（依赖目录与 routes.json
> 自动探测），版本随你的 `go.mod` 走。仓库内的 `go run ./cmd/wf` 只是开发库自身时的等价写法。

## 再按形态构建

```bash
cd _examples/basic

go build -ldflags "-s -w" -o app.exe .                 # 预渲染静态 / 纯 SPA（看 dist 形态）
go build -tags dynamic -ldflags "-s -w" -o app.exe .   # 动态 SSR
go run -tags dev .                                     # 开发模式（改 web/src 即热更）

./app.exe    # → http://localhost:8080
```

开发时更推荐 `wf dev`（在仓库根执行 `go run ./cmd/wf dev ./_examples/basic`）：
它比 `go run -tags dev .` 多了一层——**改 `.go` 会自动重新编译并重启进程**，
浏览器在重启完成后自己刷新，不用手动 Ctrl-C 再起一次。

接线只有 `dev.Handler` 一处：

```go
mux.Handle("/", dev.Handler("./web", prodHandler))
```

普通构建原样返回 `prodHandler()`；`-tags dev` 构建由 `./web` 上的 DevServer 接管，
`prodHandler` 不执行。`//go:embed all:web/dist` 拆在 `embed.go`（`!dev`）与
`embed_dev.go`（空 FS）里，所以 `go build -tags dev` 不需要先构建 `dist/`。

> 默认静态 / SPA 构建忘了先构建产物也不会崩：启动时会提示缺哪一步，`/` 返回 404。
> `-tags dynamic` 缺 `web/dist/ssr.js` 时会在启动阶段直接报错退出（没有脚本可执行）。

## 目录

```
_examples/basic/
├── main.go              http 入口 + dev.Handler 接线（无 build tag）
├── embed.go             //go:build !dev       —— //go:embed all:web/dist
├── embed_dev.go         //go:build dev        —— 空 embed.FS（dev 不要求 dist）
├── engine_static.go     //go:build !dynamic   —— 不 import jsruntime
├── engine_dynamic.go    //go:build dynamic    —— jsruntime.New(ssr.js, 4)
└── web/
    ├── package.json / package-lock.json       —— 依赖清单（lockfile 驱动安装）
    ├── routes.json                            —— 预渲染路由 + Props
    └── src/
        ├── entry-server.tsx                   —— 导出 render(url, props)
        ├── entry-server.vue.tsx               —— 同一约定的 Vue 版本
        ├── entry-client.tsx                   —— hydrateRoot + __WF_DATA__
        ├── App.tsx / App.vue                  —— 组件
        └── main.tsx                           —— 开发态入口
```

## 为什么目录叫 `_examples` 而不是 `examples`

Go 工具链会跳过 `_` / `.` 开头的目录，所以 `go build ./...`、`go vet ./...` 只覆盖库本身，
不会因为示例依赖 `web/dist`（构建产物）而报错。在目录内部 `go build .` 照常工作。
