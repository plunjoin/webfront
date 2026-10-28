// 客户端入口：纯 TSX（无框架，便于展示 HMR 机制）。
// 导出 render()，由 HMR 客户端在首屏与每次热更新时调用。
export function render() {
  const root = document.getElementById('root')!
  root.innerHTML = `
    <h1>Hello from webfront HMR 👋</h1>
    <p>编辑 <code>src/main.tsx</code> 并保存，页面会热更新（不刷新）。</p>
    <p>渲染时刻：<code>${new Date().toLocaleTimeString()}</code></p>
  `
}
