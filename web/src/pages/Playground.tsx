import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { fetchChannels } from '../api'
import { Icon } from '../components/Icon'

/**
 * API 调试台：直接向网关 /v1/chat/completions 发起测试请求
 *  - 非流式：完整 JSON 响应
 *  - 流式（SSE）：逐块解析，实时追加增量文本（打字机效果）
 *  - 支持携带 / 省略 Bearer（模型级鉴权：未受保护模型匿名可调）
 *  - 预设场景：覆盖普通 / 流式 / 鉴权 / Anthropic 渠道 / 多轮对话
 */
interface ChatMsg {
  role: string
  content: string
}

const DEFAULT_MESSAGES: ChatMsg[] = [{ role: 'user', content: '你好，介绍一下你自己' }]

// 预设场景：一键填充（模型 / Bearer / stream / 消息）
const SCENARIOS = [
  {
    name: '普通请求',
    icon: 'check' as const,
    desc: '非流式 JSON 响应',
    model: 'gpt-5',
    bearer: '',
    stream: false,
    messages: [{ role: 'user', content: '你好' }],
  },
  {
    name: '流式打字机',
    icon: 'activity' as const,
    desc: 'SSE 逐块增量',
    model: 'gpt-5',
    bearer: '',
    stream: true,
    messages: [{ role: 'user', content: '用三句话介绍广州' }],
  },
  {
    name: 'Anthropic 渠道',
    icon: 'key' as const,
    desc: '请求侧转换 → Claude',
    model: 'claude-3-5-sonnet',
    bearer: '',
    stream: false,
    messages: [{ role: 'user', content: 'hi' }],
  },
  {
    name: '多轮对话',
    icon: 'list' as const,
    desc: '历史 + 当前问题',
    model: 'gpt-5',
    bearer: '',
    stream: false,
    messages: [
      { role: 'user', content: '我的名字是 Alice' },
      { role: 'assistant', content: '好的 Alice，记住了！' },
      { role: 'user', content: '我叫什么名字？' },
    ],
  },
  {
    name: '无鉴权模型',
    icon: 'bolt' as const,
    desc: '未受保护模型匿名调用',
    model: 'claude-3-5-sonnet',
    bearer: '',
    stream: false,
    messages: [{ role: 'user', content: 'hello' }],
  },
]

