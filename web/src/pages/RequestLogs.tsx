import { useCallback, useEffect, useMemo, useState } from 'react'
import { fetchChannels, fetchUsageLogs, type UsageLogItem } from '../api'
import { Icon } from '../components/Icon'

/**
 * 请求日志页：usage_logs 流水明细（分页 + 筛选）
 * 数据源 GET /api/usage/logs
 */
const PAGE_SIZES = [10, 20, 50]
const PROVIDERS = ['openai', 'anthropic', 'deepseek']

export default function RequestLogs({ refreshTick }: { refreshTick: number }) {
  const [items, setItems] = useState<UsageLogItem[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [channels, setChannels] = useState<{ id: number; name: string }[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  // 筛选条件
  const [fModel, setFModel] = useState('')
  const [fChannel, setFChannel] = useState('')
  const [fProvider, setFProvider] = useState('')
  const [fDays, setFDays] = useState('7')

  const load = useCallback(async () => {
    try {
      setErr('')
      setLoading(true)
      const data = await fetchUsageLogs({
        page,
        page_size: pageSize,
        model: fModel.trim() || undefined,
        channel_id: fChannel ? Number(fChannel) : undefined,
        provider: fProvider || undefined,
        days: fDays ? Number(fDays) : undefined,
      })
      setItems(data.items)
      setTotal(data.total)
    } catch (e) {
      setErr(e instanceof Error ? e.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [page, pageSize, fModel, fChannel, fProvider, fDays])

  useEffect(() => {
    load()
  }, [load, refreshTick])

  // 渠道下拉数据（筛选用）
  useEffect(() => {
    fetchChannels()
      .then((cs) => setChannels(cs.map((c) => ({ id: c.id, name: c.name }))))
      .catch(() => {})
  }, [])

  const totalPages = useMemo(() => Math.max(1, Math.ceil(total / pageSize)), [total, pageSize])

  const fmtTime = (iso: string) => {
    const d = new Date(iso)
    // 短格式：10/25 15:25（去掉年份和秒，省出横向空间）
    const md = `${String(d.getMonth() + 1).padStart(2, '0')}/${String(d.getDate()).padStart(2, '0')}`
    const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
    return `${md} ${hm}`
  }
  const fmtCost = (v: number) => (v > 0 ? `¥${v.toFixed(4)}` : '—')
  // 状态徽标：无 status（旧数据）显示 —；2xx 绿；4xx 黄；5xx 红
  const statusColor = (s?: number) =>
    s == null || s === 0
      ? 'var(--text-muted)'
      : s < 400
        ? 'var(--ok)'
        : s < 500
          ? '#fbbf24'
          : 'var(--danger)'

  return (
    <div>
      <h2>请求日志</h2>
      <p className="subtitle">
        usage_logs 流水明细 · 每次请求一条记录，可审计可追溯 · 数据源 /api/usage/logs
      </p>

      {/* ===== 筛选行 ===== */}
      <div className="glass card-pad" style={{ marginBottom: 14 }}>
        <div className="row" style={{ gap: 10, flexWrap: 'wrap', alignItems: 'flex-end' }}>
          <div>
            <div className="muted" style={{ marginBottom: 4 }}>模型</div>
            <input
              className="glass-input"
              placeholder="如 gpt-5"
              value={fModel}
              onChange={(e) => {
                setFModel(e.target.value)
                setPage(1)
              }}
              style={{ width: 140 }}
            />
          </div>
          <div>
            <div className="muted" style={{ marginBottom: 4 }}>渠道</div>
            <select
              className="glass-input"
              value={fChannel}
              onChange={(e) => {
                setFChannel(e.target.value)
                setPage(1)
              }}
              style={{ width: 130 }}
            >
              <option value="">全部</option>
              {channels.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </div>
          <div>
            <div className="muted" style={{ marginBottom: 4 }}>Provider</div>
            <select
              className="glass-input"
              value={fProvider}
              onChange={(e) => {
                setFProvider(e.target.value)
                setPage(1)
              }}
              style={{ width: 120 }}
            >
              <option value="">全部</option>
              {PROVIDERS.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          </div>
          <div>
            <div className="muted" style={{ marginBottom: 4 }}>时间范围</div>
            <select
              className="glass-input"
              value={fDays}
              onChange={(e) => {
                setFDays(e.target.value)
                setPage(1)
              }}
              style={{ width: 110 }}
            >
              <option value="">全部</option>
              <option value="1">最近 1 天</option>
              <option value="7">最近 7 天</option>
              <option value="30">最近 30 天</option>
            </select>
          </div>
        </div>
      </div>

      {err && <span style={{ color: 'var(--danger)', fontSize: '13px' }}>{err}</span>}

      {/* ===== 日志表格 ===== */}
      <div className="glass card-pad" style={{ overflowX: 'auto' }}>
        {loading ? (
          <div style={{ opacity: 0.7, padding: '14px 0' }}>加载中…</div>
        ) : items.length === 0 ? (
          <div style={{ opacity: 0.7, padding: '14px 0', fontSize: 13 }}>
            没有匹配的请求记录——先去发几个 /v1/chat/completions 请求吧。
          </div>
        ) : (
          <table style={{ width: '100%', minWidth: 880, borderCollapse: 'collapse', fontSize: 12.5 }}>
            <thead>
              <tr style={{ color: 'var(--text-muted)', textAlign: 'left', fontSize: 11.5, letterSpacing: '0.04em' }}>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500, whiteSpace: 'nowrap' }}>时间</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>链路 ID</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>调用方</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>IP</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>渠道</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>模型</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>Provider</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>状态</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>错误</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500, textAlign: 'right' }}>输入</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500, textAlign: 'right' }}>输出</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500, textAlign: 'right' }}>总 tokens</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500, textAlign: 'right' }}>费用</th>
              </tr>
            </thead>
            <tbody>
              {items.map((it) => (
                <tr key={it.id} style={{ borderBottom: '1px solid var(--border)' }}>
                  <td style={{ padding: '9px 8px', whiteSpace: 'nowrap', color: 'var(--text-secondary)', fontVariantNumeric: 'tabular-nums' }}>{fmtTime(it.created_at)}</td>
                  <td style={{ padding: '9px 8px', maxWidth: 96 }}>
                    <span
                      style={{ display: 'block', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: 'var(--text-secondary)', fontSize: 11.5, fontVariantNumeric: 'tabular-nums' }}
                      title={it.request_id}
                    >
                      {it.request_id || '—'}
                    </span>
                  </td>
                  <td style={{ padding: '9px 8px', whiteSpace: 'nowrap' }}>
                    {it.access_key_name ? (
                      <span className="badge" style={{ color: 'var(--accent-soft)', border: '1px solid rgba(139,92,246,0.3)', background: 'rgba(139,92,246,0.08)', fontSize: 10.5 }}>
                        {it.access_key_name}
                        {it.access_key_id ? ` · #${it.access_key_id}` : ''}
                      </span>
                    ) : (
                      <span className="muted" style={{ fontSize: 11.5 }}>{it.access_key_id ? `key #${it.access_key_id}` : '匿名'}</span>
                    )}
                  </td>
                  <td style={{ padding: '9px 8px', whiteSpace: 'nowrap', color: 'var(--text-secondary)', fontSize: 11.5, fontVariantNumeric: 'tabular-nums' }}>{it.client_ip || '—'}</td>
                  <td style={{ padding: '9px 8px', whiteSpace: 'nowrap' }}>{it.channel_name || `渠道 #${it.channel_id}`}</td>
                  <td style={{ padding: '9px 8px', fontWeight: 600 }}>{it.model}</td>
                  <td style={{ padding: '9px 8px' }}>
                    <span className="badge" style={{ color: 'var(--accent-soft)', border: '1px solid rgba(139,92,246,0.3)', background: 'rgba(139,92,246,0.08)', fontSize: 10.5 }}>
                      {it.provider}
                    </span>
                  </td>
                  <td style={{ padding: '9px 8px', whiteSpace: 'nowrap' }}>
                    <span
                      className="badge"
                      style={{
                        color: statusColor(it.status),
                        border: `1px solid ${statusColor(it.status)}44`,
                        background: `${statusColor(it.status)}11`,
                        fontSize: 10.5,
                        fontVariantNumeric: 'tabular-nums',
                      }}
                      title={it.error || undefined}
                    >
                      {it.status == null || it.status === 0 ? '—' : `HTTP ${it.status}`}
                    </span>
                  </td>
                  <td style={{ padding: '9px 8px', maxWidth: 170 }}>
                    <span
                      style={{
                        display: 'block',
                        overflow: 'hidden',
                        textOverflow: 'ellipsis',
                        whiteSpace: 'nowrap',
                        color: it.error ? 'var(--danger)' : 'var(--text-muted)',
                        fontSize: 11.5,
                      }}
                      title={it.error || undefined}
                    >
                      {it.error || '—'}
                    </span>
                  </td>
                  <td style={{ padding: '9px 8px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{it.prompt_tokens}</td>
                  <td style={{ padding: '9px 8px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{it.completion_tokens}</td>
                  <td style={{ padding: '9px 8px', textAlign: 'right', fontVariantNumeric: 'tabular-nums', fontWeight: 600 }}>{it.total_tokens}</td>
                  <td style={{ padding: '9px 8px', textAlign: 'right', fontVariantNumeric: 'tabular-nums', color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>{fmtCost(it.cost)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}

        {/* ===== 分页 ===== */}
        <div className="row" style={{ justifyContent: 'space-between', alignItems: 'center', marginTop: 14, gap: 10, flexWrap: 'wrap' }}>
          <div className="row" style={{ gap: 6, alignItems: 'center' }}>
            <span className="muted">每页</span>
            <select
              className="glass-input"
              value={pageSize}
              onChange={(e) => {
                setPageSize(Number(e.target.value))
                setPage(1)
              }}
              style={{ width: 64, padding: '3px 6px' }}
            >
              {PAGE_SIZES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
            <span className="muted">共 {total} 条</span>
          </div>
          <div className="row" style={{ gap: 6, alignItems: 'center' }}>
            <button className="icon-btn" disabled={page <= 1} onClick={() => setPage((p) => Math.max(1, p - 1))} style={{ width: 28, height: 28 }}>
              <Icon name="x" size={12} style={{ transform: 'rotate(180deg)' }} />
            </button>
            <span className="muted" style={{ minWidth: 56, textAlign: 'center' }}>
              {page} / {totalPages}
            </span>
            <button className="icon-btn" disabled={page >= totalPages} onClick={() => setPage((p) => Math.min(totalPages, p + 1))} style={{ width: 28, height: 28 }}>
              <Icon name="x" size={12} />
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
