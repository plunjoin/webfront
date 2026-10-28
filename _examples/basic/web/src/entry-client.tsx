// 生产态客户端入口：真·Hydration（非 innerHTML 覆盖）。
// 读取 SSR 注入的 __WF_DATA__ 作为同款 Props，用 React 18 hydrateRoot 水合。
// 服务端渲染标记与客户端必须一致（见 entry-server.tsx 的 <App {...props}/>）。
import { hydrateRoot } from 'react-dom/client'
import App from './App'

const el = document.getElementById('__WF_DATA__')
const props = el ? JSON.parse(el.textContent || '{}') : {}
hydrateRoot(document.getElementById('root')!, <App {...props} />)
