import React from 'react'
import { renderToString } from 'react-dom/server.browser'
import App from './App'

// 约定：导出 render(url, props)，由 JS 引擎池调用（见 ARCHITECTURE.md §9）
//
// 注意：#root 挂载容器由 SSR 自己输出。webfront 的 assemble 只拼
// `head + __WF_DATA__ + html`，不会额外包挂载点；而 entry-client.tsx 用
// `hydrateRoot(document.getElementById('root'), …)` 水合，两边必须对齐。
export function render(url: string, props: Record<string, any>) {
  const html = renderToString(
    <div id="root">
      <App {...props} />
    </div>
  )
  const head = `<title>${props?.title || 'webfront'}</title>`
  return { html, head }
}
