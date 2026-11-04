import { readStorage, writeStorage } from "@/lib/utils";
import type {
  AuditActor,
  Channel,
  ChannelType,
  CreateChannelBody,
  CreatedChannel,
  CreateRuleBody,
  CreateScriptBody,
  DeliveriesPage,
  DryRun,
  DryRunEvent,
  Delivery,
  DeliveryFilters,
  FilterReference,
  InboxAction,
  InboxPage,
  InboxState,
  PreviewItem,
  Rule,
  Script,
  ScriptSummary,
  UpdateChannelBody,
  UpdateScriptBody,
  UpdateRuleBody,
  AuditFilters,
  AuditPage,
  CreateCredentialBody,
  CreateWatchBody,
  Credential,
  CredentialKind,
  EventsPage,
  Me,
  ProblemDetail,
  Share,
  SharePermission,
  StateSnapshot,
  UpdateWatchBody,
  VersionInfo,
  Watch,
  WatchFilters,
} from "@/types/api";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    /** The problem's human detail, without the path and status prefix. */
    readonly detail = "",
    /** Field-level problems, e.g. location "body.config.repo". */
    readonly errors: ProblemDetail[] = [],
  ) {
    super(message);
  }
}

/** react-query retry policy: a 4xx is the server's final answer (404 must show at once), the rest retries 3 times. */
export function retryQuery(failures: number, err: unknown): boolean {
  if (err instanceof ApiError && err.status >= 400 && err.status < 500) return false;
  return failures < 3;
}

// Set by getMe; every mutation echoes it so the server can tell our pages from a forged form.
let csrfToken = "";

export const LOGIN_REDIRECT_KEY = "gedour.login.redirectedAt";
const LOGIN_LOOP_MS = 15_000;

// An object, not a bare function, so tests can spy on the redirect.
export const navigation = {
  toLogin(returnTo: string) {
    // A cookie the browser refuses (Secure over plain http) would bounce us off the provider forever.
    const last = Number(readStorage(LOGIN_REDIRECT_KEY) ?? 0);
    if (Date.now() - last < LOGIN_LOOP_MS) return;
    writeStorage(LOGIN_REDIRECT_KEY, String(Date.now()));
    window.location.assign(`/auth/login?return_to=${encodeURIComponent(returnTo)}`);
  },
  go(url: string) {
    window.location.assign(url);
  },
};

const SAFE = new Set(["GET", "HEAD", "OPTIONS"]);

// Title of the server's CSRF problem (auth.CSRFProblemTitle); any other 403 is final.
export const CSRF_PROBLEM_TITLE = "CSRF token invalid";

// A stream body is consumed by the first attempt; only these can be sent again.
function replayable(body: RequestInit["body"]): boolean {
  return (
    body === undefined ||
    body === null ||
    typeof body === "string" ||
    body instanceof FormData ||
    body instanceof URLSearchParams
  );
}

async function isCSRFProblem(res: Response): Promise<boolean> {
  try {
    const body = (await res.clone().json()) as { title?: string };
    return body.title === CSRF_PROBLEM_TITLE;
  } catch {
    return false;
  }
}

export async function request(
  path: string,
  init: RequestInit = {},
  retried = false,
): Promise<Response> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (!SAFE.has(method) && csrfToken) headers.set("X-CSRF-Token", csrfToken);
  const res = await fetch(path, { ...init, method, headers });
  if (
    res.status === 403 &&
    !SAFE.has(method) &&
    !retried &&
    replayable(init.body) &&
    (await isCSRFProblem(res))
  ) {
    // The token may be stale (session replaced in another tab): refresh it once, then retry once.
    try {
      await getMe();
    } catch (err) {
      // getMe already sent a 401 to the login page; the caller still learns which call failed.
      const why = err instanceof Error ? err.message : String(err);
      throw new ApiError(
        res.status,
        `${path}: HTTP ${res.status}: CSRF token refresh failed (${why})`,
      );
    }
    return request(path, init, true);
  }
  if (res.status === 401) {
    // 001-AC1: a lost session sends the browser to the provider, then back here.
    navigation.toLogin(window.location.pathname + window.location.search);
  }
  if (!res.ok) {
    let detail = `HTTP ${res.status}`;
    let body: { detail?: string; errors?: ProblemDetail[] } = {};
    try {
      body = (await res.json()) as typeof body;
      if (body.detail) detail = `${detail}: ${body.detail}`;
    } catch {
      // not JSON: keep the status alone
    }
    throw new ApiError(res.status, `${path}: ${detail}`, body.detail ?? "", body.errors ?? []);
  }
  return res;
}

