// 极薄 CLI：wf build / wf install / wf dev —— 纯 Go 调 esbuild + lockfile 安装器。
// 不参与运行期，不违反"单进程/不跑俩 service"。
//
// 逻辑都在 github.com/plunjoin/webfront/cli，这里只负责把退出码交还给操作系统。
// 第三方应用想在自己的 tools/ 里固定一个同版本入口，照抄这两行即可。
package main

import (
	"os"

	"github.com/plunjoin/webfront/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
