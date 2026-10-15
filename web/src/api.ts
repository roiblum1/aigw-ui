import type { Unit } from "./money";

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
  /** Where the other sites reach this site's gateway. */
  peer_host: string;
  peer_port: number;
  /** Identifies the entry routes last applied; outdated when the fleet has moved on. */
  fleet_revision: string;
  fleet_outdated: boolean;
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
  peer_host: string;
  peer_port: number;
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
  /** The backend of the model's entry route on this cluster. Set for a fleet model on a fleet cluster. */
  entry_backend?: string;
  /** What the cluster can serve of the model. Absent in a request body. */
  capacity?: {
    /** The cluster has a deployment of the model. */
    serving: boolean;
    /** An operator is draining the site: its weight goes to 0 and stays there. */
    drained: boolean;
    revision: string;
    max_model_len: string;
    pools: { namespace: string; name: string; group: string }[];
    /** "measured" when the capacity comes from the measured shapes, "declared" when from aigw-ui.io/capacity-per-instance. */
    basis: "declared" | "measured";
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
  /** The hub renders the model's entry route on every fleet cluster. */
  fleet: boolean;
  /** What happens to a tenant whose budget for the model is spent. */
  spent_mode: SpentMode;
  /** Tenants without a quota on the model are served as best-effort too. */
  best_effort_unlimited: boolean;
  /** The most a tenant may use as best-effort in one period, or null. */
  best_effort_limit: number | null;
  /** Differences between the sites that serve the model. */
  warnings: string[];
  /** What the model's limits and usage are counted in. */
  unit: Unit;
  /** The prices in use, and the ones that wait for their day. */
  prices: Price | null;
  pending_prices: Price | null;
  /** Every tenant is counted and nobody is refused. */
  price_dry_run: boolean;
  /** The market entry chosen by hand: null follows the annotation and the served names, "" is none. */
  market_id_set: string | null;
  /** The values of aigw-ui.io/market-id on the model's deployments. */
  market_annotations: string[];
  /** The model's market price, or null when it has none. */
  market: MarketPrice | null;
  /** Why the model has no market price, when it is not for want of a match. */
  market_note: string;
}

/** A model's entry in the market snapshot, in dollars for a million tokens. */
export interface MarketPrice {
  id: string;
  name: string;
  hf_id: string;
  /** How the entry was found: chosen by hand, by annotation, or by a served name. */
  source: "set" | "annotation" | "name";
  snapshot_date: string;
  prompt: number;
  completion: number;
  cache_read: number | null;
  /** What Use market prices sets, in credits; null when the market's price cannot be used. */
  prices: { price_input: number; price_cached: number; price_output: number } | null;
  /** The model's prices next to the market's. */
  state: "no-prices" | "in-use" | "moved" | "custom";
}

export interface MarketEntry {
  id: string;
  hf_id: string;
  name: string;
  context: number;
  prompt: number;
  completion: number;
  cache_read: number | null;
}

export interface MarketSearch {
  source: string;
  snapshot_date: string;
  models: MarketEntry[];
}

/** A credit is 0.00001 dollars. A model without prices is counted in tokens. */
export type { Unit } from "./money";

/** Prices of a model, in credits for a million tokens. */
export interface Price {
  version: number;
  price_input: number;
  price_cached: number;
  price_output: number;
  effective_at: string;
  /** Null until the server has started using the prices. */
  applied_at: string | null;
  note: string;
  created_at: string;
  /** "<entry>@<snapshot date>" for prices taken unchanged from the market. */
  market: string;
}

/** Prices as they are sent, in dollars for a million tokens. */
export interface PriceInput {
  input_usd: number;
  cached_usd: number;
  output_usd: number;
  now: boolean;
  note: string;
}

export type SpentMode = "refuse" | "best-effort";

