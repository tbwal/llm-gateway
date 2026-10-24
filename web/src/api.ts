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
  state: 'closed' | 'open' | 'half_open'
  failures: number
  opened_at: string
  cooldown_remaining_sec: number
}

export interface SystemStatus {
  server: { started_at: string; uptime: string }
  rate_limit: { enabled: boolean; store: string; limit_per_min: number; redis_addr: string }
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

/** 网关返回的费用单位是元，页面展示统一格式化 */
export function fmtCost(v: number): string {
  return `¥${Number.isInteger(v) ? v.toFixed(2) : v.toFixed(4)}`
}

/** 大数字千分位 */
export function fmtNum(v: number): string {
  return new Intl.NumberFormat('zh-CN').format(Math.round(v))
}
