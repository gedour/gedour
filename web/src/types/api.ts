import type { components, operations } from "@/api/schema.gen";

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
  actor?: string;
  target_type?: string;
  from?: string;
  to?: string;
}

// Phase 3 resources: generated from api/openapi.json (make openapi), never written by hand.
type Schemas = components["schemas"];
export type Watch = Schemas["Watch"];
export type WatchKind = Watch["kind"];
export type WatchConfig = Watch["config"];
export type Permission = Watch["permission"];
export type CreateWatchBody = Schemas["CreateWatchInputBody"];
export type UpdateWatchBody = Schemas["UpdateWatchInputBody"];
export type Share = Schemas["Share"];
export type SharePermission = Share["permission"];
export type Credential = Schemas["Credential"];
export type CredentialKind = Credential["kind"];
export type CreateCredentialBody = Schemas["CreateCredentialInputBody"];
export type AuditActor = Schemas["Actor"];
export type ProblemDetail = Schemas["ErrorDetail"];
export type WatchEvent = Schemas["Event"];
export type StateSnapshot = Schemas["StateSnapshot"];
export type RunStatus = NonNullable<Watch["last_status"]>;

export interface EventsPage {
  items: WatchEvent[];
  next_cursor?: string;
}

export type SharedFilter = NonNullable<
  NonNullable<operations["list-watches"]["parameters"]["query"]>["shared"]
>;

export interface WatchFilters {
  kind?: WatchKind;
  q?: string;
  shared?: SharedFilter;
}
