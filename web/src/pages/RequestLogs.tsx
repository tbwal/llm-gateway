import { useCallback, useEffect, useMemo, useState, Fragment } from 'react'
import {
  fetchChannels,
  fetchLogFiles,
  fetchUsageLogs,
  downloadLogFile,
  fmtFileSize,
  type LogFileMeta,
  type UsageLogItem,
} from '../api'
import { Icon } from '../components/Icon'

/** 下载按钮：点击触发浏览器保存；下载中显示 loading 态，失败红字提示 */
function DownloadBtn({ relPath, fileName }: { relPath: string; fileName: string }) {
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const run = async () => {
    setBusy(true)
    setErr('')
    try {
      await downloadLogFile(relPath, fileName)
    } catch (e) {
      setErr(e instanceof Error ? e.message : '下载失败')
    } finally {
      setBusy(false)
    }
  }
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
      <button
        onClick={run}
        disabled={busy}
        title={`下载 ${fileName}`}
        aria-label={`下载 ${fileName}`}
        className="req-copy-btn"
        style={{
          border: '1px solid var(--border)',
          background: 'rgba(139,92,246,0.06)',
          padding: '3px 8px',
          cursor: busy ? 'default' : 'pointer',
          color: 'var(--accent-soft)',
          display: 'inline-flex',
          alignItems: 'center',
          gap: 4,
          fontSize: 11.5,
          borderRadius: 6,
        }}
      >
        <Icon name="download" size={12} />
        {busy ? '下载中…' : '下载'}
      </button>
      {err && <span style={{ color: 'var(--danger)', fontSize: 11 }}>{err}</span>}
    </span>
  )
}

/**
 * 请求日志页：usage_logs 流水明细（分页 + 筛选）
 * 数据源 GET /api/usage/logs
 */
const PAGE_SIZES = [10, 20, 50]
const PROVIDERS = ['openai', 'anthropic', 'deepseek']

/** 复制按钮：hover 所在单元格时显示，点击复制，1.5s 内显示 ✓ 反馈
 *  显隐由 CSS 控制：.req-copy-cell:hover 显示；复制成功加 .req-copied 常显 */
function CopyBtn({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
    } catch {
      // 非 https/localhost 环境下 clipboard API 不可用：textarea 兜底
      const ta = document.createElement('textarea')
      ta.value = text
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      document.body.removeChild(ta)
    }
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }
  return (
    <button
      onClick={copy}
      title={`复制 ${label}`}
      aria-label={`复制 ${label}`}
      className={`req-copy-btn${copied ? ' req-copied' : ''}`}
      style={{
        border: 'none',
        background: 'none',
        padding: 2,
        cursor: copied ? 'default' : 'pointer',
        color: copied ? 'var(--ok)' : 'var(--text-muted)',
        display: 'inline-flex',
        alignItems: 'center',
        flexShrink: 0,
      }}
    >
      <Icon name={copied ? 'check' : 'copy'} size={12.5} />
    </button>
  )
}

/** Attempt 一次渠道尝试（与后端 model.Attempt 对齐） */
interface Attempt {
  sequence: number
  channel_id: number
  channel_name: string
  provider: string
  status_code: number // 0 = 没拿到响应（网络错误/解密失败）
  duration_ms: number
  error?: string
  will_retry: boolean // true=换渠道重试；false=最终结果
}

/** 解析 attempts_json（后端存的 JSON 串）；失败/空返回空数组 */
function parseAttempts(raw?: string): Attempt[] {
  if (!raw) return []
  try {
    const arr = JSON.parse(raw)
    return Array.isArray(arr) ? (arr as Attempt[]) : []
  } catch {
    return []
  }
}

/** 尝试链徽标颜色：成功(will_retry=false)绿，失败待重试黄/红 */
function attemptColor(a: Attempt) {
  if (!a.will_retry) return 'var(--ok)'           // 最终结果
  if (a.status_code === 0 || a.status_code >= 500) return 'var(--danger)' // 网络/5xx
  return '#fbbf24'                                 // 其他失败待重试
}

