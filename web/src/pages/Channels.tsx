import { useCallback, useEffect, useMemo, useState } from 'react'
import { createPortal } from 'react-dom'
import { motion, AnimatePresence } from 'framer-motion'
import { fetchChannels, type Channel } from '../api'
import { Icon } from '../components/Icon'

interface ChannelForm {
  name: string
  provider: string
  base_url: string
  models: string
  weight: number
  enabled: boolean
  api_key: string
}

const emptyForm: ChannelForm = {
  name: '',
  provider: 'openai',
  base_url: '',
  models: '',
  weight: 1,
  enabled: true,
  api_key: '',
}

export default function Channels({ refreshTick }: { refreshTick: number }) {
  const [channels, setChannels] = useState<Channel[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [modal, setModal] = useState<{ mode: 'create' | 'edit'; id?: number } | null>(null)
  const [form, setForm] = useState<ChannelForm>(emptyForm)
  const [saving, setSaving] = useState(false)
  const [togglingId, setTogglingId] = useState<number | null>(null)

  const load = useCallback(async () => {
    try {
      setErr('')
      setChannels(await fetchChannels())
    } catch (e) {
      setErr(e instanceof Error ? e.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load, refreshTick])

  const openCreate = () => {
    setForm(emptyForm)
    setModal({ mode: 'create' })
  }
  const openEdit = (c: Channel) => {
    setForm({
      name: c.name,
      provider: c.provider,
      base_url: c.base_url,
      models: c.models,
      weight: c.weight,
      enabled: c.enabled,
      api_key: '', // 编辑时留空 = 保持原密钥（后端规则）
    })
    setModal({ mode: 'edit', id: c.id })
  }

  const save = async () => {
    if (!form.name.trim()) return
    setSaving(true)
    try {
      const body = { ...form }
      const res = await fetch(
        modal?.mode === 'edit' ? `/api/channels/${modal!.id}` : '/api/channels',
        {
          method: modal?.mode === 'edit' ? 'PUT' : 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        },
      )
      const data = await res.json().catch(() => ({}))
      if (!res.ok || data.code !== 0) {
        setErr(data.message || `HTTP ${res.status}`)
        return
      }
      setModal(null)
      await load()
    } catch (e) {
      setErr(e instanceof Error ? e.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  // 启用/禁用开关：PUT 全量更新，enabled 翻转
  const toggle = async (c: Channel) => {
    setTogglingId(c.id)
    try {
      const res = await fetch(`/api/channels/${c.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: c.name,
          provider: c.provider,
          base_url: c.base_url,
          models: c.models,
          weight: c.weight,
          enabled: !c.enabled,
        }),
      })
      const data = await res.json().catch(() => ({}))
      if (!res.ok || data.code !== 0) {
        setErr(data.message || `HTTP ${res.status}`)
        return
      }
      await load()
    } catch (e) {
      setErr(e instanceof Error ? e.message : '切换失败')
    } finally {
      setTogglingId(null)
    }
  }

  const remove = async (c: Channel) => {
    if (!confirm(`确认删除渠道「${c.name}」？该操作不可恢复。`)) return
    try {
      const res = await fetch(`/api/channels/${c.id}`, { method: 'DELETE' })
      const data = await res.json().catch(() => ({}))
      if (!res.ok || data.code !== 0) {
        setErr(data.message || `HTTP ${res.status}`)
        return
      }
      await load()
    } catch (e) {
      setErr(e instanceof Error ? e.message : '删除失败')
    }
  }

  const providerLabel = useMemo(
    () => (p: string) => ({ openai: 'OpenAI', anthropic: 'Anthropic', deepseek: 'DeepSeek' })[p] || p,
    [],
  )

  return (
    <div>
      <h2>渠道管理</h2>
      <p className="subtitle">
        网关多渠道调度：模型命中白名单 → 加权随机选渠道 → 失败熔断转移 · 数据源 /api/channels
      </p>

      <div className="row" style={{ marginBottom: '16px' }}>
        <button className="btn-primary" onClick={openCreate}>
          <Icon name="plus" size={14} />
          新建渠道
        </button>
        {err && <span style={{ color: 'var(--danger)', fontSize: '13px' }}>{err}</span>}
      </div>

      {loading ? (
        <div className="glass panel" style={{ padding: '40px', textAlign: 'center', color: 'var(--text-secondary)' }}>
          加载中…
        </div>
      ) : channels.length === 0 ? (
        <div className="glass panel" style={{ padding: '40px', textAlign: 'center', color: 'var(--text-secondary)' }}>
          还没有渠道，点「新建渠道」创建一个
        </div>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '10px' }}>
          {channels.map((c) => (
            <div className="glass" key={c.id} style={{ display: 'flex', alignItems: 'center', gap: '14px', padding: '14px 18px' }}>
              <span className={`dot ${c.enabled ? 'ok' : 'off'}`} />
              <div style={{ minWidth: '150px' }}>
                <div style={{ fontWeight: 600, fontSize: '14px' }}>{c.name}</div>
                <div style={{ fontSize: '11.5px', color: 'var(--text-muted)', display: 'flex', alignItems: 'center', gap: 6, marginTop: 2 }}>
                  {providerLabel(c.provider)}
                  {c.api_key_set ? (
                    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 3, color: 'var(--ok)', border: '1px solid rgba(74,222,128,0.3)', background: 'rgba(74,222,128,0.08)', borderRadius: 6, padding: '0 5px', fontSize: 10.5 }}>
                      <Icon name="shield" size={9} /> 已配置
                    </span>
                  ) : (
                    <span style={{ color: 'var(--text-muted)', border: '1px solid rgba(255,255,255,0.12)', borderRadius: 6, padding: '0 5px', fontSize: 10.5 }}>
                      未配置密钥
                    </span>
                  )}
                </div>
              </div>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontSize: '12.5px', color: 'var(--text-secondary)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {c.base_url || '(默认地址)'}
                </div>
                <div style={{ fontSize: '12px', color: 'var(--text-muted)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  models: {c.models || '(全部)'}
                </div>
              </div>
              <span style={{ fontSize: '12.5px', color: 'var(--text-secondary)', fontVariantNumeric: 'tabular-nums' }}>
                w={c.weight}
              </span>

              {/* 启用开关 */}
              <button
                onClick={() => toggle(c)}
                disabled={togglingId === c.id}
                style={{
                  width: 44,
                  height: 24,
                  borderRadius: 12,
                  border: 'none',
                  cursor: 'pointer',
                  background: c.enabled ? 'linear-gradient(135deg,#34d399,#22d3ee)' : 'rgba(255,255,255,0.12)',
                  position: 'relative',
                  transition: 'background .25s ease',
                  opacity: togglingId === c.id ? 0.6 : 1,
                }}
                title={c.enabled ? '点击禁用' : '点击启用'}
              >
                <motion.span
                  layout
                  transition={{ type: 'spring', stiffness: 500, damping: 30 }}
                  style={{
                    position: 'absolute',
                    top: 2,
                    width: 20,
                    height: 20,
                    borderRadius: '50%',
                    background: '#fff',
                    left: c.enabled ? 22 : 2,
                  }}
                />
              </button>

              <button className="icon-btn" style={{ width: 30, height: 30 }} onClick={() => openEdit(c)} title="编辑">
                <Icon name="edit" size={13} />
              </button>
              <button className="icon-btn" style={{ width: 30, height: 30, color: 'var(--text-secondary)' }} onClick={() => remove(c)} title="删除">
                <Icon name="trash" size={13} />
              </button>
            </div>
          ))}
        </div>
      )}

      {/* ===== 创建/编辑弹窗（Portal 到 body 顶层：外层 Portal、内层 AnimatePresence，exit 动画正常） ===== */}
      {createPortal(
        <AnimatePresence>
          {modal && (
            <motion.div
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            style={{
              position: 'fixed',
              inset: 0,
              background: 'rgba(2,6,23,0.6)',
              backdropFilter: 'blur(6px)',
              zIndex: 300, // 高于顶部导航（100），遮罩必须盖住导航栏
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              padding: 20,
            }}
            onClick={() => setModal(null)}
          >
            <motion.div
              initial={{ opacity: 0, scale: 0.92, y: 18 }}
              animate={{ opacity: 1, scale: 1, y: 0 }}
              exit={{ opacity: 0, scale: 0.92, y: 18 }}
              transition={{ type: 'spring', stiffness: 340, damping: 28 }}
              className="glass"
              style={{ width: 520, maxWidth: '100%', padding: 24, zIndex: 310 }}
              onClick={(e) => e.stopPropagation()}
            >
              <div style={{ fontWeight: 700, fontSize: 17, marginBottom: 18 }}>
                {modal.mode === 'create' ? '新建渠道' : '编辑渠道'}
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
                <label style={{ gridColumn: '1 / 2' }}>
                  <div className="muted" style={{ marginBottom: 6 }}>名称 *</div>
                  <input className="glass-input" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="如 DeepSeek 主渠道" />
                </label>
                <label style={{ gridColumn: '2 / 3' }}>
                  <div className="muted" style={{ marginBottom: 6 }}>Provider</div>
                  <select className="glass-input" value={form.provider} onChange={(e) => setForm({ ...form, provider: e.target.value })}>
                    <option value="openai">openai</option>
                    <option value="anthropic">anthropic</option>
                    <option value="deepseek">deepseek</option>
                  </select>
                </label>
                <label style={{ gridColumn: '1 / 3' }}>
                  <div className="muted" style={{ marginBottom: 6 }}>Base URL</div>
                  <input className="glass-input" value={form.base_url} onChange={(e) => setForm({ ...form, base_url: e.target.value })} placeholder="https://api.deepseek.com （留空用 provider 默认地址）" />
                </label>
                <label style={{ gridColumn: '1 / 3' }}>
                  <div className="muted" style={{ marginBottom: 6 }}>
                    API Key {modal?.mode === 'create' ? '（必填，加密后落库）' : '（留空 = 保持原密钥）'}
                  </div>
                  <input
                    className="glass-input"
                    type="password"
                    value={form.api_key}
                    onChange={(e) => setForm({ ...form, api_key: e.target.value })}
                    placeholder={modal?.mode === 'create' ? 'sk-xxx（AES-GCM 加密存储，永不外泄）' : 'sk-xxx（留空不修改）'}
                  />
                </label>
                <label style={{ gridColumn: '1 / 3' }}>
                  <div className="muted" style={{ marginBottom: 6 }}>模型白名单（逗号分隔）</div>
                  <input className="glass-input" value={form.models} onChange={(e) => setForm({ ...form, models: e.target.value })} placeholder="deepseek-chat, deepseek-reasoner（留空 = 全部模型）" />
                </label>
                <label style={{ gridColumn: '1 / 2' }}>
                  <div className="muted" style={{ marginBottom: 6 }}>权重（加权随机调度）</div>
                  <input className="glass-input" type="number" min={1} value={form.weight} onChange={(e) => setForm({ ...form, weight: Number(e.target.value) || 1 })} />
                </label>
                <label style={{ gridColumn: '2 / 3', display: 'flex', alignItems: 'flex-end', gap: 8, paddingBottom: 4 }}>
                  <input type="checkbox" checked={form.enabled} onChange={(e) => setForm({ ...form, enabled: e.target.checked })} />
                  <span className="muted" style={{ marginBottom: 0 }}>创建后立即启用</span>
                </label>
              </div>
              <div className="row" style={{ justifyContent: 'flex-end', marginTop: 20 }}>
                <button className="icon-btn" onClick={() => setModal(null)}>
                  取消
                </button>
                <button
                  className="btn-primary"
                  onClick={save}
                  disabled={saving || !form.name.trim()}
                >
                  {saving ? '保存中…' : '保存'}
                </button>
              </div>
            </motion.div>
          </motion.div>
          )}
        </AnimatePresence>,
        document.body,
      )}
    </div>
  )
}