/** A period in which a tenant was served as best-effort on a model. */
export interface OverageEntry {
  model_id: string;
  model_name: string;
  tenant_id: string;
  tenant_slug: string;
  since: string;
  until: string;
  active: boolean;
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
  /** In tokens, or in credits for a model with prices. See unit. */
  token_limit: number;
  window: Window;
  /** Counted but never rejects a request. */
  shadow: boolean;
  unit: Unit;
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
  /** Tokens used as best-effort, after the budget was spent. */
  overage_used: number;
  resets_at: string;
  counters: { backend: string; clusters: string[]; used: number; overage?: boolean }[];
  /** Set while the tenant's requests for the model are served as best-effort. */
  best_effort_until?: string;
  /** The most the model lets a tenant use as best-effort in one period. */
  best_effort_limit?: number;
  /** The tenant reached that limit and is refused until the period ends. */
  best_effort_capped?: boolean;
  /** What limit, used and overage_used are counted in. */
  unit: Unit;
  /** The model's prices are being tried out: counted, nobody refused. */
  dry_run?: boolean;
}

/** A model's default bucket: every request to the model is charged to it. */
export interface UsagePool {
  model_id: string;
  model_name: string;
  limit: number;
  window: Window;
  used: number;
  resets_at: string;
  unit: Unit;
  /** The model's prices are being tried out. The limit is then not the pool. */
  dry_run?: boolean;
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

/** What one tenant used of one model in one hour or day. */
export interface UsagePoint {
  at: string;
  tenant_id: string;
  tenant_slug: string;
  model_id: string;
  model_name: string;
  unit: Unit;
  /** Within the tenant's budget. */
  used: number;
  /** As best-effort, after the budget was spent. */
  best_effort: number;
}

export type Step = "hour" | "day";

export interface UsageHistory {
  /** The start of the first step. Steps without usage are left out of points. */
  from: string;
  step: Step;
  points: UsagePoint[];
}

/** A tenant's budget on one model, as the tenant sees it. */
export interface MyBudget {
  model_name: string;
  unit: Unit;
  limit: number;
  window: Window;
  used: number;
  resets_at: string;
  /** False while the budget is only counted and nobody is refused. */
  enforced: boolean;
  best_effort_used: number;
  best_effort_until?: string;
  best_effort_limit?: number;
  best_effort_capped?: boolean;
}

/** What a tenant sees of itself after signing in with one of its keys. */
/** What an operator chooses for one capacity measurement. */
export interface CapacitySettings {
  /** Requests in flight at each load step, ascending. */
  concurrency: number[];
  /** Requests a step sends at least; four per request in flight above it. */
  min_requests: number;
  /** The longest a step may take, in seconds. */
  step_seconds: number;
  sessions: number;
  max_prompt_tokens: number;
  max_output_tokens: number;
  ttft_p95_ms: number;
  itl_p95_ms: number;
}

/** What one instance of a deployment is: the same model runs in several. */
export interface CapacityShape {
  topology: "single" | "multi-node" | "prefill-decode";
  gpu: string;
  gpus_per_instance: number;
  revision: string;
  key: string;
  label: string;
}

/** What one load step measured. */
export interface CapacityStep {
  concurrency: number;
  requests: number;
  errors: number;
  duration_s: number;
  input_tokens: number;
  output_tokens: number;
  isl_mean: number;
  osl_mean: number;
  ttft_p50_ms: number;
  ttft_p95_ms: number;
  itl_p50_ms: number;
  itl_p95_ms: number;
  server_prompt_tokens: number | null;
  server_cached_tokens: number | null;
  error?: string;
}

/** A step with what the server made of it. */
export interface CapacityStepView {
  concurrency: number;
  met: boolean;
  reason?: string;
  /** The step reached its longest time before enough requests were answered. */
  few_samples: boolean;
  input_per_s: number;
  output_per_s: number;
  cached_share: number | null;
}

export interface CapacityRun {
  id: string;
  model_id: string;
  model_name: string;
  cluster_id: string;
  cluster_name: string;
  namespace: string;
  deployment: string;
  shape: CapacityShape;
  job_name: string;
  /** The pod that was measured. */
  instance: string;
  settings: CapacitySettings;
  status: "running" | "succeeded" | "failed" | "cancelled";
  detail: string;
  dataset: { sessions: number; requests: number; prompt_mean: number; prompt_max: number } | null;
  steps: CapacityStep[];
  /** The highest step that met the targets, why the next one did not, and every step. */
  result: { best: CapacityStep | null; limit: string; steps: CapacityStepView[] };
  created_at: string;
  finished_at: string | null;
}

export interface CapacityPrices {
  price_input: number;
  price_cached: number;
  price_output: number;
}

/** What one shape of a model gives. Amounts are in the model's unit, costs in credits. */
export interface CapacityShapePlan {
  shape: CapacityShape;
  instances: number;
  sites: string[];
  measured: boolean;
  concurrency: number;
  per_instance_day: number;
  cost_per_instance_hour: number | null;
  /** A million input-equivalent tokens on this shape busy all day. */
  cost_per_million: number | null;
}

/** What a model can be given. Amounts are in unit, costs in credits. */
/** Where one model's budgets stand against what its GPUs serve, in its unit, per day. */
export interface ModelBudget {
  model_id: string;
  model_name: string;
  unit: Unit;
  /** False until an instance is measured: the amounts are then 0, not a limit. */
  measured: boolean;
  capacity_day: number;
  ceiling_day: number;
  given_day: number;
  left_day: number;
  overcommit: number | null;
  overcommit_limit: number;
  short_windows: number;
  pool_day: number | null;
}

export interface Budgets {
  models: ModelBudget[];
  /** Added up over the measured models counted in dollars. */
  credits: { capacity_day: number; ceiling_day: number; given_day: number; left_day: number };
}

export interface CapacityPlan {
  unit: Unit;
  utilization: number;
  shapes: CapacityShapePlan[];
  instances: number;
  measured_instances: number;
  full_day: number;
  capacity_day: number;
  /** What the budgets let tenants use in a day; hourly and per-minute ones at their daily maximum. */
  given_day: number;
  /** How many budgets are per hour or minute. */
  short_windows: number;
  overcommit: number | null;
  /** How far the budgets may add up beyond capacity_day, the ceiling that follows, and what is left under it. */
  overcommit_limit: number;
  ceiling_day: number;
  /** Negative when the budgets are past the ceiling. */
  left_day: number;
  /** The most the shared pool lets be used in a day, or null when it is the strict 1. */
  pool_day: number | null;
  busiest_day: number;
  busiest_share: number | null;
  observed_utilization: number | null;
  safe_allocated_day: number | null;
  cost_per_hour: number | null;
  break_even: CapacityPrices | null;
  charged_per_hour: number | null;
  notes: string[];
}

/** A deployment (LLMInferenceService) of a model on a site. */
export interface CapacityDeployment {
  cluster_id: string;
  cluster_name: string;
  namespace: string;
  name: string;
  model_id: string;
  topology: string;
  gpu: string;
  gpus_per_instance: number;
  revision: string;
  instances: number | null;
  drained: boolean;
  observed_at: string;
  shape: CapacityShape;
  max_model_len: number;
}

export interface CapacityModel {
  model_id: string;
  model_name: string;
  unit: Unit;
  target_utilization: number;
  deployments: CapacityDeployment[];
  defaults: CapacitySettings;
  /** The newest measurement of each shape that found a load within the targets. */
  latest: CapacityRun[];
  plan: CapacityPlan;
  running: CapacityRun | null;
}

export interface CapacityGPU {
  gpu: string;
  usd_per_hour: number | null;
  note: string;
  deployments: number;
}

export interface CapacityOverview {
  /** The AIPerf image the server runs, or "" when none is set. */
  image: string;
  gpus: CapacityGPU[];
  models: CapacityModel[];
}

export interface MyUsage {
  tenant: string;
  display_name: string;
  at: string;
  usage_enabled: boolean;
  budgets: MyBudget[];
  /** Credits for a million tokens. */
  prices: { model_name: string; price_input: number; price_cached: number; price_output: number }[];
  days_from: string;
  days: UsagePoint[];
  hours_from: string;
  hours: UsagePoint[];
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

export interface DocEntry {
  /** The file's path without ".md", such as "user-guide" or "release-notes/v0.8.0". */
  name: string;
  title: string;
  group: string;
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
  /** The guides and release notes built into the server. */
  docs: () => request<DocEntry[]>("GET", "/docs"),
  doc: (name: string) => request<{ name: string; title: string; markdown: string }>("GET", "/docs/text/" + name),
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
  usageHistory: (step: Step, days: number) => request<UsageHistory>("GET", `/usage/history?step=${step}&days=${days}`),
  /** A tenant's own usage, by one of its API keys. */
  myUsage: (key: string) => request<MyUsage>("GET", "/my/usage", undefined, key),

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
  setModelFleet: (id: string, enabled: boolean) => request<void>("PUT", `/models/${id}/fleet`, { enabled }),
  setModelSpent: (id: string, mode: SpentMode, unlimited: boolean, limit: number | null) =>
    request<void>("PUT", `/models/${id}/spent`, { mode, best_effort_unlimited: unlimited, best_effort_limit: limit }),
  prices: (id: string) => request<Price[]>("GET", `/models/${id}/prices`),
  setPrices: (id: string, p: PriceInput) => request<Price[]>("PUT", `/models/${id}/prices`, p),
  deletePendingPrices: (id: string) => request<void>("DELETE", `/models/${id}/prices/pending`),
  market: (q: string) => request<MarketSearch>("GET", `/market?q=${encodeURIComponent(q)}`),
  setMarketID: (id: string, marketID: string | null) => request<void>("PUT", `/models/${id}/market-id`, { market_id: marketID }),
  setPriceDryRun: (id: string, enabled: boolean) => request<void>("PUT", `/models/${id}/prices/dry-run`, { enabled }),
  overage: () => request<OverageEntry[]>("GET", "/overage"),
  capacity: () => request<CapacityOverview>("GET", "/capacity"),
  capacityRuns: (modelId: string) => request<CapacityRun[]>("GET", `/models/${modelId}/capacity-runs`),
  startCapacityRun: (
    modelId: string,
    body: { cluster_id: string; namespace: string; deployment: string; pod: string; settings: CapacitySettings },
  ) => request<CapacityRun>("POST", `/models/${modelId}/capacity-runs`, body),
  cancelCapacityRun: (id: string) => request<CapacityRun>("POST", `/capacity-runs/${id}/cancel`),
  deleteCapacityRun: (id: string) => request<void>("DELETE", `/capacity-runs/${id}`),
  deploymentPods: (clusterId: string, namespace: string, name: string) =>
    request<string[]>("GET", `/clusters/${clusterId}/deployments/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/pods`),
  setTargetUtilization: (modelId: string, utilization: number) =>
    request<void>("PUT", `/models/${modelId}/target-utilization`, { utilization }),
  setOvercommitLimit: (modelId: string, limit: number) =>
    request<void>("PUT", `/models/${modelId}/overcommit-limit`, { limit }),
  budgets: () => request<Budgets>("GET", "/budgets"),
  setGPUPrice: (gpu: string, usd_per_hour: number, note: string) =>
    request<void>("PUT", `/gpu-prices/${encodeURIComponent(gpu)}`, { usd_per_hour, note }),
  deleteGPUPrice: (gpu: string) => request<void>("DELETE", `/gpu-prices/${encodeURIComponent(gpu)}`),
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
