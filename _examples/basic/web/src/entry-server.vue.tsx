// Vue SSR 入口：演示 webfront 库（框架无关）同样能渲染 Vue。
// 构建期经 vue.go 的 esbuild 插件把 App.vue 编译成 JS；运行期在 goja 里
// 用 vue + @vue/server-renderer 渲染，renderToString 返回 Promise（库已统一 await）。
import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from './App.vue'

export async function render(url: string, props: Record<string, any>) {
  const app = createSSRApp(App, props)
  const html = await renderToString(app)
  const head = `<title>webfront Vue SSR</title>`
  return { html, head }
}
