// Mirrors server.BuildInfo (GET /api/v1/version).
export interface VersionInfo {
  version: string;
  commit: string;
  build_timestamp: string;
  builder: string;
  project_url: string;
}

// GET /api/v1/me
export interface Me {
  id: string;
  email: string;
  display_name: string;
  csrf_token: string;
  dev_mode: boolean;
}

// audit.Item (GET /api/v1/audit)
export interface AuditItem {
  id: number;
  at: string;
  actor_user_id: string | null;
  actor_name: string;
  action: string;
  target_type: string;
  target_id: string;
  before: unknown;
  after: unknown;
  request_id: string;
}

export interface AuditPage {
  items: AuditItem[];
  next_cursor?: string;
}

export interface AuditFilters {
  target_type?: string;
  from?: string;
  to?: string;
}