async function sendJSON<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await request(path, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  return (res.status === 204 ? undefined : await res.json()) as T;
}

async function getJSON<T>(path: string): Promise<T> {
  return (await (await request(path)).json()) as T;
}

export function getVersion(): Promise<VersionInfo> {
  return getJSON<VersionInfo>("/api/v1/version");
}

export async function getMe(): Promise<Me> {
  const me = await getJSON<Me>("/api/v1/me");
  csrfToken = me.csrf_token;
  // Signed in: the next 401 is a fresh expiry, not a loop.
  writeStorage(LOGIN_REDIRECT_KEY, "0");
  return me;
}

/** Ends the session; resolves to where the browser must go next (the provider's logout, or the signed-out page). */
export async function logout(): Promise<string> {
  const { redirect } = (await (await request("/auth/logout", { method: "POST" })).json()) as {
    redirect: string;
  };
  return redirect;
}

export function listAudit(filters: AuditFilters, cursor: string, limit = 50): Promise<AuditPage> {
  const q = new URLSearchParams({ limit: String(limit) });
  for (const [k, v] of Object.entries(filters)) if (v) q.set(k, v);
  if (cursor) q.set("cursor", cursor);
  return getJSON<AuditPage>(`/api/v1/audit?${q}`);
}

export async function listAuditActors(): Promise<AuditActor[]> {
  return (await getJSON<{ items: AuditActor[] }>("/api/v1/audit/actors")).items;
}

const watchPath = (id: string) => `/api/v1/watches/${encodeURIComponent(id)}`;

export async function listWatches(filters: WatchFilters = {}): Promise<Watch[]> {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(filters)) if (v) q.set(k, v);
  return (await getJSON<{ items: Watch[] }>(`/api/v1/watches?${q}`)).items;
}

export const getWatch = (id: string) => getJSON<Watch>(watchPath(id));
export const createWatch = (body: CreateWatchBody) =>
  sendJSON<Watch>("POST", "/api/v1/watches", body);
export const updateWatch = (id: string, body: UpdateWatchBody) =>
  sendJSON<Watch>("PATCH", watchPath(id), body);
export const setWatchPaused = (id: string, paused: boolean) =>
  sendJSON<Watch>("POST", `${watchPath(id)}/${paused ? "pause" : "resume"}`);
export const deleteWatch = (id: string) => sendJSON<undefined>("DELETE", watchPath(id));
export const runWatch = (id: string) => sendJSON<Watch>("POST", `${watchPath(id)}/run`);

export function listWatchEvents(id: string, cursor: string, limit = 20): Promise<EventsPage> {
  const q = new URLSearchParams({ limit: String(limit) });
  if (cursor) q.set("cursor", cursor);
  return getJSON<EventsPage>(`${watchPath(id)}/events?${q}`);
}

export async function listWatchStates(id: string, limit = 20): Promise<StateSnapshot[]> {
  return (await getJSON<{ items: StateSnapshot[] }>(`${watchPath(id)}/states?limit=${limit}`))
    .items;
}

export async function listShares(watchId: string): Promise<Share[]> {
  return (await getJSON<{ items: Share[] }>(`${watchPath(watchId)}/shares`)).items;
}
export const addShare = (watchId: string, email: string, permission: SharePermission) =>
  sendJSON<Share>("POST", `${watchPath(watchId)}/shares`, { email, permission });
export const updateShare = (watchId: string, userId: string, permission: SharePermission) =>
  sendJSON<Share>("PUT", `${watchPath(watchId)}/shares/${encodeURIComponent(userId)}`, {
    permission,
  });
export const deleteShare = (watchId: string, userId: string) =>
  sendJSON<undefined>("DELETE", `${watchPath(watchId)}/shares/${encodeURIComponent(userId)}`);

const credentialPath = (id: string) => `/api/v1/credentials/${encodeURIComponent(id)}`;

export async function listCredentials(kind?: CredentialKind): Promise<Credential[]> {
  const q = kind ? `?kind=${kind}` : "";
  return (await getJSON<{ items: Credential[] }>(`/api/v1/credentials${q}`)).items;
}
export const createCredential = (body: CreateCredentialBody) =>
  sendJSON<Credential>("POST", "/api/v1/credentials", body);
export const rotateCredentialSecret = (id: string, secret: string) =>
  sendJSON<Credential>("PUT", `${credentialPath(id)}/secret`, { secret });
