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
}

export interface ClusterInput {
  name: string;
  site: string;
  namespace: string;
  gateway_name: string;
  auth_enabled: boolean;
  kubeconfig: string;
}

export interface Endpoint {
  cluster_id: string;
  cluster_name?: string;
  host: string;
  port: number;
  upstream_model: string;
  /** "discovered" endpoints come from the cluster's own routes and are read-only. */
  source?: "manual" | "discovered";
  backends?: { name: string; model: string }[];
}

export interface Model {
  id: string;
  name: string;
  slug: string;
  default_limit: number;
  default_window: Window;
  endpoints: Endpoint[];
}

export interface ModelInput {
  name: string;
  default_limit: number;
  default_window: Window;
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
  setQuota: (tenantId: string, model_id: string, token_limit: number, window: Window) =>
    request<Quota[]>("PUT", `/tenants/${tenantId}/quotas`, { model_id, token_limit, window }),
  deleteQuota: (id: string) => request<void>("DELETE", `/quotas/${id}`),
};
