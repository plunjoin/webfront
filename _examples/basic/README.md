# basic —— 三形态最小示例

同一份前端工程（`web/`），用 build tag 切出三种运行形态：

| 形态 | 命令 | 体积 | 说明 |
| --- | --- | --- | --- |
| 预渲染静态（SSG） | `go build .` | 6.89 MB | 不链接 JS 引擎，只发预生成的 HTML |
| 动态 SSR | `go build -tags dynamic .` | 15.62 MB | 带 goja，每请求实时渲染 |
| 开发服务器 | `wf dev`（或 `go run -tags dev . dev`） | 11.36 MB | esbuild 单文件转译 + WebSocket HMR；改 `.go` 自动重启 |

## 先构建前端产物

产物不进版本库（`dist/` 里只有 `.gitkeep` 占位，好让 `go build` 在没有产物时也能通过）。
在**仓库根目录**执行：

```bash
go run ./cmd/wf install -modules wf_modules ./_examples/basic
go run ./cmd/wf build   -modules wf_modules \
    -prerender _examples/basic/web/routes.json ./_examples/basic
```

两条命令都是纯 Go：`install` 读 `web/package-lock.json` 拉依赖到 `web/wf_modules/.store/`，
`build` 调 esbuild 出 `web/dist/{ssr.js, assets/*, pages/*.html}`。

## 再按形态构建

```bash
cd _examples/basic

go build -ldflags "-s -w" -o app.exe .                 # 预渲染静态
go build -tags dynamic -ldflags "-s -w" -o app.exe .   # 动态 SSR
go run -tags dev . dev                                 # 开发模式（改 web/src 即热更）

./app.exe    # → http://localhost:8080
```

开发时更推荐 `wf dev`（在仓库根执行 `go run ./cmd/wf dev ./_examples/basic`）：
它比 `go run -tags dev . dev` 多了一层——**改 `.go` 会自动重新编译并重启进程**，
浏览器在重启完成后自己刷新，不用手动 Ctrl-C 再起一次。

> 忘了先构建产物也不会崩：程序启动时会提示缺哪一步，`/` 返回 404。

## 目录

```
_examples/basic/
├── main.go              http 入口 + //go:embed all:web/dist
├── engine_static.go     //go:build !dynamic   —— 不 import jsruntime
├── engine_dynamic.go    //go:build dynamic    —— jsruntime.New(ssr.js, 4)
├── dev_mode.go          //go:build dev        —— dev.NewDevServer
├── dev_stub.go          //go:build !dev       —— 生产构建下的桩，保证不含 esbuild
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
