// Shapes of the JSON the backend returns (internal/api/*.go).

export interface User {
  id: number
  username: string
  is_admin: boolean
}

export type WorkspaceState = 'offline' | 'starting' | 'running' | 'stopped'

/** `none` means the image declares no healthcheck. */
export type Health = 'none' | 'starting' | 'healthy' | 'unhealthy'

export interface Usage {
  /** 100 = one full core. */
  cpu_percent: number
  mem_bytes: number
  mem_limit_bytes: number
  pids: number
}

export interface WorkspaceStatus {
  state: WorkspaceState
  health: Health
  mem_limit_mb: number
  cpu_limit: number
  last_active_at: string | null
  last_stopped_at: string | null
  usage: Usage | null
  /** Present only while the container is starting or running. */
  neko_password?: string
}

/** What GET /api/me reports about the workspace (database only). */
export interface MeWorkspace {
  state: WorkspaceState
  mem_limit_mb: number
  cpu_limit: number
  last_active_at: string | null
}
