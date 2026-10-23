import { useCallback, useEffect, useRef, useState } from 'react'
import * as echarts from 'echarts'

/**
 * ECharts 最小封装（修复版）：
 * 图表容器可能是条件渲染（loading 结束后才出现），所以不能用普通 ref + mount 时 init——
 * 改用 callback ref：容器真正挂载时才 init 图表，卸载时 dispose。
 * option 变化时 setOption，窗口变化时自适应。
 */
export function useECharts(option: echarts.EChartsOption | null) {
  const chartRef = useRef<echarts.ECharts | null>(null)
  const [el, setEl] = useState<HTMLDivElement | null>(null)

  // 容器挂载/卸载时 init / dispose
  useEffect(() => {
    if (!el) return
    const chart = echarts.init(el)
    chartRef.current = chart
    const onResize = () => chart.resize()
    window.addEventListener('resize', onResize)
    return () => {
      window.removeEventListener('resize', onResize)
      chart.dispose()
      chartRef.current = null
    }
  }, [el])

  // option 变化时渲染（数据到达后图表才有内容）
  useEffect(() => {
    if (option && chartRef.current) {
      chartRef.current.setOption(option, { notMerge: true })
    }
  }, [option, el])

  const ref = useCallback((node: HTMLDivElement | null) => {
    setEl(node)
  }, [])

  return ref
}

/** 图表通用暗色主题（黑紫面板内的深色图表区） */
export const chartTextColor = 'rgba(244,244,248,0.72)'
export const chartAxisColor = 'rgba(244,244,248,0.14)'
