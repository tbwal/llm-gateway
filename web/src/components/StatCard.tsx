import { useEffect, useRef, useState } from 'react'
import { Icon } from './Icon'

/**
 * 统计卡：黑紫玻璃卡片 + 悬停上浮 + 数值滚动增长动效
 * 数字从 0 滚动到目标值（rAF 实现，约 900ms ease-out）
 */
export default function StatCard({
  label,
  value,
  suffix,
  sub,
  icon,
  decimals = 0,
}: {
  label: string
  value: number
  suffix?: string
  sub?: string
  icon: string
  decimals?: number
}) {
  const [display, setDisplay] = useState(0)
  const fromRef = useRef(0)
  const rafRef = useRef(0)

  useEffect(() => {
    const target = value
    const from = fromRef.current
    const duration = 900
    const start = performance.now()
    const tick = (now: number) => {
      const p = Math.min((now - start) / duration, 1)
      const eased = 1 - Math.pow(1 - p, 3)
      const v = from + (target - from) * eased
      setDisplay(v)
      if (p < 1) {
        rafRef.current = requestAnimationFrame(tick)
      } else {
        fromRef.current = target
      }
    }
    rafRef.current = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(rafRef.current)
  }, [value])

  return (
    <div className="glass stat-card">
      <span className="stat-icon">
        <Icon name={icon} size={15} />
      </span>
      <div className="stat-label">{label}</div>
      <div className="stat-value">
        {decimals > 0 ? display.toFixed(decimals) : Math.round(display).toLocaleString('zh-CN')}
        {suffix && <span style={{ fontSize: '15px', marginLeft: '4px' }}>{suffix}</span>}
      </div>
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  )
}
