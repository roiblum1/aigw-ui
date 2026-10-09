export type Window = "1m" | "1h" | "1d";

export interface Cluster {
  id: string;
  name: string;
  site: string;
  namespace: string;
  gateway_name: string;
  auth_enabled: boolean;
  sync_status: "pending" | "synced" | "error";
  sync_message: string;
  synced_at: string | null;
  discovery_message: string;
  discovered_at: string | null;
  gateway_url: string;
  has_discovery_token: boolean;
  /** One of the sites that share traffic; it gets the site weights. */
  fleet_enabled: boolean;
  /** Gateway listener the API-key policy attaches to; empty is the whole Gateway. */
  client_listener: string;
}

export interface ClusterInput {
  name: string;
  site: string;
  namespace: string;
  gateway_name: string;
  auth_enabled: boolean;
  kubeconfig: string;
  gateway_url: string;
  discovery_token: string;
  fleet_enabled: boolean;
  client_listener: string;
}

export interface Endpoint {
  cluster_id: string;
  cluster_name?: string;
  host: string;
  port: number;
  upstream_model: string;
  /** "discovered" endpoints come from the cluster's own routes and are read-only. */
  source?: "manual" | "discovered";
  /** Empty when the cluster has nothing a quota can attach to for this model. */
  backends?: { name: string; namespace?: string; model: string; override: boolean }[];
  /** What the cluster can serve of the model. Absent in a request body. */
  capacity?: {
    /** The cluster has a deployment of the model. */
    serving: boolean;
    /** An operator is draining the site: its weight goes to 0 and stays there. */
    drained: boolean;
    revision: string;
    max_model_len: string;
    /** Ready instances times the capacity of one. Null while the cluster has not reported it. */
    observed: number | null;
    observed_at: string | null;
    /** The applied value, which follows observed slowly. */
    weight: number | null;
    changed_at: string | null;
    detail: string;
  };
}

export interface Model {
  id: string;
  name: string;
  slug: string;
  default_limit: number;
  default_window: Window;
  /** CEL over the token counts; empty charges total_tokens. */
  cost_expression: string;
  endpoints: Endpoint[];
  quota_capable: boolean;
  /** Each site's share of the model's traffic, as written to the gateways. */
  site_weights: { zone: string; weight: number }[];
  /** Why the weights are not being written, when they are not. */
  site_weights_note: string;
  /** Differences between the sites that serve the model. */
  warnings: string[];
}

export interface ModelInput {
  name: string;
  default_limit: number;
  default_window: Window;
  cost_expression: string;
  endpoints: Endpoint[];
}

export interface Tenant {
  id: string;
  slug: string;
  display_name: string;
  enabled: boolean;
  key_count: number;
  quota_count: number;
}

export interface ApiKey {
  id: string;
  name: string;
  client_id: string;
  key_prefix: string;
  revoked_at: string | null;
  created_at: string;
}

export interface Quota {
  id: string;
  model_id: string;
  model_name: string;
  token_limit: number;
  window: Window;
  /** Counted but never rejects a request. */
  shadow: boolean;
}

export interface TenantDetail {
  tenant: Tenant;
  keys: ApiKey[];
  quotas: Quota[];
}

export interface Overview {
  clusters: number;
  clusters_error: number;
  models: number;
  tenants: number;
  active_keys: number;
  quotas: number;
}

export interface TaskChange {
  kind: string;
  namespace: string;
  name: string;
  action?: "created" | "updated" | "deleted";
  /** Condition the gateway's controller set on the object, e.g. "Accepted". */
  gateway?: string;
  gateway_message?: string;
}

export interface TaskResult {
  cluster_name: string;
  /** A failed result is tried again by the next sync. */
  status: "pending" | "succeeded" | "failed";
  message: string;
  changes: TaskChange[];
  rejected: TaskChange[];
  finished_at: string | null;
}

export interface Task {
  id: number;
  action: string;
  summary: string;
  status: "pending" | "succeeded" | "failed";
  message: string;
  created_at: string;
  results: TaskResult[];
}

export type StepStatus = "pending" | "running" | "passed" | "failed" | "warning" | "skipped";

export interface SelfTestRun {
  cluster_id: string;
  cluster_name: string;
  model_id: string;
  model_name: string;
  status: "running" | "passed" | "failed";
  started_at: string;
  finished_at: string | null;
  steps: { id: string; title: string; status: StepStatus; detail: string }[];
}

export interface AuditEntry {
  id: number;
  at: string;
  /** How the caller was authenticated. */
  actor: string;
  /** The name the caller gave. Not verified. */
  on_behalf_of: string;
  method: string;
  path: string;
  status: number;
  action: string;
  summary: string;
  remote_addr: string;
  forwarded_for: string;
  user_agent: string;
}

export interface UsageQuota {
  tenant_id: string;
  tenant_slug: string;
  model_id: string;
  model_name: string;
  limit: number;
  window: Window;
  shadow: boolean;
  /** Highest counter; each counter is held to the limit on its own. */
  used: number;
  resets_at: string;
  counters: { backend: string; clusters: string[]; used: number }[];
}

/** A model's default bucket: every request to the model is charged to it. */
export interface UsagePool {
  model_id: string;
  model_name: string;
  limit: number;
  window: Window;
  used: number;
  resets_at: string;
}

export interface UsageReport {
  /** False when the server has no Redis to read from. */
  enabled: boolean;
  /** Whether the server may reset a quota's usage. */
  can_reset: boolean;
  at: string;
  quotas: UsageQuota[];
  pools: UsagePool[];
  hint?: string;
}

