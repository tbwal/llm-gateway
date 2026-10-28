import { useCallback, useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { motion, AnimatePresence } from 'framer-motion'
import {
  fetchModelPricing,
  createModelPricing,
  updateModelPricing,
  deleteModelPricing,
  type ModelPricing,
} from '../api'
import { Icon } from '../components/Icon'

/**
 * 模型单价管理页：CRUD + 启停
 *
 * 计费口径：input/output 单价单位是「元 / 1K token」
 * 改完单价后自动刷新内存缓存（后端 handler 里调了 pricing.Refresh()）
 * 新模型调用时按缓存里的价算钱，不用重启
 */
export default function ModelPricingPage({ refreshTick }: { refreshTick: number }) {
  const [rows, setRows] = useState<ModelPricing[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [modal, setModal] = useState<{ mode: 'create' | 'edit'; id?: number } | null>(null)
  const [form, setForm] = useState({ model_name: '', input_price: '', output_price: '', enabled: true })
  const [saving, setSaving] = useState(false)
  const [confirmDel, setConfirmDel] = useState<ModelPricing | null>(null) // 删除确认弹窗

  const load = useCallback(async () => {
    try {
      setErr('')
      setRows(await fetchModelPricing())
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
    setForm({ model_name: '', input_price: '', output_price: '', enabled: true })
    setModal({ mode: 'create' })
  }
  const openEdit = (r: ModelPricing) => {
    setForm({
      model_name: r.model_name,
      input_price: String(r.input_price),
      output_price: String(r.output_price),
      enabled: r.enabled,
    })
    setModal({ mode: 'edit', id: r.id })
  }

  const save = async () => {
    if (!form.model_name.trim()) {
      alert('模型名不能为空')
      return
    }
    const input = parseFloat(form.input_price)
    const output = parseFloat(form.output_price)
    if (isNaN(input) || isNaN(output) || input < 0 || output < 0) {
      alert('单价必须是 ≥0 的数字')
      return
    }
    setSaving(true)
    try {
      if (modal?.mode === 'create') {
        await createModelPricing({
          model_name: form.model_name.trim(),
          input_price: input,
          output_price: output,
          enabled: form.enabled,
        })
      } else {
        await updateModelPricing(modal!.id!, {
          input_price: input,
          output_price: output,
          enabled: form.enabled,
        })
      }
      setModal(null)
      await load()
    } catch (e) {
      alert(e instanceof Error ? e.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const remove = async (r: ModelPricing) => {
    setConfirmDel(r) // 打开自定义确认弹窗
  }

  const doDelete = async () => {
    if (!confirmDel) return
    try {
      await deleteModelPricing(confirmDel.id)
      setConfirmDel(null)
      await load()
    } catch (e) {
      alert(e instanceof Error ? e.message : '删除失败')
    }
  }

  const toggle = async (r: ModelPricing) => {
    try {
      await updateModelPricing(r.id, { enabled: !r.enabled })
      await load()
    } catch (e) {
      alert(e instanceof Error ? e.message : '操作失败')
    }
  }

  return (
    <div>
      <h2>模型单价</h2>
      <p className="subtitle">
        单位：元 / 1K token。改完立即生效（内存缓存热更新） · 数据源 /api/model-pricing
      </p>

      <div className="row" style={{ marginBottom: '16px' }}>
        <button className="btn-primary" onClick={openCreate}>
          <Icon name="plus" size={14} />
          新增单价
        </button>
        {err && <span style={{ color: 'var(--danger)', fontSize: '13px' }}>{err}</span>}
      </div>

      {loading ? (
        <div className="glass panel" style={{ padding: 40, textAlign: 'center', color: 'var(--text-secondary)' }}>
          加载中…
        </div>
      ) : rows.length === 0 ? (
        <div className="glass panel" style={{ padding: 40, textAlign: 'center', color: 'var(--text-secondary)' }}>
          暂无单价配置，点「新增单价」创建
        </div>
      ) : (
        <div className="glass panel" style={{ overflow: 'hidden', padding: 0 }}>
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ borderBottom: '1px solid rgba(255,255,255,0.08)' }}>
                <th style={{ padding: '14px 20px', textAlign: 'left', fontWeight: 600, fontSize: 13, color: 'var(--text-secondary)' }}>模型名</th>
                <th style={{ padding: '14px 20px', textAlign: 'right', fontWeight: 600, fontSize: 13, color: 'var(--text-secondary)' }}>输入价（元/1K）</th>
                <th style={{ padding: '14px 20px', textAlign: 'right', fontWeight: 600, fontSize: 13, color: 'var(--text-secondary)' }}>输出价（元/1K）</th>
                <th style={{ padding: '14px 20px', textAlign: 'left', fontWeight: 600, fontSize: 13, color: 'var(--text-secondary)' }}>状态</th>
                <th style={{ padding: '14px 20px', textAlign: 'right', fontWeight: 600, fontSize: 13, color: 'var(--text-secondary)' }}>操作</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.id} style={{ opacity: r.enabled ? 1 : 0.5, borderBottom: '1px solid rgba(255,255,255,0.04)' }}>
                  <td style={{ padding: '14px 20px', fontWeight: 500 }}>{r.model_name}</td>
                  <td style={{ padding: '14px 20px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{r.input_price.toFixed(4)}</td>
                  <td style={{ padding: '14px 20px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{r.output_price.toFixed(4)}</td>
                  <td style={{ padding: '14px 20px' }}>
                    <span
                      className="badge"
                      style={{
                        color: r.enabled ? 'var(--ok)' : 'var(--text-muted)',
                        border: `1px solid ${r.enabled ? 'rgba(74,222,128,0.3)' : 'rgba(255,255,255,0.12)'}`,
                        background: r.enabled ? 'rgba(74,222,128,0.08)' : 'transparent',
                        cursor: 'pointer',
                      }}
                      onClick={() => toggle(r)}
                      title="点击启停"
                    >
                      {r.enabled ? '启用' : '停用'}
                    </span>
                  </td>
                  <td style={{ padding: '14px 20px', textAlign: 'right', whiteSpace: 'nowrap' }}>
                    <button className="icon-btn" style={{ width: 36, height: 36 }} onClick={() => openEdit(r)} title="编辑">
                      <Icon name="edit" size={16} />
                    </button>
                    <button className="icon-btn" style={{ width: 36, height: 36, color: 'var(--text-secondary)', marginLeft: 8 }} onClick={() => remove(r)} title="删除">
                      <Icon name="trash" size={16} />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* 弹窗：新增/编辑 */}
      {modal && createPortal(
        <AnimatePresence>
          <motion.div
            className="modal-mask"
            initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
            onClick={() => setModal(null)}
          >
            <motion.div
              className="modal glass"
              initial={{ y: 20, opacity: 0 }} animate={{ y: 0, opacity: 1 }} exit={{ y: 20, opacity: 0 }}
              onClick={(e) => e.stopPropagation()}
            >
              <h3>{modal.mode === 'create' ? '新增单价' : `编辑：${form.model_name}`}</h3>

              {modal.mode === 'create' && (
                <label className="field">
                  <span>模型名</span>
                  <input
                    value={form.model_name}
                    onChange={(e) => setForm({ ...form, model_name: e.target.value })}
                    placeholder="如 gpt-4o-mini"
                  />
                </label>
              )}

              <label className="field">
                <span>输入单价（元 / 1K token）</span>
                <input
                  type="number" step="0.0001" min="0"
                  value={form.input_price}
                  onChange={(e) => setForm({ ...form, input_price: e.target.value })}
                />
              </label>

              <label className="field">
                <span>输出单价（元 / 1K token）</span>
                <input
                  type="number" step="0.0001" min="0"
                  value={form.output_price}
                  onChange={(e) => setForm({ ...form, output_price: e.target.value })}
                />
              </label>

              <label className="field row">
                <input
                  type="checkbox"
                  checked={form.enabled}
                  onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
                />
                <span>启用</span>
              </label>

              <div className="modal-actions">
                <button className="btn" onClick={() => setModal(null)}>取消</button>
                <button className="btn-primary" onClick={save} disabled={saving}>
                  {saving ? '保存中…' : '保存'}
                </button>
              </div>
            </motion.div>
          </motion.div>
        </AnimatePresence>,
        document.body,
      )}

      {/* 删除确认弹窗 */}
      {confirmDel && createPortal(
        <AnimatePresence>
          <motion.div
            className="modal-mask"
            initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
            onClick={() => setConfirmDel(null)}
          >
            <motion.div
              className="modal glass"
              initial={{ y: 20, opacity: 0 }} animate={{ y: 0, opacity: 1 }} exit={{ y: 20, opacity: 0 }}
              onClick={(e) => e.stopPropagation()}
              style={{ maxWidth: 380 }}
            >
              <h3 style={{ marginTop: 0 }}>确认删除</h3>
              <p style={{ margin: '12px 0 20px', color: 'var(--text-secondary)', fontSize: 14, lineHeight: 1.6 }}>
                确定删除 <code>{confirmDel.model_name}</code> 的单价配置？<br />
                删除后该模型的调用将按默认价（0.002/0.002 元/1K）计费。
              </p>
              <div className="modal-actions">
                <button className="btn" onClick={() => setConfirmDel(null)}>取消</button>
                <button
                  className="btn-primary"
                  onClick={doDelete}
                  style={{ background: 'var(--danger)', borderColor: 'var(--danger)' }}
                >
                  确认删除
                </button>
              </div>
            </motion.div>
          </motion.div>
        </AnimatePresence>,
        document.body,
      )}
    </div>
  )
}