export const deleteCredential = (id: string) => sendJSON<undefined>("DELETE", credentialPath(id));

export function listInbox(state: InboxState, cursor: string, limit = 50): Promise<InboxPage> {
  const q = new URLSearchParams({ state, limit: String(limit) });
  if (cursor) q.set("cursor", cursor);
  return getJSON<InboxPage>(`/api/v1/inbox?${q}`);
}
export async function inboxCount(): Promise<number> {
  return (await getJSON<{ unread: number }>("/api/v1/inbox/count")).unread;
}
export const inboxAction = (action: InboxAction, ids: number[]) =>
  sendJSON<{ updated: number; unread: number }>("POST", `/api/v1/inbox/${action}`, { ids });

const channelPath = (id: string) => `/api/v1/channels/${encodeURIComponent(id)}`;

export async function listChannels(): Promise<Channel[]> {
  return (await getJSON<{ items: Channel[] }>("/api/v1/channels")).items;
}
export async function listChannelTypes(): Promise<ChannelType[]> {
  return (await getJSON<{ items: ChannelType[] }>("/api/v1/channel-types")).items;
}
export const createChannel = (body: CreateChannelBody) =>
  sendJSON<CreatedChannel>("POST", "/api/v1/channels", body);
export const updateChannel = (id: string, body: UpdateChannelBody) =>
  sendJSON<Channel>("PATCH", channelPath(id), body);
export const deleteChannel = (id: string) => sendJSON<undefined>("DELETE", channelPath(id));
/** Sends a test message; a script channel runs its script live and returns its logs. */
export const testChannel = (id: string) =>
  sendJSON<{ ok: boolean; error?: string; logs?: string[] | null }>(
    "POST",
    `${channelPath(id)}/test`,
  );

export function listDeliveries(filters: DeliveryFilters, cursor: string): Promise<DeliveriesPage> {
  const q = new URLSearchParams({ limit: "25" });
  for (const [k, v] of Object.entries(filters)) if (v) q.set(k, v);
  if (cursor) q.set("cursor", cursor);
  return getJSON<DeliveriesPage>(`/api/v1/deliveries?${q}`);
}
export const retryDelivery = (id: number) =>
  sendJSON<Delivery>("POST", `/api/v1/deliveries/${id}/retry`);

const rulePath = (id: string) => `/api/v1/rules/${encodeURIComponent(id)}`;

export async function listRules(): Promise<Rule[]> {
  return (await getJSON<{ items: Rule[] }>("/api/v1/rules")).items;
}
export const getRule = (id: string) => getJSON<Rule>(rulePath(id));
export const createRule = (body: CreateRuleBody) => sendJSON<Rule>("POST", "/api/v1/rules", body);
export const updateRule = (id: string, body: UpdateRuleBody) =>
  sendJSON<Rule>("PATCH", rulePath(id), body);
export const deleteRule = (id: string) => sendJSON<undefined>("DELETE", rulePath(id));
export async function previewRule(filter: string, watchId: string): Promise<PreviewItem[]> {
  const body = { filter, watch_id: watchId || undefined };
  return (await sendJSON<{ items: PreviewItem[] }>("POST", "/api/v1/rules/preview", body)).items;
}
export const getFilterReference = () => getJSON<FilterReference>("/api/v1/filters/reference");

const scriptPath = (id: string) => `/api/v1/scripts/${encodeURIComponent(id)}`;

export async function listScripts(): Promise<ScriptSummary[]> {
  return (await getJSON<{ items: ScriptSummary[] | null }>("/api/v1/scripts")).items ?? [];
}
export const getScript = (id: string) => getJSON<Script>(scriptPath(id));
export const createScript = (body: CreateScriptBody) =>
  sendJSON<Script>("POST", "/api/v1/scripts", body);
export const updateScript = (id: string, body: UpdateScriptBody) =>
  sendJSON<Script>("PATCH", scriptPath(id), body);
export const deleteScript = (id: string) => sendJSON<undefined>("DELETE", scriptPath(id));
/** Runs code in the sandbox without sending anything: a saved script by id, or unsaved source. */
export const dryRunScript = (target: { id: string } | { source: string }, on: DryRunEvent) =>
  "id" in target
    ? sendJSON<DryRun>("POST", `${scriptPath(target.id)}/dry-run`, on)
    : sendJSON<DryRun>("POST", "/api/v1/scripts/dry-run", { source: target.source, ...on });
