import { useCallback, useEffect, useState } from 'react'
import { fetchSystemStatus, type SystemStatus } from '../api'
import { Icon } from '../components/Icon'

/**
 * 系统状态页：聚合网关运行时快照（/api/system/status）
 *  - 服务 / 限流 / 安全 / 渠道缓存 / AccessKey 规模
 *  - 熔断器状态：每个渠道 closed / open / half_open + 冷却剩余
 */
const BREAKER_META: Record<string, { text: string; icon: string; color: string; bg: string; border: string }> = {
  closed: {
    text: '正常',
    icon: 'check',
    color: 'var(--ok)',
    bg: 'rgba(74,222,128,0.08)',
    border: 'rgba(74,222,128,0.3)',
  },
  open: {
    text: '熔断中',
    icon: 'alert',
    color: 'var(--danger)',
    bg: 'rgba(248,113,113,0.08)',
    border: 'rgba(248,113,113,0.3)',
  },
  half_open: {
    text: '半开探测',
    icon: 'gauge',
    color: '#fbbf24',
    bg: 'rgba(251,191,36,0.08)',
    border: 'rgba(251,191,36,0.3)',
  },
}

export default function SystemStatusPage({ refreshTick }: { refreshTick: number }) {
  const [st, setSt] = useState<SystemStatus | null>(null)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  const load = useCallback(async () => {
    try {
      setErr('')
      setSt(await fetchSystemStatus())
    } catch (e) {
      setErr(e instanceof Error ? e.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load, refreshTick])

  const fmt = (iso: string | null | undefined) =>
    iso ? new Date(iso).toLocaleString('zh-CN') : '—'

  return (
    <div>
      <h2>系统状态</h2>
      <p className="subtitle">
        运行时快照：服务 / 限流 / 加密 / 渠道缓存 / 熔断器 · 数据源 /api/system/status
      </p>

      {err && <span style={{ color: 'var(--danger)', fontSize: '13px' }}>{err}</span>}

      {loading || !st ? (
        <div className="glass card-pad" style={{ opacity: 0.7 }}>
          加载中…
        </div>
      ) : (
        <>
          {/* ===== 状态卡组 ===== */}
          <div className="stats-row">
            <div className="glass sys-card">
              <div className="sys-head">
                <Icon name="server" size={13} /> 服务运行
              </div>
              <div className="sys-main">{st.server.uptime}</div>
              <div className="sys-sub">
                <Icon name="clock" size={11} style={{ verticalAlign: -1, marginRight: 3 }} />
                启动于 {fmt(st.server.started_at)}
              </div>
            </div>

            <div className="glass sys-card">
              <div className="sys-head">
                <Icon name="gauge" size={13} /> 限流
              </div>
              <div className="sys-main">
                <span
                  className="badge"
                  style={{
                    alignSelf: 'center',
                    color: st.rate_limit.enabled ? 'var(--ok)' : 'var(--text-muted)',
                    border: `1px solid ${st.rate_limit.enabled ? 'rgba(74,222,128,0.3)' : 'rgba(255,255,255,0.12)'}`,
                    background: st.rate_limit.enabled ? 'rgba(74,222,128,0.08)' : 'transparent',
                  }}
                >
                  <Icon name={st.rate_limit.enabled ? 'check' : 'x'} size={10} />
                  {st.rate_limit.enabled ? '开启' : '关闭'}
                </span>
                <span style={{ fontSize: 26 }}>{st.rate_limit.limit_per_min}</span>
                <span className="unit">次/分钟</span>
              </div>
              <div className="sys-sub">
                store={st.rate_limit.store} · redis {st.rate_limit.redis_addr}
              </div>
            </div>

            <div className="glass sys-card">
              <div className="sys-head">
                <Icon name="shield" size={13} /> 密钥加密
              </div>
              <div className="sys-main" style={{ fontSize: 15, fontWeight: 600, alignItems: 'center' }}>
                <Icon name={st.security.secret_key_set ? 'check' : 'bolt'} size={15} color={undefined} />
                <span style={{ color: st.security.secret_key_set ? 'var(--ok)' : '#fbbf24' }}>
                  {st.security.secret_key_set ? 'AES 密钥已配置' : '开发默认密钥'}
                </span>
              </div>
              <div className="sys-sub">
                {st.security.secret_key_set ? '渠道密钥加密传输已启用' : '生产请设置 LLM_GATEWAY_SECRET_KEY'}
              </div>
            </div>

            <div className="glass sys-card">
              <div className="sys-head">
                <Icon name="database" size={13} /> 渠道缓存
              </div>
              <div className="sys-main">
                {st.cache.enabled_channels}
                <span className="unit">/ {st.cache.total_channels} 启用</span>
              </div>
              <div className="sys-sub">
                <Icon name="clock" size={11} style={{ verticalAlign: -1, marginRight: 3 }} />
                快照 {fmt(st.cache.last_refresh)}
              </div>
            </div>

            <div className="glass sys-card">
              <div className="sys-head">
                <Icon name="key" size={13} /> AccessKey
              </div>
              <div className="sys-main">{st.access_keys}</div>
              <div className="sys-sub">已签发调用方凭证</div>
            </div>
          </div>

          {/* ===== 熔断器状态 ===== */}
          <div className="glass card-pad">
            <div className="row" style={{ alignItems: 'center', justifyContent: 'space-between', marginBottom: 2 }}>
              <div className="panel-title" style={{ margin: 0 }}>熔断器状态</div>
              {st.breakers.length > 0 && (
                <div className="row" style={{ gap: 10, alignItems: 'center' }}>
                  {(
                    [
                      ['closed', '正常'],
                      ['half_open', '半开'],
                      ['open', '熔断'],
                    ] as const
                  ).map(([key, label]) => {
                    const n = st.breakers.filter((b) => b.state === key).length
                    const meta = BREAKER_META[key]
                    return (
                      <span
                        key={key}
                        className="badge"
                        style={{ color: meta.color, border: `1px solid ${meta.border}`, background: meta.bg, fontSize: 11 }}
                      >
                        <span style={{ width: 5, height: 5, borderRadius: '50%', background: meta.color, display: 'inline-block' }} />
                        {n} {label}
                      </span>
                    )
                  })}
                </div>
              )}
            </div>
            <div
              style={{
                fontSize: 11.5,
                color: 'var(--text-muted)',
                marginBottom: 10,
                paddingLeft: 24,
              }}
            >
              连续失败 3 次自动熔断摘除，冷却 30s 后放行探测自动恢复
            </div>

            {st.breakers.length === 0 ? (
              <div
                style={{
                  display: 'flex',
                  flexDirection: 'column',
                  alignItems: 'center',
                  padding: '30px 0 26px',
                  gap: 10,
                }}
              >
                <div
                  style={{
                    width: 54,
                    height: 54,
                    borderRadius: '50%',
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'center',
                    background: 'linear-gradient(135deg, rgba(74,222,128,0.16), rgba(74,222,128,0.04))',
                    border: '1px solid rgba(74,222,128,0.28)',
                    boxShadow: '0 0 26px rgba(74,222,128,0.10)',
                  }}
                >
                  <Icon name="check" size={22} color="var(--ok)" strokeWidth={2} />
                </div>
                <div style={{ fontSize: 14, fontWeight: 600 }}>所有渠道状态正常</div>
                <div
                  style={{
                    fontSize: 12,
                    color: 'var(--text-muted)',
                    textAlign: 'center',
                    lineHeight: 1.7,
                    maxWidth: 420,
                  }}
                >
                  连续失败 3 次自动熔断摘除，冷却 30s 后放行探测自动恢复。
                  <br />
                  渠道发生故障时，这里会实时展示每个渠道的状态与冷却倒计时。
                </div>
              </div>
            ) : (
              <div className="breaker-list">
                {st.breakers.map((b) => {
                  const meta = BREAKER_META[b.state]
                  return (
                    <div key={b.channel_id} className="breaker-row">
                      <div className="breaker-id">
                        {b.channel_name || `渠道 #${b.channel_id}`}
                      </div>
                      <span className="badge" style={{ color: meta.color, border: `1px solid ${meta.border}`, background: meta.bg }}>
                        <Icon name={meta.icon} size={10} />
                        {meta.text}
                      </span>
                      <div className="breaker-desc">
                        {b.state === 'open'
                          ? `连续失败 ${b.failures} 次${b.cooldown_remaining_sec > 0 ? ` · 冷却剩余 ${Math.ceil(b.cooldown_remaining_sec)}s` : ' · 冷却已过，等待半开探测'}`
                          : b.state === 'half_open'
                            ? '已放行 1 个探测请求，等待结果'
                            : `失败计数 ${b.failures}（成功清零）`}
                      </div>
                      {b.state === 'open' && b.opened_at && (
                        <div className="breaker-time">熔断于 {fmt(b.opened_at)}</div>
                      )}
                    </div>
                  )
                })}
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}
