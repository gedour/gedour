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
  is_admin: boolean;
  /** The admin who signed in, while the session acts as the user above (006-AC15); null otherwise. */
  acting_as: { id: string; email: string; display_name: string } | null;
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
  /** User the actor was acting as (006-AC16). */
  on_behalf_of: string | null;
  on_behalf_of_name: string;
  /** Key the change was made with (007-AC26); its name is null for others than its owner, and once the key is gone. */
  api_key_id: string | null;
  api_key_name: string | null;
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
export type ApiKey = Schemas["APIKey"];
export type ApiKeyCreated = Schemas["APIKeyCreated"];
export type ApiKeyScope = ApiKey["scope"];
export type ApiKeyDuration = Schemas["CreateAPIKeyInputBody"]["duration"];
export type CreateApiKeyBody = Schemas["CreateAPIKeyInputBody"];
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

// Phase 5 resources.
export type Channel = Schemas["Channel"];
export type ChannelKind = Channel["type"];
export type CreatedChannel = Schemas["CreatedChannel"];
export type ChannelType = Schemas["ChannelType"];
export type CreateChannelBody = Schemas["CreateChannelInputBody"];
export type UpdateChannelBody = Schemas["UpdateChannelInputBody"];
export type Rule = Schemas["Rule"];
export type CreateRuleBody = Schemas["CreateRuleInputBody"];
export type UpdateRuleBody = Schemas["UpdateRuleInputBody"];
export type PreviewItem = Schemas["PreviewItem"];
export type Delivery = Schemas["Delivery"];
export type DeliveryStatus = Delivery["status"];
export type InboxItem = Schemas["InboxItem"];
export type InboxState = NonNullable<
  NonNullable<operations["list-inbox"]["parameters"]["query"]>["state"]
>;
export type InboxAction = "read" | "unread" | "archive" | "unarchive";

export interface InboxPage {
  items: InboxItem[];
  next_cursor?: string;
}

export interface DeliveriesPage {
  items: Delivery[];
  next_cursor?: string;
}

export interface DeliveryFilters {
  channel_id?: string;
  status?: DeliveryStatus;
}
export type FilterReference = Schemas["Ref"];

// Phase 6 resources.
export type Script = Schemas["Script"];
export type ScriptSummary = Schemas["ScriptSummary"];
export type CreateScriptBody = Schemas["CreateScriptInputBody"];
export type UpdateScriptBody = Schemas["UpdateScriptInputBody"];
export type SampleEvent = Schemas["SampleEvent"];
export type DryRun = Schemas["DryRun"];
export type DryRunFetch = Schemas["Fetch"];
/** The event of a dry run: a real one the caller can read, or a made-up sample. */
export type DryRunEvent = { event_id: number } | { sample: SampleEvent };

// Spec 006 resources.
export type AdminUser = Schemas["AdminUser"];
export type AdminUserStatus = AdminUser["status"];
export type AdminWatch = Schemas["AdminWatch"];
export type TransferResult = Schemas["TransferResult"];
export type PurgePreview = Schemas["PurgePreviewOutputBody"];
