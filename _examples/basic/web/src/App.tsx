import React from 'react'

export default function App({ name, path }: { name?: string; path?: string }) {
  return (
    <div className="app" data-ssr="true">
      <h1>Hello {name || 'Stranger'} 👋</h1>
      <p>本页由 <strong>webfront</strong>（纯 Go + goja）在服务端渲染。</p>
      <p>请求路径1：<code>{path}</code></p>
    </div>
  )
}
