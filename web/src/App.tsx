import { useState } from 'react'
import { Routes, Route, useLocation, useNavigate } from 'react-router-dom'
import { motion } from 'framer-motion'
import { Icon } from './components/Icon'
import Dashboard from './pages/Dashboard'
import Channels from './pages/Channels'
import UsageBoard from './pages/UsageBoard'
import Placeholder from './pages/Placeholder'

// 顶部导航项：path / 标题 / 线性图标
const NAV_ITEMS = [
  { path: '/', label: '总览', icon: 'dashboard' },
  { path: '/channels', label: '渠道管理', icon: 'tag' },
  { path: '/usage', label: '用量看板', icon: 'chart' },
  { path: '/access-keys', label: 'AccessKey', icon: 'key' },
  { path: '/system', label: '系统状态', icon: 'activity' },
  { path: '/logs', label: '请求日志', icon: 'list' },
]

export default function App() {
  const location = useLocation()
  const navigate = useNavigate()
  const [refreshing, setRefreshing] = useState(0)

  // 点击刷新：广播一个自增计数（各页面监听后重新拉数据）
  const onRefresh = () => setRefreshing((n) => n + 1)

  return (
    <div className="app-shell">
      {/* ===== 顶部导航（React Bits 式：透明 + 文字链接 + 细线指示） ===== */}
      <nav className="topnav">
        <span className="nav-brand" onClick={() => navigate('/')}>
          <span className="nav-logo">G</span>
          <span className="nav-brand-text">Gateway</span>
        </span>
        {NAV_ITEMS.map((item) => {
          const active =
            item.path === '/' ? location.pathname === '/' : location.pathname.startsWith(item.path)
          return (
            <span
              key={item.path}
              className={`nav-item${active ? ' active' : ''}`}
              onClick={() => navigate(item.path)}
            >
              {active && (
                <motion.span
                  className="nav-line"
                  layoutId="nav-line"
                  transition={{ type: 'spring', stiffness: 500, damping: 34 }}
                />
              )}
              <span style={{ position: 'relative', display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                <Icon name={item.icon} size={13} />
                {item.label}
              </span>
            </span>
          )
        })}
        <span className="nav-spacer" />
        <button
          className="icon-btn round"
          title="刷新数据"
          onClick={(e) => {
            const el = e.currentTarget
            el.classList.remove('spinning')
            void el.offsetWidth
            el.classList.add('spinning')
            onRefresh()
          }}
        >
          <Icon name="refresh" size={15} />
        </button>
      </nav>

      {/* ===== 页面路由 ===== */}
      <div className="page-enter" key={location.pathname}>
        <Routes location={location}>
          <Route path="/" element={<Dashboard refreshTick={refreshing} />} />
          <Route path="/channels" element={<Channels refreshTick={refreshing} />} />
          <Route path="/usage" element={<UsageBoard refreshTick={refreshing} />} />
          <Route path="/access-keys" element={<Placeholder title="AccessKey 管理" desc="后端预留：access_keys 表 + Bearer 校验中间件，实现后此页展示签发 / 启停 / 复制。" />} />
          <Route path="/system" element={<Placeholder title="系统状态" desc="展示限流 / 熔断 / 健康探测等运行配置（后续从 /api/system/status 读取）。" />} />
          <Route path="/logs" element={<Placeholder title="请求日志" desc="预留：request_logs 表 + 列表 API，支撑首字延迟与排障。" />} />
        </Routes>
      </div>
    </div>
  )
}
