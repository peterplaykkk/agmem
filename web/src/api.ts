// agmem API client — 按 internal/api/server.go 契约对接。
// 注意：后端 store 结构体无 json tag，字段序列化为 Go 原生名（ID/KBID/…），
// sql.NullString 序列化为 {"String":..,"Valid":..}。此处按该形状定义类型。
export type TaskStatus =
  | 'pending'
  | 'embedded'
  | 'cleaning'
  | 'cleaned'
  | 'failed';

export const TASK_STATUSES: TaskStatus[] = [
  'pending',
  'embedded',
  'cleaning',
  'cleaned',
  'failed',
];

export interface NullString {
  String: string;
  Valid: boolean;
}

export interface TaskItem {
  ID: number;
  TaskID: number;
  KBID: number;
  DirID: number;
  Status: TaskStatus;
  OriginalText: string;
  CleanText: NullString | null;
  Error: NullString | null;
  PointID: NullString | null;
  CleanPointID: NullString | null;
}

export interface Task {
  ID: number;
  KBID: number;
  DirID: number;
  Status: TaskStatus;
  Items: TaskItem[];
}

export interface PointsResponse {
  kb: string;
  status: string;
  /** 以下为服务端分页契约（page 1 基，服务端规范化）；后端未部署时缺省 → 前端降级为「仅展示前 N 条」。 */
  page?: number;
  page_size?: number;
  total?: number;
  items: TaskItem[];
}

export interface VersionResponse {
  kb: string;
  version: number;
}

export interface HealthResponse {
  status: string;
  version: string;
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function parseError(res: Response): Promise<ApiError> {
  let msg = `${res.status} ${res.statusText}`;
  try {
    const body = (await res.json()) as { error?: string };
    if (body && typeof body.error === 'string') msg = body.error;
  } catch {
    /* non-JSON error body */
  }
  return new ApiError(res.status, msg);
}

async function request<T>(
  path: string,
  init?: RequestInit & { signal?: AbortSignal },
): Promise<T> {
  const res = await fetch(path, {
    credentials: 'same-origin',
    headers: init?.body != null ? { 'Content-Type': 'application/json' } : undefined,
    ...init,
  });
  if (!res.ok) throw await parseError(res);
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  return request<T>(path, { signal });
}

export function postJSON<T>(path: string, body: unknown, signal?: AbortSignal): Promise<T> {
  return request<T>(path, { method: 'POST', body: JSON.stringify(body), signal });
}

// —— 契约端点映射 ——

export const api = {
  health: (signal?: AbortSignal) => getJSON<HealthResponse>('/api/healthz', signal),

  // 已实现
  createKB: (name: string) => postJSON<{ id: number; name: string }>('/api/kb', { name }),
  createDir: (kb: string, name: string) =>
    postJSON<{ id: number; kb: string; dir: string }>(`/api/kb/${encodeURIComponent(kb)}/dir`, { name }),
  uploadDocs: (
    kb: string,
    body: { dir: string; texts?: string[]; files?: { name: string; text: string }[] },
  ) => postJSON<{ task_id?: number }>(`/api/kb/${encodeURIComponent(kb)}/docs`, body),
  task: (kb: string, taskID: number, signal?: AbortSignal) =>
    getJSON<Task>(`/api/kb/${encodeURIComponent(kb)}/tasks/${taskID}`, signal),
  points: (kb: string, status: string, page: number, pageSize: number, signal?: AbortSignal) =>
    getJSON<PointsResponse>(
      `/api/kb/${encodeURIComponent(kb)}/points?status=${encodeURIComponent(status)}&page=${page}&page_size=${pageSize}`,
      signal,
    ),
  version: (kb: string, signal?: AbortSignal) =>
    getJSON<VersionResponse>(`/api/version/${encodeURIComponent(kb)}`, signal),

  // 以下端点后端骨架尚未提供（501 / 不存在），前端按契约调用并优雅降级：
  //   GET /api/kb                  — 知识库列表（未实现）
  //   GET /api/kb/{kb}/dirs        — 目录列表（未实现）
  //   GET /api/me                  — 会话身份（未实现）
  listKBs: (signal?: AbortSignal) => getJSON<{ kbs: { name: string; owner: string }[] }>('/api/kb', signal),
  listDirs: (kb: string, signal?: AbortSignal) =>
    getJSON<{ dirs: { name: string }[] }>(`/api/kb/${encodeURIComponent(kb)}/dirs`, signal),
};

/** 端点未实现（501）或未配置（503）→ 页面渲染「后端接口未就绪」降级态。 */
export function isNotReady(e: ApiError | null): boolean {
  return e != null && (e.status === 501 || e.status === 503);
}
