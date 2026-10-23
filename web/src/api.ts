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

interface ChannelListResp {
  code: number
  message: string
  data: Channel[]
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

/** 网关返回的费用单位是元，页面展示统一格式化 */
export function fmtCost(v: number): string {
  return `¥${Number.isInteger(v) ? v.toFixed(2) : v.toFixed(4)}`
}

/** 大数字千分位 */
export function fmtNum(v: number): string {
  return new Intl.NumberFormat('zh-CN').format(Math.round(v))
}
