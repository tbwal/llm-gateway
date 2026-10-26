// 管理台 API 封装：对齐网关现有 REST API
// 用法示例见各页面；所有请求走相对路径 /api/*
// （dev 由 vite proxy 转发到 :8080，build 后与网关同源）

export interface UsageSummary {
  requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost: number
}

export interface UsageSummaryResp {
  days: number
  summary: UsageSummary
}

export interface DailyRow {
  date: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost: number
}

export interface DailyResp {
  days: number
  rows: DailyRow[]
}

export interface BreakdownRow {
  key: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost: number
}

export interface BreakdownResp {
  days: number
  group: string
  rows: BreakdownRow[]
}

export interface Channel {
  id: number
  name: string
  provider: string
  base_url: string
  models: string
  weight: number
  enabled: boolean
  api_key_set: boolean
  created_at: string
  updated_at: string
}

export interface AccessKey {
  id: number
  name: string
  scopes: string
  enabled: boolean
  last_used_at: string | null
  created_at: string
}

interface ChannelListResp {
  code: number
  message: string
  data: Channel[]
}

interface AccessKeyListResp {
  code: number
  message: string
  data: AccessKey[]
}

/** 创建 AccessKey 的响应：多一个 key 字段（明文仅返回这一次） */
export interface AccessKeyCreated extends AccessKey {
  key: string
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path)
  if (!res.ok) {
    throw new Error(`HTTP ${res.status}: ${path}`)
  }
  return (await res.json()) as T
}

export function fetchSummary(days = 7): Promise<UsageSummaryResp> {
  return getJSON(`/api/usage/summary?days=${days}`)
}

export function fetchDaily(days = 7): Promise<DailyResp> {
  return getJSON(`/api/usage/daily?days=${days}`)
}

export function fetchBreakdown(group: 'model' | 'channel', days = 7): Promise<BreakdownResp> {
  return getJSON(`/api/usage/breakdown?group=${group}&days=${days}`)
}

export async function fetchChannels(): Promise<Channel[]> {
  const resp = await getJSON<ChannelListResp>('/api/channels')
  return resp.data ?? []
}

// ===== AccessKey 管理 =====

export async function fetchAccessKeys(): Promise<AccessKey[]> {
  const resp = await getJSON<AccessKeyListResp>('/api/access-keys')
  return resp.data ?? []
}

