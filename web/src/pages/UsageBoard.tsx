import { useCallback, useEffect, useMemo, useState } from 'react'
import * as echarts from 'echarts'
import { motion } from 'framer-motion'
import { fetchDaily, fetchBreakdown, fmtCost, fmtNum, type DailyRow, type BreakdownRow } from '../api'
import { useECharts, chartTextColor, chartAxisColor } from '../hooks/useECharts'

const DAYS_OPTIONS = [7, 30, 90]
const GROUP_OPTIONS = [
  { value: 'model', label: '按模型' },
  { value: 'channel', label: '按渠道' },
] as const

export default function UsageBoard({ refreshTick }: { refreshTick: number }) {
  const [days, setDays] = useState(7)
  const [group, setGroup] = useState<'model' | 'channel'>('model')
  const [daily, setDaily] = useState<DailyRow[]>([])
  const [breakdown, setBreakdown] = useState<BreakdownRow[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  const load = useCallback(async () => {
    try {
      setErr('')
      const [d, b] = await Promise.all([fetchDaily(days), fetchBreakdown(group, days)])
      setDaily(d.rows)
      setBreakdown(b.rows)
    } catch (e) {
      setErr(e instanceof Error ? e.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [days, group])

  useEffect(() => {
    load()
  }, [load, refreshTick])

  // 按天趋势：请求数 + 费用 双轴
  const trendOption = useMemo<echarts.EChartsOption | null>(() => {
    if (daily.length === 0) return null
    return {
      backgroundColor: 'transparent',
      tooltip: { trigger: 'axis' },
      legend: {
        data: ['请求数', '费用'],
        textStyle: { color: chartTextColor, fontSize: 12 },
        top: 0,
      },
      grid: { left: 12, right: 12, top: 30, bottom: 8, containLabel: true },
      xAxis: {
        type: 'category',
        data: daily.map((r) => r.date),
        axisLine: { lineStyle: { color: chartAxisColor } },
        axisLabel: { color: chartTextColor },
      },
      yAxis: [
        {
          type: 'value',
          name: '请求数',
          nameTextStyle: { color: chartTextColor },
          axisLabel: { color: chartTextColor },
          splitLine: { lineStyle: { color: chartAxisColor } },
        },
        {
          type: 'value',
          name: '费用',
          nameTextStyle: { color: chartTextColor },
          axisLabel: { color: chartTextColor },
          splitLine: { show: false },
        },
      ],
      series: [
        {
          name: '请求数',
          type: 'line',
          smooth: true,
          symbolSize: 5,
          data: daily.map((r) => r.requests),
          itemStyle: { color: '#a78bfa' },
          areaStyle: { color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(139,92,246,0.3)' },
            { offset: 1, color: 'rgba(139,92,246,0)' },
          ]) },
        },
        {
          name: '费用',
          type: 'line',
          smooth: true,
          yAxisIndex: 1,
          symbolSize: 5,
          data: daily.map((r) => r.cost),
          itemStyle: { color: '#e879f9' },
        },
      ],
    }
  }, [daily])

  const trendRef = useECharts(trendOption)

  // 分组柱状（拆两张单轴图，避免双轴量纲不同导致的视觉误导）：
  // ① 请求数：柱子高度 = 真实请求数
  const reqOption = useMemo<echarts.EChartsOption | null>(() => {
    if (breakdown.length === 0) return null
    const axis = {
      type: 'category' as const,
      data: breakdown.map((r) => r.key),
      axisLabel: {
        color: chartTextColor,
        interval: 0,
        rotate: breakdown.length > 4 ? 20 : 0,
        formatter: (v: string) => (v.length > 8 ? `${v.slice(0, 8)}…` : v),
      },
      axisLine: { lineStyle: { color: chartAxisColor } },
    }
    return {
      backgroundColor: 'transparent',
      tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
      grid: { left: 12, right: 12, top: 16, bottom: 8, containLabel: true },
      xAxis: axis,
      yAxis: [
        {
          type: 'value',
          name: '请求数',
          nameTextStyle: { color: chartTextColor, fontSize: 11 },
          axisLabel: { color: chartTextColor },
          splitLine: { lineStyle: { color: chartAxisColor } },
        },
      ],
      series: [
        {
          name: '请求数',
          type: 'bar',
          data: breakdown.map((r) => r.requests),
          itemStyle: { borderRadius: [5, 5, 0, 0], color: '#8b5cf6' },
          barMaxWidth: 26,
        },
      ],
    }
  }, [breakdown])

  // ② 总 Tokens：柱子高度 = 真实 tokens
  const tokOption = useMemo<echarts.EChartsOption | null>(() => {
    if (breakdown.length === 0) return null
    return {
      backgroundColor: 'transparent',
      tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
      grid: { left: 12, right: 12, top: 16, bottom: 8, containLabel: true },
      xAxis: {
        type: 'category',
        data: breakdown.map((r) => r.key),
        axisLabel: {
          color: chartTextColor,
          interval: 0,
          rotate: breakdown.length > 4 ? 20 : 0,
          formatter: (v: string) => (v.length > 8 ? `${v.slice(0, 8)}…` : v),
        },
        axisLine: { lineStyle: { color: chartAxisColor } },
      },
      yAxis: [
        {
          type: 'value',
          name: '总 Tokens',
          nameTextStyle: { color: chartTextColor, fontSize: 11 },
          axisLabel: { color: chartTextColor },
          splitLine: { lineStyle: { color: chartAxisColor } },
        },
      ],
      series: [
        {
          name: '总 Tokens',
          type: 'bar',
          data: breakdown.map((r) => r.total_tokens),
          itemStyle: { borderRadius: [5, 5, 0, 0], color: '#c084fc' },
          barMaxWidth: 26,
        },
      ],
    }
  }, [breakdown])

  const reqRef = useECharts(reqOption)
  const tokRef = useECharts(tokOption)

  const totalCost = useMemo(() => breakdown.reduce((s, r) => s + r.cost, 0), [breakdown])
  const totalReq = useMemo(() => breakdown.reduce((s, r) => s + r.requests, 0), [breakdown])

  return (
    <div>
      <h2>用量看板</h2>
      <p className="subtitle">按时间窗口聚合 Token 消耗与费用 · 数据源 /api/usage/daily、/api/usage/breakdown</p>

      {/* 筛选条：玻璃胶囊 */}
      <div className="glass" style={{ display: 'inline-flex', alignItems: 'center', gap: 4, padding: 6, marginBottom: 18 }}>
        {DAYS_OPTIONS.map((d) => (
          <button
            key={d}
            onClick={() => setDays(d)}
            style={{
              position: 'relative',
              padding: '7px 18px',
              borderRadius: 12,
              border: 'none',
              background: 'transparent',
              color: days === d ? '#fff' : 'var(--text-secondary)',
              cursor: 'pointer',
              fontSize: 13,
            }}
          >
            {days === d && (
              <motion.span
                layoutId="days-pill"
                style={{ position: 'absolute', inset: 0, borderRadius: 12, background: 'rgba(99,102,241,0.4)', border: '1px solid rgba(255,255,255,0.2)' }}
                transition={{ type: 'spring', stiffness: 400, damping: 32 }}
              />
            )}
            <span style={{ position: 'relative' }}>近 {d} 天</span>
          </button>
        ))}
        <span style={{ width: 1, height: 22, background: 'rgba(255,255,255,0.15)', margin: '0 8px' }} />
        {GROUP_OPTIONS.map((g) => (
          <button
            key={g.value}
            onClick={() => setGroup(g.value)}
            style={{
              position: 'relative',
              padding: '7px 18px',
              borderRadius: 12,
              border: 'none',
              background: 'transparent',
              color: group === g.value ? '#fff' : 'var(--text-secondary)',
              cursor: 'pointer',
              fontSize: 13,
            }}
          >
            {group === g.value && (
              <motion.span
                layoutId="group-pill"
                style={{ position: 'absolute', inset: 0, borderRadius: 12, background: 'rgba(168,85,247,0.4)', border: '1px solid rgba(255,255,255,0.2)' }}
                transition={{ type: 'spring', stiffness: 400, damping: 32 }}
              />
            )}
            <span style={{ position: 'relative' }}>{g.label}</span>
          </button>
        ))}
        <span style={{ marginLeft: 'auto', padding: '0 10px', fontSize: 12.5, color: 'var(--text-secondary)', fontVariantNumeric: 'tabular-nums' }}>
          合计 {fmtNum(totalReq)} 请求 · {fmtCost(totalCost)}
        </span>
      </div>

      {err && (
        <div className="glass panel" style={{ borderColor: 'rgba(248,113,113,.5)', marginBottom: 16 }}>
          ⚠️ 加载失败：{err}
        </div>
      )}

      {loading && !err ? (
        <div className="glass panel" style={{ padding: 40, textAlign: 'center', color: 'var(--text-secondary)' }}>加载中…</div>
      ) : (
        <>
          <div className="dash-grid">
            <div className="glass panel">
              <div className="panel-title">每日趋势（请求数 / 费用）</div>
              <div className="chart-box" ref={trendRef} />
            </div>
            <div className="glass panel">
              <div className="panel-title">分组统计</div>
              <div className="chart-box" style={{ height: 150 }} ref={reqRef} />
              <div className="chart-box" style={{ height: 150 }} ref={tokRef} />
            </div>
          </div>

          {/* 明细表 */}
          <div className="glass panel" style={{ marginTop: 16 }}>
            <div className="panel-title">明细（{group === 'model' ? '按模型' : '按渠道'}）</div>
            {breakdown.length === 0 ? (
              <div className="muted" style={{ padding: '24px 0', textAlign: 'center' }}>暂无数据</div>
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
                  <thead>
                    <tr style={{ color: 'var(--text-muted)', textAlign: 'left', fontSize: 12 }}>
                      <th style={{ padding: '6px 10px' }}>{group === 'model' ? '模型' : '渠道'}</th>
                      <th style={{ padding: '6px 10px', textAlign: 'right' }}>请求数</th>
                      <th style={{ padding: '6px 10px', textAlign: 'right' }}>Prompt Tokens</th>
                      <th style={{ padding: '6px 10px', textAlign: 'right' }}>Completion Tokens</th>
                      <th style={{ padding: '6px 10px', textAlign: 'right' }}>总 Tokens</th>
                      <th style={{ padding: '6px 10px', textAlign: 'right' }}>费用</th>
                    </tr>
                  </thead>
                  <tbody>
                    {breakdown.map((r) => (
                      <tr key={r.key} style={{ borderTop: '1px solid rgba(255,255,255,0.06)' }}>
                        <td style={{ padding: '9px 10px' }}>{r.key}</td>
                        <td style={{ padding: '9px 10px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtNum(r.requests)}</td>
                        <td style={{ padding: '9px 10px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtNum(r.prompt_tokens)}</td>
                        <td style={{ padding: '9px 10px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtNum(r.completion_tokens)}</td>
                        <td style={{ padding: '9px 10px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtNum(r.total_tokens)}</td>
                        <td style={{ padding: '9px 10px', textAlign: 'right', fontVariantNumeric: 'tabular-nums', color: '#e879f9' }}>{fmtCost(r.cost)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}
