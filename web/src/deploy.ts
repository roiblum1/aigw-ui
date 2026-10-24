// What the Deployments page does with a deployment's values: the values file
// of the model-deployer chart, edited through a form and as JSON.

export type Values = Record<string, unknown>;

function isMap(v: unknown): v is Values {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** The value at a dotted path such as "serving.replicas", or undefined. */
// A path is keys joined by "."; a key with a dot in it (nvidia.com/gpu)
// writes the dot as \u0000.
function keys(path: string): string[] {
  return path.split(".").map((k) => k.replaceAll("\u0000", "."));
}

export function getIn(values: Values, path: string): unknown {
  let cur: unknown = values;
  for (const key of keys(path)) {
    if (!isMap(cur)) return undefined;
    cur = cur[key];
  }
  return cur;
}

/**
 * A copy of values with the path set. undefined, "" and an empty list remove
 * the key (the chart's default applies then), and a map left empty goes too.
 */
export function setIn(values: Values, path: string, value: unknown): Values {
  const [first, ...rest] = path.split(".");
  const key = first.replaceAll("\u0000", ".");
  const out: Values = { ...values };
  const remove = value === undefined || value === "" || (Array.isArray(value) && value.length === 0);
  if (rest.length === 0) {
    if (remove) delete out[key];
    else out[key] = value;
    return out;
  }
  const child = setIn(isMap(out[key]) ? (out[key] as Values) : {}, rest.join("."), value);
  if (Object.keys(child).length === 0) delete out[key];
  else out[key] = child;
  return out;
}

/** A form's number field: "" removes the key, anything else is a number. */
export function numberOrEmpty(text: string): number | undefined {
  if (text.trim() === "") return undefined;
  const n = Number(text);
  return Number.isFinite(n) ? n : undefined;
}

/** vLLM's arguments, one per line. */
export function argsToText(args: unknown): string {
  return Array.isArray(args) ? args.map(String).join("\n") : "";
}

export function textToArgs(text: string): string[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "");
}

// KServe's launcher runs the arguments through bash eval: one plain token
// each. The same rule as the hub's and the chart's.
const plainArg = /^[^\s'"$`\\;&|<>(){}*?[\]!#~]+$/;
// A value in single quotes is one word to eval and nothing in it expands.
const quotedArg = /^--[A-Za-z0-9._-]+='[^']*'$/;
const chartFlag = /^--kv[-_]transfer[-_]config/;

/** Why an argument is refused, or "" when it is fine. */
export function argProblem(arg: string): string {
  if (!plainArg.test(arg) && !quotedArg.test(arg))
    return `${arg}: not one plain token or --option='value' (no spaces, quotes or shell characters outside single quotes)`;
  if (chartFlag.test(arg)) return `${arg}: set by the chart when LMCache is on (see the fields above)`;
  return "";
}

/** Environment variables, NAME=value per line. */
export function envToText(env: unknown): string {
  if (!isMap(env)) return "";
  return Object.keys(env)
    .sort()
    .map((k) => `${k}=${String(env[k])}`)
    .join("\n");
}

/** The variables of the text, or the first line that is not NAME=value. */
export function textToEnv(text: string): { env: Record<string, string>; problem: string } {
  const env: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (line === "") continue;
    const eq = line.indexOf("=");
    const name = eq > 0 ? line.slice(0, eq).trim() : "";
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name)) return { env, problem: `${line}: not NAME=value` };
    env[name] = line.slice(eq + 1).trim();
  }
  return { env, problem: "" };
}

/** The name the hub and the chart give a model's objects. */
export function slug(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9-]/g, "-")
    .replace(/^-+|-+$/g, "");
}

/** GPUs one replica asks for (llmisvc.resources nvidia.com/gpu). */
export function gpusOf(values: Values): number {
  const v = getIn(values, "llmisvc.resources.limits") as Values | undefined;
  const r = getIn(values, "llmisvc.resources.requests") as Values | undefined;
  return Number(v?.["nvidia.com/gpu"] ?? r?.["nvidia.com/gpu"] ?? 0);
}

/** Sets the GPUs of one replica in both requests and limits; 0 removes them. */
export function setGpus(values: Values, gpus: number | undefined): Values {
  const n = gpus ? String(gpus) : undefined;
  return setIn(setIn(values, "llmisvc.resources.requests.nvidia\u0000com/gpu", n), "llmisvc.resources.limits.nvidia\u0000com/gpu", n);
}

/**
 * Data-parallel ranks: one port each (llmisvc.dataParallelExternalLB),
 * KServe's (llmisvc.parallelism.data), or vLLM's --data-parallel-size flag.
 * 0 without.
 */