export interface ProbeResult {
  reachable: boolean;
  error?: string;
  probe?: { kubernetes_version: string; gateway_found: boolean; kinds: Record<string, boolean> };
}

const TOKEN_KEY = "aigw-ui-token";

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setToken(token: string) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    // Storage can be unavailable; the token then lasts for this page load only.
  }
}

const NAME_KEY = "aigw-ui-name";

/** The name this browser's user gave at sign-in, recorded in the audit log. */
export function getName(): string {
  try {
    return localStorage.getItem(NAME_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setName(name: string) {
  try {
    if (name) localStorage.setItem(NAME_KEY, name);
    else localStorage.removeItem(NAME_KEY);
  } catch {
    // Without storage the audit log shows the admin token only.
  }
}

export class Unauthorized extends Error {}
export class NotFound extends Error {}

async function request<T>(method: string, path: string, body?: unknown, token = getToken()): Promise<T> {
  const res = await fetch("/api/v1" + path, {
    method,
    headers: {
      Authorization: "Bearer " + token,
      ...(body ? { "Content-Type": "application/json" } : {}),
      // Percent-encoded, because a header cannot carry every character a name can.
      ...(getName() ? { "X-On-Behalf-Of": encodeURIComponent(getName()) } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) throw new Unauthorized("Invalid token");
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => null);
  if (res.status === 404) throw new NotFound(data?.error ?? "Not found");
  if (!res.ok) throw new Error(data?.error ?? `Request failed (${res.status})`);
  return data as T;
}

export const api = {
  checkToken: (token: string) => request<Overview>("GET", "/overview", undefined, token),
  overview: () => request<Overview>("GET", "/overview"),
  /** The platform design page, as a complete HTML document. */
  platformArchitecture: async () => {
    const res = await fetch("/api/v1/docs/platform-architecture", { headers: { Authorization: "Bearer " + getToken() } });
    if (res.status === 401) throw new Unauthorized("Invalid token");
    if (!res.ok) throw new Error(`Request failed (${res.status})`);
    return res.text();
  },
  tasks: () => request<Task[]>("GET", "/tasks"),
  audit: (before?: number) => request<AuditEntry[]>("GET", "/audit" + (before ? `?before=${before}` : "")),
  startSelfTest: (clusterId: string, model_id: string) =>
    request<SelfTestRun>("POST", `/clusters/${clusterId}/selftest`, { model_id }),
  /** The last self-test of a cluster, or null when none ran since the server started. */
  selfTest: (clusterId: string) =>
    request<SelfTestRun>("GET", `/clusters/${clusterId}/selftest`).catch((err) => {
      if (err instanceof NotFound) return null;
      throw err;
    }),
  resetUsage: (tenantId: string, modelId: string) =>
    request<{ counters: number; deleted: number }>("POST", `/tenants/${tenantId}/quotas/${modelId}/reset`),
  usage: (tenantId?: string) => request<UsageReport>("GET", "/usage" + (tenantId ? `?tenant_id=${tenantId}` : "")),

  clusters: () => request<Cluster[]>("GET", "/clusters"),
  createCluster: (c: ClusterInput) => request<Cluster>("POST", "/clusters", c),
  updateCluster: (id: string, c: ClusterInput) => request<Cluster>("PUT", `/clusters/${id}`, c),
  deleteCluster: (id: string) => request<void>("DELETE", `/clusters/${id}`),
  probeCluster: (id: string) => request<ProbeResult>("POST", `/clusters/${id}/probe`),
  syncCluster: (id: string) => request<{ ok: boolean; error?: string }>("POST", `/clusters/${id}/sync`),
  syncAll: () => request<Cluster[]>("POST", "/sync"),
  discoverAll: () => request<Cluster[]>("POST", "/discover"),
  manifests: (id: string) => request<{ yaml: string }>("GET", `/clusters/${id}/manifests`),

  models: () => request<Model[]>("GET", "/models"),
  createModel: (m: ModelInput) => request<{ id: string }>("POST", "/models", m),
  updateModel: (id: string, m: ModelInput) => request<{ id: string }>("PUT", `/models/${id}`, m),
  deleteModel: (id: string) => request<void>("DELETE", `/models/${id}`),
  drainSite: (modelId: string, clusterId: string, drained: boolean) =>
    request<void>("PUT", `/models/${modelId}/sites/${clusterId}/drain`, { drained }),

  tenants: () => request<Tenant[]>("GET", "/tenants"),
  tenant: (id: string) => request<TenantDetail>("GET", `/tenants/${id}`),
  createTenant: (slug: string, display_name: string) => request<Tenant>("POST", "/tenants", { slug, display_name }),
  updateTenant: (id: string, display_name: string, enabled: boolean) =>
    request<Tenant>("PUT", `/tenants/${id}`, { display_name, enabled }),
  deleteTenant: (id: string) => request<void>("DELETE", `/tenants/${id}`),
  createKey: (tenantId: string, name: string) =>
    request<{ key: ApiKey; secret: string }>("POST", `/tenants/${tenantId}/keys`, { name }),
  revokeKey: (id: string) => request<void>("DELETE", `/keys/${id}`),
  setQuota: (tenantId: string, model_id: string, token_limit: number, window: Window, shadow: boolean) =>
    request<Quota[]>("PUT", `/tenants/${tenantId}/quotas`, { model_id, token_limit, window, shadow }),
  deleteQuota: (id: string) => request<void>("DELETE", `/quotas/${id}`),
};