export default function RequestLogs({ refreshTick }: { refreshTick: number }) {
  const [items, setItems] = useState<UsageLogItem[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [channels, setChannels] = useState<{ id: number; name: string }[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  // 日志文件区（请求级日志 + 全局日志）
  const [globalFile, setGlobalFile] = useState<LogFileMeta | null>(null)
  const [logFiles, setLogFiles] = useState<LogFileMeta[]>([])
  const [logDir, setLogDir] = useState('')
  const [logLoading, setLogLoading] = useState(true)
  const [logErr, setLogErr] = useState('')

  // 筛选条件
  const [fModel, setFModel] = useState('')
  const [fChannel, setFChannel] = useState('')
  const [fProvider, setFProvider] = useState('')
  const [fRequestID, setFRequestID] = useState('')
  const [fAccessKey, setFAccessKey] = useState('')
  const [fClientIP, setFClientIP] = useState('')
  const [fDays, setFDays] = useState('7')
  const [expandedId, setExpandedId] = useState<number | null>(null) // 展开尝试链的行 id

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
        request_id: fRequestID.trim() || undefined,
        access_key_name: fAccessKey.trim() || undefined,
        client_ip: fClientIP.trim() || undefined,
        days: fDays ? Number(fDays) : undefined,
      })
      setItems(data.items)
      setTotal(data.total)
    } catch (e) {
      setErr(e instanceof Error ? e.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [page, pageSize, fModel, fChannel, fProvider, fRequestID, fAccessKey, fClientIP, fDays])

  useEffect(() => {
    load()
  }, [load, refreshTick])

  // 渠道下拉数据（筛选用）
  useEffect(() => {
    fetchChannels()
      .then((cs) => setChannels(cs.map((c) => ({ id: c.id, name: c.name }))))
      .catch(() => {})
  }, [])

  // 日志文件列表（请求级日志 + 全局日志）
  useEffect(() => {
    fetchLogFiles()
      .then((d) => {
        setGlobalFile(d.global)
        setLogFiles(d.files)
        setLogDir(d.dir)
        setLogErr('')
      })
      .catch((e) => setLogErr(e instanceof Error ? e.message : '加载日志文件列表失败'))
      .finally(() => setLogLoading(false))
  }, [refreshTick])

  // 请求日志文件按日期分组（服务端已按日期+文件名倒序）
  const byDate = useMemo(() => {
    const groups: { date: string; files: LogFileMeta[] }[] = []
    for (const f of logFiles) {
      let g = groups[groups.length - 1]
      if (!g || g.date !== f.date) {
        g = { date: f.date, files: [] }
        groups.push(g)
      }
      g.files.push(f)
    }
    return groups
  }, [logFiles])

  const totalPages = useMemo(() => Math.max(1, Math.ceil(total / pageSize)), [total, pageSize])

  const fmtTime = (iso: string) => {
    const d = new Date(iso)
    // 短格式：10/25 15:25（去掉年份和秒，省出横向空间）
    const md = `${String(d.getMonth() + 1).padStart(2, '0')}/${String(d.getDate()).padStart(2, '0')}`
    const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
    return `${md} ${hm}`
  }
  const fmtCost = (v: number) => (v > 0 ? `¥${v.toFixed(4)}` : '—')
  // 耗时格式化：<1s 显示毫秒，≥1s 显示秒；0ms（本地毫秒内完成）显示 <1ms
  const fmtDur = (ms?: number) => {
    if (ms == null || ms < 0) return '—'
    if (ms === 0) return '<1ms'
    return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`
  }
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
    // 突破 app-shell 的 1200px 限制：日志表格列多（14 列），窄容器里挤成竖条。
    // 外层用负 margin 溢出到视口，内层限 1440px 居中——比 1200 宽 240px，但不满屏
    <div style={{ marginLeft: 'calc(50% - 50vw)', marginRight: 'calc(50% - 50vw)', padding: '0 24px' }}>
      <div style={{ maxWidth: 1440, margin: '0 auto' }}>
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
              style={{ width: 130 }}
            />
          </div>
          <div>
            <div className="muted" style={{ marginBottom: 4 }}>链路 ID</div>
            <input
              className="glass-input"
              placeholder="req_xxx 精确查"
              value={fRequestID}
              onChange={(e) => {
                setFRequestID(e.target.value)
                setPage(1)
              }}
              style={{ width: 150 }}
            />
          </div>
          <div>
            <div className="muted" style={{ marginBottom: 4 }}>调用方</div>
            <input
              className="glass-input"
              placeholder="如 audit-user / 匿名"
              value={fAccessKey}
              onChange={(e) => {
                setFAccessKey(e.target.value)
                setPage(1)
              }}
              style={{ width: 130 }}
            />
          </div>
          <div>
            <div className="muted" style={{ marginBottom: 4 }}>IP</div>
            <input
              className="glass-input"
              placeholder="如 ::1 / 127.0.0.1"
              value={fClientIP}
              onChange={(e) => {
                setFClientIP(e.target.value)
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

      {/* ===== 日志文件：请求级日志（一条 requestID 一个文件）+ 全局日志 ===== */}
      <div className="glass card-pad" style={{ marginBottom: 14 }}>
        <div className="row" style={{ justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 8 }}>
          <div>
            <span style={{ fontWeight: 600, fontSize: 13.5 }}>日志文件</span>
            <span className="muted" style={{ marginLeft: 10, fontSize: 12 }}>
              {logDir || '…'}
            </span>
          </div>
          {logErr && <span style={{ color: 'var(--danger)', fontSize: 12 }}>{logErr}</span>}
        </div>

        {logLoading ? (
          <div style={{ opacity: 0.7, padding: '10px 0', fontSize: 13 }}>加载文件列表…</div>
        ) : (
          <div style={{ marginTop: 10 }}>
            {/* 全局日志（main.go tee 落盘；存在才显示） */}
            {globalFile && (
              <div
                className="row"
                style={{ justifyContent: 'space-between', alignItems: 'center', gap: 10, padding: '7px 10px', borderRadius: 8, background: 'rgba(139,92,246,0.05)', border: '1px solid var(--border)' }}
              >
                <div className="row" style={{ alignItems: 'center', gap: 8, minWidth: 0 }}>
                  <Icon name="server" size={13} style={{ color: 'var(--accent-soft)', flexShrink: 0 }} />
                  <span style={{ fontSize: 12.5, fontWeight: 600 }}>全局日志</span>
                  <span className="badge" style={{ fontSize: 10.5, color: 'var(--text-muted)' }}>{globalFile.name}</span>
                </div>
                <div className="row" style={{ alignItems: 'center', gap: 10 }}>
                  <span className="muted" style={{ fontSize: 11.5, fontVariantNumeric: 'tabular-nums' }}>
                    {fmtFileSize(globalFile.size)} · {globalFile.mod_time}
                  </span>
                  <DownloadBtn relPath={globalFile.rel_path} fileName={globalFile.name} />
                </div>
              </div>
            )}

            {/* 请求级日志：按日期分组，紧凑列表（超高滚动） */}
            {byDate.length === 0 ? (
              <div style={{ opacity: 0.7, padding: '10px 0', fontSize: 13 }}>还没有请求日志文件——发几个请求后这里会出现 data/request-logs/日期/req_xxx.log。</div>
            ) : (
              <div style={{ marginTop: 8, maxHeight: 240, overflowY: 'auto' }}>
                {byDate.map((g) => (
                  <div key={g.date} style={{ marginBottom: 6 }}>
                    <div className="muted" style={{ fontSize: 11, letterSpacing: '0.05em', padding: '4px 4px 2px' }}>
                      {g.date} · {g.files.length} 个文件
                    </div>
                    {g.files.map((f) => (
                      <div
                        key={f.rel_path}
                        className="row"
                        style={{ justifyContent: 'space-between', alignItems: 'center', gap: 10, padding: '4px 8px', borderRadius: 6 }}
                      >
                        <div className="row" style={{ alignItems: 'center', gap: 8, minWidth: 0 }}>
                          <Icon name="list" size={12} style={{ color: 'var(--text-muted)', flexShrink: 0 }} />
                          <span
                            style={{ fontSize: 11.5, fontVariantNumeric: 'tabular-nums', color: 'var(--text-secondary)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1, minWidth: 0 }}
                            title={f.name}
                          >
                            {f.name.replace(/\.log$/, '')}
                          </span>
                        </div>
                        <div className="row" style={{ alignItems: 'center', gap: 8, flexShrink: 0 }}>
                          <span className="muted" style={{ fontSize: 11, fontVariantNumeric: 'tabular-nums' }}>
                            {fmtFileSize(f.size)} · {f.mod_time.slice(11)}
                          </span>
                          <DownloadBtn relPath={f.rel_path} fileName={f.name} />
                        </div>
                      </div>
                    ))}
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>

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
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>尝试</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>耗时</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500 }}>错误</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500, textAlign: 'right' }}>输入</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500, textAlign: 'right' }}>输出</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500, textAlign: 'right' }}>总 tokens</th>
                <th style={{ padding: '8px 8px', borderBottom: '1px solid var(--border)', fontWeight: 500, textAlign: 'right' }}>费用</th>
              </tr>
            </thead>
            <tbody>
              {items.map((it) => {
                const attempts = parseAttempts(it.attempts_json)
                const colCount = 15 // 表头列数（加"尝试"后=15）
                return (
                <Fragment key={it.id}>
                <tr style={{ borderBottom: '1px solid var(--border)' }}>
                  <td style={{ padding: '9px 8px', whiteSpace: 'nowrap', color: 'var(--text-secondary)', fontVariantNumeric: 'tabular-nums' }}>{fmtTime(it.created_at)}</td>
                  <td style={{ padding: '9px 8px', maxWidth: 112 }}>
                    <span className="req-copy-cell" style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                      <span
                        style={{ display: 'block', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: 'var(--text-secondary)', fontSize: 11.5, fontVariantNumeric: 'tabular-nums', flex: 1, minWidth: 0 }}
                        title={it.request_id}
                      >
                        {it.request_id || '—'}
                      </span>
                      {it.request_id && <CopyBtn text={it.request_id} label="链路 ID" />}
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
                  {/* 尝试链列：>1 次显示徽标（可点开展开）；=1 次/无尝试链显示 — */}
                  <td style={{ padding: '9px 8px', whiteSpace: 'nowrap' }}>
                    {(() => {
                      const attempts = parseAttempts(it.attempts_json)
                      if (attempts.length <= 1) return <span className="muted" style={{ fontSize: 11 }}>—</span>
                      const retried = attempts.filter((a) => a.will_retry).length
                      const isOpen = expandedId === it.id
                      return (
                        <button
                          onClick={() => setExpandedId(isOpen ? null : it.id)}
                          title={isOpen ? '收起尝试链' : `展开 ${attempts.length} 次尝试`}
                          style={{
                            border: '1px solid rgba(139,92,246,0.35)',
                            background: isOpen ? 'rgba(139,92,246,0.15)' : 'rgba(139,92,246,0.06)',
                            color: 'var(--accent-soft)',
                            fontSize: 10.5,
                            padding: '2px 8px',
                            borderRadius: 10,
                            cursor: 'pointer',
                            fontVariantNumeric: 'tabular-nums',
                          }}
                        >
                          {retried > 0 ? `${retried} 次重试` : `${attempts.length} 次`} {isOpen ? '▾' : '▸'}
                        </button>
                      )
                    })()}
                  </td>
                  <td style={{ padding: '9px 8px', whiteSpace: 'nowrap', fontSize: 11.5, fontVariantNumeric: 'tabular-nums' }} title={it.duration_ms != null && it.first_response_ms != null ? `总耗时 ${it.duration_ms}ms · 首响应 ${it.first_response_ms}ms` : undefined}>
                    {it.duration_ms != null && it.duration_ms > 0 ? (
                      <>
                        <span style={{ color: 'var(--text-primary)', fontWeight: 600 }}>{fmtDur(it.duration_ms)}</span>
                        {it.first_response_ms != null && (
                          <span className="muted" style={{ marginLeft: 6 }}>首 {fmtDur(it.first_response_ms)}</span>
                        )}
                      </>
                    ) : (
                      <span className="muted">—</span>
                    )}
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
                {/* 展开行：尝试链详情（跨全部列） */}
                {expandedId === it.id && attempts.length > 0 && (
                  <tr style={{ borderBottom: '1px solid var(--border)', background: 'rgba(139,92,246,0.04)' }}>
                    <td colSpan={colCount} style={{ padding: '10px 14px' }}>
                      <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
                        <div style={{ marginBottom: 8, fontWeight: 600, fontSize: 11.5, letterSpacing: '0.04em' }}>
                          故障转移链 · 共 {attempts.length} 跳
                        </div>
                        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, alignItems: 'center' }}>
                          {attempts.map((a, idx) => (
                            <Fragment key={a.sequence}>
                              {/* 跳之间的箭头 */}
                              {idx > 0 && (
                                <span style={{ color: 'var(--text-muted)', fontSize: 12 }}>→</span>
                              )}
                              <span
                                style={{
                                  display: 'inline-flex',
                                  alignItems: 'center',
                                  gap: 6,
                                  padding: '4px 10px',
                                  borderRadius: 12,
                                  border: `1px solid ${attemptColor(a)}55`,
                                  background: `${attemptColor(a)}11`,
                                  fontSize: 11.5,
                                  fontVariantNumeric: 'tabular-nums',
                                }}
                                title={a.error || undefined}
                              >
                                <span style={{ color: 'var(--text-muted)' }}>#{a.sequence}</span>
                                <span style={{ fontWeight: 600 }}>{a.channel_name || `渠道 #${a.channel_id}`}</span>
                                <span style={{ color: attemptColor(a) }}>
                                  {a.status_code > 0 ? `HTTP ${a.status_code}` : '网络错误'}
                                </span>
                                <span className="muted">{fmtDur(a.duration_ms)}</span>
                                {a.error && (
                                  <span style={{ color: 'var(--danger)', maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                                    {a.error}
                                  </span>
                                )}
                                {a.will_retry && (
                                  <span style={{ color: '#fbbf24', fontSize: 10 }}>(重试)</span>
                                )}
                              </span>
                            </Fragment>
                          ))}
                        </div>
                      </div>
                    </td>
                  </tr>
                )}
                </Fragment>
                )
              })}
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
    </div>
  )
}