export default function Playground({ refreshTick }: { refreshTick: number }) {
  const [model, setModel] = useState(SCENARIOS[0].model)
  const [bearer, setBearer] = useState('')
  const [stream, setStream] = useState(false)
  const [messagesJson, setMessagesJson] = useState(() => JSON.stringify(DEFAULT_MESSAGES, null, 2))

  // 请求状态
  const [sending, setSending] = useState(false)
  const [status, setStatus] = useState<number | null>(null)
  const [latency, setLatency] = useState<number | null>(null)
  const [nonStreamRaw, setNonStreamRaw] = useState('') // 非流式原始响应体
  const [streamText, setStreamText] = useState('') // 流式累积 delta 文本
  const [streamRaw, setStreamRaw] = useState('') // 流式原始 SSE 事件流
  const [streamMeta, setStreamMeta] = useState<{ usage?: unknown; finishReason?: string; eventCount: number }>({
    eventCount: 0,
  })
  const [err, setErr] = useState('')
  const [copied, setCopied] = useState(false)

  // 模型建议列表（来自渠道配置）
  const [modelSuggestions, setModelSuggestions] = useState<string[]>([])
  useEffect(() => {
    fetchChannels()
      .then((cs) => {
        const set = new Set<string>()
        for (const c of cs) {
          for (const m of (c.models || '').split(',')) {
            const t = m.trim()
            if (t && t !== '*') set.add(t)
          }
        }
        setModelSuggestions([...set].sort())
      })
      .catch(() => {})
  }, [refreshTick])

  const abortRef = useRef<AbortController | null>(null)

  const parseMessages = useCallback((): ChatMsg[] | null => {
    try {
      const v = JSON.parse(messagesJson)
      if (!Array.isArray(v)) throw new Error('messages 必须是数组')
      for (const m of v) {
        if (!m || typeof m.role !== 'string' || typeof m.content !== 'string') {
          throw new Error('每条消息需包含 role 与 content 字符串')
        }
      }
      return v
    } catch (e) {
      setErr(`messages JSON 无效: ${e instanceof Error ? e.message : e}`)
      return null
    }
  }, [messagesJson])

  const send = useCallback(async () => {
    const msgs = parseMessages()
    if (!msgs) return
    setErr('')
    setStatus(null)
    setLatency(null)
    setNonStreamRaw('')
    setStreamText('')
    setStreamRaw('')
    setStreamMeta({ eventCount: 0 })

    const started = performance.now()
    const controller = new AbortController()
    abortRef.current = controller

    const headers: Record<string, string> = { 'Content-Type': 'application/json' }
    if (bearer.trim()) headers['Authorization'] = `Bearer ${bearer.trim()}`

    try {
      setSending(true)
      const res = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers,
        body: JSON.stringify({ model, messages: msgs, stream }),
        signal: controller.signal,
      })
      setStatus(res.status)
      setLatency(Math.round(performance.now() - started))

      if (!stream) {
        const text = await res.text()
        // 完整原始响应体：不做解析改写，原样展示
        setNonStreamRaw(text)
        return
      }

      // ── SSE 流式：逐块读取，按空行切事件 ──
      // ① 原始 SSE 事件流完整累积（data: 行原文）
      // ② 提取 delta.content 增量做打字机；usage/finish_reason 取末尾汇总
      if (!res.body) {
        setErr('响应无 body（流式失败）')
        return
      }
      const reader = res.body.getReader()
      const decoder = new TextDecoder()
      let buf = ''
      let textAcc = ''
      let rawAcc = ''
      let events = 0
      let finish: string | undefined
      let usage: unknown

      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        // SSE 事件以空行分隔；最后一段可能不完整，留在 buf 下次拼
        const parts = buf.split('\n\n')
        buf = parts.pop() ?? ''
        for (const part of parts) {
          rawAcc += part + '\n\n'
          setStreamRaw(rawAcc)
          for (const line of part.split('\n')) {
            if (!line.startsWith('data:')) continue
            const payload = line.slice(5).trim()
            if (!payload || payload === '[DONE]') continue
            try {
              const evt = JSON.parse(payload)
              events++
              const delta = evt.choices?.[0]?.delta
              if (delta?.content) {
                textAcc += delta.content
                setStreamText(textAcc)
              }
              if (evt.choices?.[0]?.finish_reason) {
                finish = evt.choices[0].finish_reason
              }
              if (evt.usage) usage = evt.usage
            } catch {
              /* 非 JSON 行忽略 */
            }
          }
        }
      }
      setStreamText(textAcc)
      setStreamMeta({ usage, finishReason: finish, eventCount: events })
    } catch (e) {
      if (e instanceof DOMException && e.name === 'AbortError') {
        setErr('已中止')
      } else {
        setErr(e instanceof Error ? e.message : '请求失败')
      }
    } finally {
      setSending(false)
    }
  }, [model, bearer, stream, messagesJson, parseMessages])

  // 页面切换时中止进行中的请求
  useEffect(() => () => abortRef.current?.abort(), [])

  const stop = () => abortRef.current?.abort()

  const fmtMessages = useMemo(() => messagesJson.split('\n').length, [messagesJson])

  // ── 完整 curl：与页面发送等价的直连网关命令（流式加 -N 禁用缓冲） ──
  const curl = useMemo(() => {
    let msgs: ChatMsg[] | null = null
    try {
      msgs = JSON.parse(messagesJson)
    } catch {
      /* 非法 JSON 时回退到默认模板 */
    }
    const payload = JSON.stringify({ model, messages: msgs ?? DEFAULT_MESSAGES, stream })
    const lines = [
      stream ? 'curl -N' : 'curl',
      '-X POST http://localhost:8080/v1/chat/completions',
      '-H "Content-Type: application/json"',
    ]
    if (bearer.trim()) lines.push(`-H "Authorization: Bearer ${bearer.trim()}"`)
    lines.push(`-d '${payload}'`)
    return lines.join(' \\\n  ')
  }, [model, bearer, stream, messagesJson])

  const copyCurl = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(curl)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      setErr('复制失败（浏览器拒绝剪贴板权限）')
    }
  }, [curl])

  const statusColor =
    status == null ? 'var(--text-muted)' : status < 400 ? 'var(--ok)' : status < 500 ? '#fbbf24' : 'var(--danger)'

  return (
    <div>
      <h2>API 调试台</h2>
      <p className="subtitle">
        直连网关统一入口 /v1/chat/completions · 覆盖非流式 / 流式 / 鉴权 / 协议转换测试
      </p>

      {/* ===== 预设场景 ===== */}
      <div className="glass card-pad" style={{ marginBottom: 14 }}>
        <div className="muted" style={{ marginBottom: 8 }}>预设场景（一键填充配置）</div>
        <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
          {SCENARIOS.map((s) => (
            <button
              key={s.name}
              className="icon-btn"
              title={s.desc}
              onClick={() => {
                setModel(s.model)
                setBearer(s.bearer)
                setStream(s.stream)
                setMessagesJson(JSON.stringify(s.messages, null, 2))
                setErr('')
              }}
              style={{ padding: '6px 12px', gap: 6 }}
            >
              <Icon name={s.icon} size={12} />
              {s.name}
            </button>
          ))}
        </div>
      </div>

      <div className="dash-grid">
        {/* ===== 左：请求配置 ===== */}
        <div className="glass card-pad">
          <div className="panel-title" style={{ marginBottom: 14 }}>请求配置</div>

          <div className="muted" style={{ marginBottom: 4 }}>模型</div>
          <input
            className="glass-input"
            list="model-suggestions"
            value={model}
            onChange={(e) => setModel(e.target.value)}
            placeholder="如 gpt-5 / claude-3-5-sonnet"
            style={{ width: '100%', marginBottom: 12 }}
          />
          <datalist id="model-suggestions">
            {modelSuggestions.map((m) => (
              <option key={m} value={m} />
            ))}
          </datalist>

          <div className="muted" style={{ marginBottom: 4 }}>Authorization（Bearer，可留空=匿名）</div>
          <input
            className="glass-input"
            value={bearer}
            onChange={(e) => setBearer(e.target.value)}
            placeholder="sk_lgw_…"
            style={{ width: '100%', marginBottom: 12 }}
          />

          <div className="row" style={{ alignItems: 'center', marginBottom: 12, gap: 8 }}>
            <span
              className="badge"
              style={{
                cursor: 'pointer',
                color: stream ? 'var(--accent-soft)' : 'var(--text-muted)',
                border: `1px solid ${stream ? 'rgba(139,92,246,0.4)' : 'rgba(255,255,255,0.15)'}`,
                background: stream ? 'rgba(139,92,246,0.12)' : 'transparent',
              }}
              onClick={() => setStream((s) => !s)}
            >
              <Icon name={stream ? 'check' : 'x'} size={10} />
              stream={String(stream)}
            </span>
            <span className="muted">点击切换流式 / 非流式</span>
          </div>

          <div className="row" style={{ justifyContent: 'space-between', alignItems: 'center', marginBottom: 4 }}>
            <div className="muted">messages（JSON 数组，{fmtMessages} 行）</div>
            <span
              style={{ fontSize: 11, color: 'var(--accent-soft)', cursor: 'pointer' }}
              onClick={() => setMessagesJson(JSON.stringify(DEFAULT_MESSAGES, null, 2))}
            >
              重置模板
            </span>
          </div>
          <textarea
            className="glass-input"
            value={messagesJson}
            onChange={(e) => setMessagesJson(e.target.value)}
            spellCheck={false}
            style={{
              width: '100%',
              minHeight: 150,
              fontFamily: 'var(--mono, Consolas, monospace)',
              fontSize: 12,
              resize: 'vertical',
              whiteSpace: 'pre',
            }}
          />

          <div className="row" style={{ gap: 8, marginTop: 14 }}>
            <button className="btn-primary" onClick={send} disabled={sending} style={{ gap: 6 }}>
              <Icon name={sending ? 'activity' : 'bolt'} size={14} />
              {sending ? '请求中…' : '发送请求'}
            </button>
            {sending && (
              <button className="icon-btn" onClick={stop} title="中止">
                <Icon name="x" size={14} />
              </button>
            )}
          </div>

          {err && (
            <div style={{ color: 'var(--danger)', fontSize: 12.5, marginTop: 10 }}>{err}</div>
          )}
        </div>

        {/* ===== 右：响应区 ===== */}
        <div className="glass card-pad">
          <div className="row" style={{ justifyContent: 'space-between', alignItems: 'center', marginBottom: 10 }}>
            <div className="panel-title" style={{ margin: 0 }}>响应</div>
            {(status != null || latency != null) && (
              <div className="row" style={{ gap: 8, alignItems: 'center' }}>
                {status != null && (
                  <span
                    className="badge"
                    style={{ color: statusColor, border: `1px solid ${statusColor}44`, background: `${statusColor}11`, fontSize: 11 }}
                  >
                    HTTP {status}
                  </span>
                )}
                {latency != null && (
                  <span className="muted" style={{ fontVariantNumeric: 'tabular-nums' }}>{latency} ms</span>
                )}
                {stream && (
                  <span className="muted" style={{ fontVariantNumeric: 'tabular-nums' }}>
                    {streamMeta.eventCount} events
                  </span>
                )}
              </div>
            )}
          </div>

          {/* ===== 完整 curl（当前配置实时生成，可复制到终端跑） ===== */}
          <div className="muted" style={{ marginBottom: 4, fontSize: 11.5 }}>
            本次请求等价 curl（直连网关 :8080）
          </div>
          <div style={{ position: 'relative' }}>
            <pre
              style={{
                margin: 0,
                marginBottom: 14,
                padding: '10px 12px',
                paddingRight: 64,
                borderRadius: 10,
                background: 'rgba(0,0,0,0.35)',
                border: '1px solid rgba(255,255,255,0.08)',
                fontSize: 11.5,
                lineHeight: 1.6,
                overflowX: 'auto',
                fontFamily: 'var(--mono, Consolas, monospace)',
                color: 'var(--text-secondary)',
                whiteSpace: 'pre-wrap',
                wordBreak: 'break-all',
              }}
            >
              {curl}
            </pre>
            <button
              className="icon-btn"
              onClick={copyCurl}
              title="复制 curl"
              style={{ position: 'absolute', top: 8, right: 8, width: 28, height: 28 }}
            >
              <Icon name={copied ? 'check' : 'key'} size={12} />
            </button>
            {copied && (
              <span style={{ position: 'absolute', top: 38, right: 8, fontSize: 10.5, color: 'var(--ok)' }}>已复制</span>
            )}
          </div>

          {status == null && !sending ? (
            <div style={{ opacity: 0.6, fontSize: 13, padding: '20px 0', textAlign: 'center' }}>
              配置左侧请求后点击发送，响应显示在这里。
            </div>
          ) : stream ? (
            <>
              {/* 累积 delta 文本（打字机效果） */}
              <div
                className="glass"
                style={{
                  padding: '12px 14px',
                  borderRadius: 10,
                  minHeight: 90,
                  fontSize: 13.5,
                  lineHeight: 1.7,
                  whiteSpace: 'pre-wrap',
                  marginBottom: 12,
                }}
              >
                {streamText || (sending ? <span style={{ opacity: 0.5 }}>等待首个增量…</span> : '（无增量内容）')}
              </div>
              {streamMeta.finishReason && (
                <div className="muted" style={{ marginBottom: 4 }}>
                  finish_reason: <span style={{ color: 'var(--text-secondary)' }}>{streamMeta.finishReason}</span>
                </div>
              )}
              {streamMeta.usage && (
                <div className="muted" style={{ marginBottom: 10 }}>
                  usage: <span style={{ color: 'var(--text-secondary)' }}>{JSON.stringify(streamMeta.usage)}</span>
                </div>
              )}
              {/* 完整原始 SSE 事件流 */}
              <div className="muted" style={{ marginBottom: 4, fontSize: 11.5 }}>完整原始 SSE 事件流</div>
              <pre
                style={{
                  margin: 0,
                  maxHeight: 300,
                  overflow: 'auto',
                  padding: '10px 12px',
                  borderRadius: 10,
                  background: 'rgba(0,0,0,0.35)',
                  border: '1px solid rgba(255,255,255,0.08)',
                  fontSize: 11.5,
                  lineHeight: 1.6,
                  fontFamily: 'var(--mono, Consolas, monospace)',
                  whiteSpace: 'pre-wrap',
                  wordBreak: 'break-all',
                  color: 'var(--text-secondary)',
                }}
              >
                {streamRaw || (sending ? '等待首个事件…' : '（无事件）')}
              </pre>
            </>
          ) : (
            <>
              <div className="muted" style={{ marginBottom: 4, fontSize: 11.5 }}>完整原始响应体</div>
              <pre
                style={{
                  margin: 0,
                  maxHeight: 420,
                  overflow: 'auto',
                  fontSize: 12,
                  lineHeight: 1.6,
                  fontFamily: 'var(--mono, Consolas, monospace)',
                  whiteSpace: 'pre-wrap',
                  wordBreak: 'break-word',
                  color: 'var(--text-secondary)',
                }}
              >
                {nonStreamRaw || (sending ? '等待响应…' : '（空响应体）')}
              </pre>
            </>
          )}
        </div>
      </div>
    </div>
  )
}
