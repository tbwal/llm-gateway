import { useCallback, useEffect, useMemo, useState } from 'react'
import * as echarts from 'echarts'
import {
  fetchSummary,
  fetchDaily,
  fetchBreakdown,
  fetchChannels,
  fmtCost,
  type Channel,
  type DailyRow,
  type BreakdownRow,
} from '../api'
import StatCard from '../components/StatCard'
import { Icon } from '../components/Icon'
import { useECharts, chartTextColor, chartAxisColor } from '../hooks/useECharts'

export default function Dashboard({ refreshTick }: { refreshTick: number }) {
  const [summary, setSummary] = useState({ requests: 0, total_tokens: 0, cost: 0 })
  const [daily, setDaily] = useState<DailyRow[]>([])
  const [breakdown, setBreakdown] = useState<BreakdownRow[]>([])
  const [channels, setChannels] = useState<Channel[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  const load = useCallback(async () => {
    try {
      setErr('')
      const [s, d, b, c] = await Promise.all([
        fetchSummary(7),
        fetchDaily(7),
        fetchBreakdown('model', 7),
        fetchChannels(),
      ])
      setSummary({
        requests: s.summary.requests,
        total_tokens: s.summary.total_tokens,
        cost: s.summary.cost,
      })
      setDaily(d.rows ?? [])
      setBreakdown(b.rows ?? [])
      setChannels(c)
    } catch (e) {
      setErr(e instanceof Error ? e.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load, refreshTick])

  // ===== 每日趋势折线（请求数 + tokens 双轴，黑紫风配色） =====
  const lineOption = useMemo<echarts.EChartsOption | null>(() => {
    if (daily.length === 0) return null
    const dates = daily.map((r) => r.date)
    return {
      backgroundColor: 'transparent',
      tooltip: { trigger: 'axis' },
      legend: {
        data: ['请求数', '总 Tokens'],
        textStyle: { color: chartTextColor, fontSize: 12 },
        top: 0,
        itemWidth: 14,
      },
      grid: { left: 12, right: 12, top: 34, bottom: 8, containLabel: true },
      xAxis: {
        type: 'category',
        data: dates,
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
          name: 'Tokens',
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
          symbol: 'circle',
          symbolSize: 5,
          data: daily.map((r) => r.requests),
          itemStyle: { color: '#a78bfa' },
          areaStyle: {
            color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
              { offset: 0, color: 'rgba(139,92,246,0.32)' },
              { offset: 1, color: 'rgba(139,92,246,0)' },
            ]),
          },
        },
        {
          name: '总 Tokens',
          type: 'line',
          smooth: true,
          yAxisIndex: 1,
          symbol: 'circle',
          symbolSize: 5,
          data: daily.map((r) => r.total_tokens),
          itemStyle: { color: '#e879f9' },
        },
      ],
    }
  }, [daily])

  const lineRef = useECharts(lineOption)

  // ===== 模型费用排行（前 6） =====
  const rank = useMemo(() => {
    const sorted = [...breakdown].sort((a, b) => b.cost - a.cost).slice(0, 6)
    const max = sorted[0]?.cost || 1
    return sorted.map((r) => ({ ...r, pct: Math.max(2, (r.cost / max) * 100) }))
  }, [breakdown])

  const enabledCount = useMemo(() => channels.filter((c) => c.enabled).length, [channels])

  return (
    <div>
      <h2>总览</h2>
      <p className="subtitle">最近 7 天网关运行概览 · 数据源 /api/usage/* 与 /api/channels</p>

      {/* ===== 统计卡 ===== */}
      <div className="stats-row">
        <StatCard label="总请求数" value={summary.requests} suffix="次" icon="chart" sub="近 7 天" />
        <StatCard label="总 Tokens" value={summary.total_tokens} icon="dashboard" sub="近 7 天" />
        <StatCard label="累计费用" value={summary.cost} suffix="元" icon="shield" sub="按单价三级匹配计算" decimals={2} />
        <StatCard label="启用渠道" value={enabledCount} suffix="个" icon="tag" sub={`共 ${channels.length} 个渠道`} />
      </div>

      {err && (
        <div className="glass panel err-panel" style={{ marginBottom: '14px', display: 'flex', alignItems: 'center', gap: 8 }}>
          <span style={{ color: 'var(--danger)', display: 'inline-flex' }}>
            <Icon name="alert" size={15} />
          </span>
          <span>数据加载失败：{err}（请确认网关已启动在 :8080）</span>
        </div>
      )}

      {loading && !err ? (
        <div className="glass panel" style={{ padding: '40px', textAlign: 'center', color: 'var(--text-secondary)' }}>
          加载中…
        </div>
      ) : (
        <>
          <div className="dash-grid">
            <div className="glass panel">
              <div className="panel-title">每日请求趋势</div>
              <div className="chart-box" ref={lineRef} />
            </div>
            <div className="glass panel">
              <div className="panel-title">模型费用排行</div>
              {rank.length === 0 ? (
                <div className="muted" style={{ padding: '30px 0', textAlign: 'center' }}>
                  暂无数据，先发几个请求
                </div>
              ) : (
                rank.map((r) => (
                  <div className="rank-row" key={r.key}>
                    <span className="rank-name">{r.key}</span>
                    <div className="rank-bar-wrap">
                      <div className="rank-bar" style={{ width: `${r.pct}%` }} />
                    </div>
                    <span className="rank-val">{fmtCost(r.cost)}</span>
                  </div>
                ))
              )}
            </div>
          </div>

          {/* ===== 渠道状态 ===== */}
          <div className="glass panel">
            <div className="panel-title">渠道状态</div>
            <div className="channel-grid">
              {channels.length === 0 && (
                <div className="muted" style={{ padding: '16px 0' }}>
                  暂无渠道，去「渠道管理」创建一个
                </div>
              )}
              {channels.map((c) => (
                <div className="glass channel-chip" key={c.id}>
                  <span className={`dot ${c.enabled ? 'ok' : 'off'}`} />
                  <div className="channel-meta">
                    <div className="channel-name">{c.name}</div>
                    <div className="channel-sub">
                      {c.provider} · {c.models.split(',').slice(0, 2).join('/')}
                      {c.models.split(',').length > 2 ? '…' : ''}
                    </div>
                  </div>
                  <span className="channel-weight">w={c.weight}</span>
                </div>
              ))}
            </div>
          </div>
        </>
      )}
    </div>
  )
}
