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

export interface UsageReport {
  /** False when the server has no Redis to read from. */
  enabled: boolean;
  /** Whether the server may reset a quota's usage. */
  can_reset: boolean;
  at: string;
  quotas: UsageQuota[];
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

export class Unauthorized extends Error {}

async function request<T>(method: string, path: string, body?: unknown, token = getToken()): Promise<T> {
  const res = await fetch("/api/v1" + path, {
    method,
    headers: { Authorization: "Bearer " + token, ...(body ? { "Content-Type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) throw new Unauthorized("Invalid token");
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error ?? `Request failed (${res.status})`);
  return data as T;
}

export const api = {
  checkToken: (token: string) => request<Overview>("GET", "/overview", undefined, token),
  overview: () => request<Overview>("GET", "/overview"),
  tasks: () => request<Task[]>("GET", "/tasks"),
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