export function ranksOf(values: Values): number {
  const lb = Number(getIn(values, "llmisvc.dataParallelExternalLB.ranks") ?? 0);
  if (lb > 1) return lb;
  const data = Number(getIn(values, "llmisvc.parallelism.data") ?? 0);
  if (data > 1) return data;
  for (const flags of [getIn(values, "llmisvc.extraArgs"), getIn(values, "llmisvc.args")]) {
    if (!Array.isArray(flags)) continue;
    for (const a of flags) {
      const m = /^--data[-_]parallel[-_]size=(\d+)$/.exec(String(a));
      if (m) return Number(m[1]);
    }
  }
  return 0;
}

function flagNumber(values: Values, flag: RegExp): number {
  for (const flags of [getIn(values, "llmisvc.extraArgs"), getIn(values, "llmisvc.args")]) {
    if (!Array.isArray(flags)) continue;
    for (const a of flags) {
      const m = flag.exec(String(a));
      if (m) return Number(m[1]);
    }
  }
  return 0;
}

function hasFlag(values: Values, flag: RegExp): boolean {
  return [getIn(values, "llmisvc.extraArgs"), getIn(values, "llmisvc.args")].some(
    (flags) => Array.isArray(flags) && flags.some((a) => flag.test(String(a))),
  );
}

/** What the deployment runs on, in a few words: replicas × (pods per replica × GPUs, parallelism). */
export function shape(values: Values): string {
  const gpus = gpusOf(values);
  const product = String(getIn(values, "nodes.gpuProduct") ?? "");
  const tp = Number(getIn(values, "llmisvc.parallelism.tensor") ?? 0) || flagNumber(values, /^--tensor[-_]parallel[-_]size=(\d+)$/);
  const ranks = ranksOf(values);
  const replicas = Number(getIn(values, "llmisvc.replicas") ?? 1);
  const multi = getIn(values, "llmisvc.multiNode") === true;
  const local = Number(getIn(values, "llmisvc.parallelism.dataLocal") ?? 0);
  const pods = multi && local > 0 && ranks > local ? Math.ceil(ranks / local) : 1;
  const ep = getIn(values, "llmisvc.parallelism.expert") === true || hasFlag(values, /^--enable[-_]expert[-_]parallel$/);
  const parts: string[] = [];
  if (ranks > 1) parts.push(`DP${ranks}`);
  if (tp > 1) parts.push(`TP${tp}`);
  if (ep) parts.push(`EP${Math.max(ranks, 1) * Math.max(tp, 1)}`);
  if (getIn(values, "llmisvc.roce") === true) parts.push("RoCE");
  if (getIn(values, "lmcache.enabled") === true) parts.push("LMCache");
  const per = gpus > 0 ? `${gpus} × ${product || "GPU"}` : "CPU";
  const where = pods > 1 ? `${pods} nodes × ${per}` : per;
  return `${replicas} × (${where}${parts.length ? ", " + parts.join(" ") : ""})`;
}

/**
 * A recipe's shape: shape() when it sets one, else what it leaves to each
 * cluster (a recipe without GPUs, product or CPU request).
 */
export function recipeShape(values: Values): string {
  const set = gpusOf(values) > 0 || getIn(values, "nodes.gpuProduct") || getIn(values, "llmisvc.resources.requests.cpu");
  if (set) return shape(values);
  return getIn(values, "lmcache.enabled") === true ? "shape per cluster, LMCache" : "shape per cluster";
}

/** Where the weights come from, in a few words. */
export function weights(values: Values): string {
  const sub = String(getIn(values, "model.subPath") ?? "");
  const s3 = String(getIn(values, "model.s3.path") ?? "");
  const uri = String(getIn(values, "model.uri") ?? "");
  if (sub) return s3 ? `s3://${s3} → node disks` : `node disks: ${sub}`;
  if (getIn(values, "modelCache.enabled") === true) return `${uri} → PVC`;
  return uri;
}

/** The keys of a layer, two levels deep (llmisvc.resources): what a cluster sets itself. */
export function overridden(layer: Values, prefix = "", depth = 2): string[] {
  const out: string[] = [];
  for (const k of Object.keys(layer).sort()) {
    const v = layer[k];
    if (isMap(v) && Object.keys(v).length > 0 && depth > 1) out.push(...overridden(v, prefix + k + ".", depth - 1));
    else out.push(prefix + k);
  }
  return out;
}

/** The values a new deployment starts from: a preset's, without its title. */
export function fromPreset(values: Values | undefined): Values {
  const out: Values = { ...(values ?? {}) };
  delete out.preset;
  delete out.deployment;
  return out;
}