/** 签发新 key；返回的 data.key 是明文，仅此一次 */
export async function createAccessKey(body: { name: string; scopes?: string }): Promise<AccessKeyCreated> {
  const res = await fetch('/api/access-keys', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  const resp = await res.json()
  if (resp.code !== 0) throw new Error(resp.message)
  return resp.data
}

/** 更新：改名 / 改范围 / 启停（不换 key） */
export async function updateAccessKey(
  id: number,
  body: { name?: string; scopes?: string; enabled?: boolean },
): Promise<void> {
  const res = await fetch(`/api/access-keys/${id}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  const resp = await res.json()
  if (resp.code !== 0) throw new Error(resp.message)
}

/** 删除（彻底吊销） */
export async function deleteAccessKey(id: number): Promise<void> {
  const res = await fetch(`/api/access-keys/${id}`, { method: 'DELETE' })
  const resp = await res.json()
  if (resp.code !== 0) throw new Error(resp.message)
}

// ===== 系统状态 =====

export interface BreakerState {
  channel_id: number
  channel_name: string
  state: 'closed' | 'open' | 'half_open'
  failures: number
  opened_at: string
  cooldown_remaining_sec: number
}

export interface SystemStatus {
  server: { started_at: string; uptime: string }
  rate_limit: { enabled: boolean; store: string; limit_per_min: number; persisted?: string; redis_addr: string }
  security: { secret_key_set: boolean }
  cache: { enabled_channels: number; total_channels: number; last_refresh: string }
  access_keys: number
  breakers: BreakerState[]
}

interface SystemStatusResp {
  code: number
  message: string
  data: SystemStatus
}

export async function fetchSystemStatus(): Promise<SystemStatus> {
  const resp = await getJSON<SystemStatusResp>('/api/system/status')
  return resp.data
}

// ===== 限流设置（系统状态页可改、重启保留） =====

export interface RateLimitState {
  enabled: boolean
  store: string
  limit_per_min: number
  persisted?: string
  redis_addr: string
}

interface RateLimitResp {
  code: number
  message: string
  data: RateLimitState
}

export async function fetchRateLimit(): Promise<RateLimitState> {
  const resp = await getJSON<RateLimitResp>('/api/system/ratelimit')
  return resp.data
}

/** 运行时改窗口上限（立即生效 + 落库持久化）；参数错误后端返回真 HTTP 400 */
export async function updateRateLimit(perMin: number): Promise<RateLimitState> {
  const res = await fetch('/api/system/ratelimit', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ per_min: perMin }),
  })
  if (!res.ok) {
    let msg = `HTTP ${res.status}`
    try {
      const body = await res.json()
      if (body?.error) msg = body.error
    } catch {
      /* 非 JSON 错误体，用默认文案 */
    }
    throw new Error(msg)
  }
  const resp = (await res.json()) as RateLimitResp
  return resp.data
}

// ===== 请求日志 =====

export interface UsageLogItem {
  id: number
  request_id?: string // 链路 ID（req_xxx）：同一请求跨日志可关联（失败/成功对应同一条）
  channel_id: number
  channel_name: string
  provider: string
  model: string
  access_key_id?: number // 调用方 AccessKey id（0 = 匿名）
  access_key_name?: string // 调用方备注名（冗余展示）
  client_ip?: string // 调用方 IP（IPv6 最长 45）
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost: number
  status?: number // 失败也落库：200=成功，4xx/5xx=失败
  error?: string // 失败时的错误摘要（成功为空）
  duration_ms?: number // 网关总耗时（毫秒）
  first_response_ms?: number // 首响应延迟（流式=首个 token；非流式=上游响应到达）
  created_at: string
}

export interface UsageLogsResp {
  total: number
  page: number
  page_size: number
  items: UsageLogItem[]
}

export interface LogsQuery {
  page?: number
  page_size?: number
  model?: string
  channel_id?: number
  provider?: string
  request_id?: string // 链路 ID 精确筛选（排障：输入 req_xxx 拉出该请求全部记录）
  access_key_name?: string // 调用方名称精确筛选
  client_ip?: string // 客户端 IP 精确筛选
  days?: number
}

export async function fetchUsageLogs(q: LogsQuery = {}): Promise<UsageLogsResp> {
  const params = new URLSearchParams()
  if (q.page) params.set('page', String(q.page))
  if (q.page_size) params.set('page_size', String(q.page_size))
  if (q.model) params.set('model', q.model)
  if (q.channel_id) params.set('channel_id', String(q.channel_id))
  if (q.provider) params.set('provider', q.provider)
  if (q.request_id) params.set('request_id', q.request_id)
  if (q.access_key_name) params.set('access_key_name', q.access_key_name)
  if (q.client_ip) params.set('client_ip', q.client_ip)
  if (q.days) params.set('days', String(q.days))
  const qs = params.toString()
  // ⚠️ 与 usage 看板其他接口一致：返回体无 code/data 包装（{total,page,page_size,items}）
  return getJSON<UsageLogsResp>(`/api/usage/logs${qs ? `?${qs}` : ''}`)
}

/** 网关返回的费用单位是元，页面展示统一格式化 */
export function fmtCost(v: number): string {
  return `¥${Number.isInteger(v) ? v.toFixed(2) : v.toFixed(4)}`
}

/** 大数字千分位 */
export function fmtNum(v: number): string {
  return new Intl.NumberFormat('zh-CN').format(Math.round(v))
}

/* ================= 日志文件（请求日志页"日志文件"区） ================= */

export interface LogFileMeta {
  date: string // 日期目录名；全局日志为 "global"
  name: string // requestID.log / global.log
  rel_path: string // 下载参数：请求日志为 "2026-10-25/req_xxx.log"，全局日志固定 "global"
  size: number // 字节
  mod_time: string // 修改时间（展示用）
}

export interface LogFilesResp {
  global: LogFileMeta | null
  files: LogFileMeta[] // 请求日志文件，日期倒序
  dir: string // 服务端日志根目录（展示用）
}

export async function fetchLogFiles(): Promise<LogFilesResp> {
  return getJSON<LogFilesResp>('/api/logs/files')
}

/** 下载日志文件：后端以附件返回 → blob → 浏览器触发保存 */
export async function downloadLogFile(relPath: string, fileName: string): Promise<void> {
  const resp = await fetch(`/api/logs/files/download?file=${encodeURIComponent(relPath)}`)
  if (!resp.ok) {
    const body = await resp.json().catch(() => null)
    throw new Error(body?.message || `下载失败（HTTP ${resp.status}）`)
  }
  const blob = await resp.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = fileName
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

/** 日志文件大小格式化：B → KB → MB */
export function fmtFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}
