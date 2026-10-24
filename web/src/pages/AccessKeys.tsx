import { useCallback, useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { motion, AnimatePresence } from 'framer-motion'
import {
  fetchAccessKeys,
  createAccessKey,
  updateAccessKey,
  deleteAccessKey,
  type AccessKey,
} from '../api'
import { Icon } from '../components/Icon'

/**
 * AccessKey 管理页：签发 / 启停 / 删除
 *
 * 安全设计（面试讲点）：
 *  - 密钥明文只在"签发"那一刻显示一次（复制后请立即保存）
 *  - 之后列表永远拿不到明文/哈希——数据库泄露也无法冒充调用（SHA-256 哈希存库）
 *  - 启停 = 即时吊销：停用后该 key 立即 401，不用重新签发
 *  - scopes 限定了这把 key 能调哪些模型
 */
export default function AccessKeys({ refreshTick }: { refreshTick: number }) {
  const [keys, setKeys] = useState<AccessKey[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [modal, setModal] = useState<{ mode: 'create' | 'edit'; id?: number } | null>(null)
  const [form, setForm] = useState({ name: '', scopes: '' })
  const [createdKey, setCreatedKey] = useState('') // 创建成功后的明文（仅展示一次）
  const [copied, setCopied] = useState(false)
  const [saving, setSaving] = useState(false)
  const [togglingId, setTogglingId] = useState<number | null>(null)

  const load = useCallback(async () => {
    try {
      setErr('')
      setKeys(await fetchAccessKeys())
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
    setForm({ name: '', scopes: '' })
    setCreatedKey('')
    setCopied(false)
    setModal({ mode: 'create' })
  }
  const openEdit = (k: AccessKey) => {
    setCreatedKey('')
    setForm({ name: k.name, scopes: k.scopes })
    setModal({ mode: 'edit', id: k.id })
  }

  const save = async () => {
    if (!form.name.trim()) return
    setSaving(true)
    try {
      if (modal?.mode === 'edit') {
        await updateAccessKey(modal.id!, { name: form.name.trim(), scopes: form.scopes.trim() })
        setModal(null)
      } else {
        const created = await createAccessKey({ name: form.name.trim(), scopes: form.scopes.trim() })
        setCreatedKey(created.key) // 进入"明文展示态"，停留在这个弹窗
      }
      await load()
    } catch (e) {
      setErr(e instanceof Error ? e.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  // 启用/禁用开关：停用 = 即时吊销（不用重新签发）
  const toggle = async (k: AccessKey) => {
    setTogglingId(k.id)
    try {
      await updateAccessKey(k.id, { enabled: !k.enabled })
      await load()
    } catch (e) {
      setErr(e instanceof Error ? e.message : '切换失败')
    } finally {
      setTogglingId(null)
    }
  }

  const remove = async (k: AccessKey) => {
    if (!confirm(`确认删除「${k.name}」？删除后该 key 立即失效，不可恢复。`)) return
    try {
      await deleteAccessKey(k.id)
      await load()
    } catch (e) {
      setErr(e instanceof Error ? e.message : '删除失败')
    }
  }

  const copyKey = async () => {
    try {
      await navigator.clipboard.writeText(createdKey)
      setCopied(true)
      setTimeout(() => setCopied(false), 1600)
    } catch {
      setErr('复制失败，请手动选择复制')
    }
  }

  return (
    <div>
      <h2>AccessKey 管理</h2>
      <p className="subtitle">
        调用方凭证：Bearer 鉴权 → 可信身份限流 · 明文仅签发时显示一次，库中仅存 SHA-256 哈希 · 数据源
        /api/access-keys
      </p>

      <div className="row" style={{ marginBottom: '16px' }}>
        <button className="btn-primary" onClick={openCreate}>
          <Icon name="plus" size={14} />
          签发 AccessKey
        </button>
        {err && <span style={{ color: 'var(--danger)', fontSize: '13px' }}>{err}</span>}
      </div>

      {loading ? (
        <div className="glass card-pad" style={{ opacity: 0.7 }}>
          加载中…
        </div>
      ) : keys.length === 0 ? (
        <div className="glass card-pad" style={{ opacity: 0.7 }}>
          还没有签发任何 AccessKey。点「签发 AccessKey」创建一把，用它调用 /v1/chat/completions。
        </div>
      ) : (
        <div className="glass card-pad" style={{ padding: '4px 0' }}>
          {keys.map((k) => (
            <div
              key={k.id}
              className="row"
              style={{
                padding: '14px 20px',
                borderBottom: '1px solid var(--border)',
                gap: 16,
                opacity: k.enabled ? 1 : 0.55,
              }}
            >
              {/* 名称 + 徽标 */}
              <div style={{ minWidth: 180, flex: '1 1 200px' }}>
                <div className="row" style={{ gap: 8 }}>
                  <span style={{ fontWeight: 600 }}>{k.name}</span>
                  {k.enabled ? (
                    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 3, color: 'var(--ok)', border: '1px solid rgba(74,222,128,0.3)', background: 'rgba(74,222,128,0.08)', borderRadius: 6, padding: '0 5px', fontSize: 10.5 }}>
                      <Icon name="power" size={9} /> 启用
                    </span>
                  ) : (
                    <span style={{ color: 'var(--text-muted)', border: '1px solid rgba(255,255,255,0.12)', borderRadius: 6, padding: '0 5px', fontSize: 10.5 }}>
                      已停用
                    </span>
                  )}
                </div>
                <div style={{ color: 'var(--text-secondary)', fontSize: 12, marginTop: 4 }}>
                  id={k.id} · 签发于 {new Date(k.created_at).toLocaleString('zh-CN')}
                  {k.last_used_at
                    ? ` · 最近使用 ${new Date(k.last_used_at).toLocaleString('zh-CN')}`
                    : ' · 从未使用'}
                </div>
              </div>

              {/* scopes 范围 */}
              <div style={{ flex: '1 1 220px', color: 'var(--text-secondary)', fontSize: 13 }}>
                <div style={{ color: 'var(--text-primary)', fontWeight: 500, marginBottom: 4 }}>可调用模型</div>
                <span
                  style={{
                    fontFamily: 'var(--mono)',
                    fontSize: 12,
                    wordBreak: 'break-all',
                    opacity: k.scopes ? 1 : 0.6,
                  }}
                >
                  {k.scopes || '*（全部模型）'}
                </span>
              </div>

              {/* 操作 */}
              <div className="row" style={{ gap: 6 }}>
                <button
                  className="icon-btn"
                  style={{ width: 30, height: 30 }}
                  title={k.enabled ? '停用（即时吊销）' : '恢复启用'}
                  onClick={() => toggle(k)}
                  disabled={togglingId === k.id}
                >
                  <Icon name="power" size={14} />
                </button>
                <button
                  className="icon-btn"
                  style={{ width: 30, height: 30, color: 'var(--text-secondary)' }}
                  title="编辑"
                  onClick={() => openEdit(k)}
                >
                  <Icon name="edit" size={13} />
                </button>
                <button
                  className="icon-btn"
                  style={{ width: 30, height: 30, color: 'var(--danger)' }}
                  title="删除"
                  onClick={() => remove(k)}
                >
                  <Icon name="trash" size={13} />
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* ===== 创建/编辑弹窗（Portal 到 body 顶层） ===== */}
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
                zIndex: 300,
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'center',
                padding: 20,
              }}
              onClick={() => !createdKey && setModal(null)}
            >
              <motion.div
                initial={{ opacity: 0, scale: 0.92, y: 18 }}
                animate={{ opacity: 1, scale: 1, y: 0 }}
                exit={{ opacity: 0, scale: 0.92, y: 18 }}
                transition={{ type: 'spring', stiffness: 340, damping: 28 }}
                className="glass"
                style={{ width: 540, maxWidth: '100%', padding: 24, zIndex: 310 }}
                onClick={(e) => e.stopPropagation()}
              >
                {/* 创建成功 → 明文展示态（仅此一次） */}
                {createdKey ? (
                  <>
                    <div className="row" style={{ gap: 8, marginBottom: 12 }}>
                      <Icon name="key" size={16} />
                      <h3 style={{ margin: 0 }}>签发成功</h3>
                    </div>
                    <p style={{ color: 'var(--text-secondary)', fontSize: 13, marginTop: 0 }}>
                      这是密钥<strong>唯一一次</strong>完整展示，关闭后无法再查看（库中只存哈希）。请立即复制保存。
                    </p>
                    <div
                      className="glass"
                      style={{
                        padding: '12px 14px',
                        fontFamily: 'var(--mono)',
                        fontSize: 13,
                        wordBreak: 'break-all',
                        userSelect: 'all',
                      }}
                    >
                      {createdKey}
                    </div>
                    <div className="row" style={{ marginTop: 16, gap: 8, justifyContent: 'flex-end' }}>
                      <button className="btn-primary" onClick={copyKey}>
                        <Icon name="copy" size={14} />
                        {copied ? '已复制' : '复制密钥'}
                      </button>
                      <button className="icon-btn" onClick={() => setModal(null)}>
                        我已保存，关闭
                      </button>
                    </div>
                  </>
                ) : (
                  <>
                    <div className="row" style={{ gap: 8, marginBottom: 16 }}>
                      <Icon name="key" size={16} />
                      <h3 style={{ margin: 0 }}>{modal.mode === 'create' ? '签发 AccessKey' : '编辑 AccessKey'}</h3>
                    </div>
                    <label>
                      <div className="muted" style={{ marginBottom: 6 }}>名称 *</div>
                      <input className="glass-input" placeholder="如：线上服务 / 面试演示" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
                    </label>
                    <label style={{ display: 'block', marginTop: 14 }}>
                      <div className="muted" style={{ marginBottom: 6 }}>scopes（允许的模型，逗号分隔）</div>
                      <input className="glass-input" placeholder="留空 = 全部模型；如 gpt-5, claude-3-5-sonnet" value={form.scopes} onChange={(e) => setForm({ ...form, scopes: e.target.value })} />
                    </label>
                    <div className="row" style={{ marginTop: 20, gap: 8, justifyContent: 'flex-end' }}>
                      <button className="icon-btn" onClick={() => setModal(null)}>
                        取消
                      </button>
                      <button className="btn-primary" onClick={save} disabled={saving || !form.name.trim()}>
                        {saving ? '提交中…' : modal.mode === 'create' ? '签发' : '保存'}
                      </button>
                    </div>
                  </>
                )}
              </motion.div>
            </motion.div>
          )}
        </AnimatePresence>,
        document.body,
      )}
    </div>
  )
}
