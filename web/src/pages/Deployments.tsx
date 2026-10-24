import { useEffect, useState, type ReactNode } from "react";
import { Boxes, Plus, Rocket } from "lucide-react";
import { api, type GitDeployment, type GitModel, type GitOps } from "../api";
import { Empty, ErrorBanner, Field, FormModal, PageHeader, formatTime, useAction, useLoad, usePolling } from "../components";
import {
  argProblem,
  argsToText,
  envToText,
  getIn,
  gpusOf,
  numberOrEmpty,
  overridden,
  setGpus,
  setIn,
  shape,
  textToArgs,
  textToEnv,
  weights,
  type Values,
} from "../deploy";

const REFRESH_MS = 5000;

type Dialog =
  | { kind: "deploy" }
  | { kind: "edit"; d: GitDeployment }
  | { kind: "recipe"; m: GitModel }
  | { kind: "model" };

export default function Deployments() {
  const { data, error, reload } = useLoad(api.gitops, "gitops");
  usePolling(reload, REFRESH_MS);
  const action = useAction();
  const [dialog, setDialog] = useState<Dialog | null>(null);
  const [done, setDone] = useState("");
  const usable = !!data?.enabled && !data.error;

  const remove = (d: GitDeployment) => {
    const typed = prompt(
      `Type ${d.model} to remove it from ${d.cluster}. The hub removes its directory under ${d.cluster} from git; Argo CD then deletes ${d.model_name} there. Its recipe stays.`,
    );
    if (typed === null) return;
    action.run(async () => {
      if (typed !== d.model) throw new Error(`Not removed: you typed "${typed}", not ${d.model}.`);
      const { commit } = await api.deleteDeployment(d.cluster, d.model, d.revision);
      setDone(`Removed ${d.model} from ${d.cluster} in git (commit ${commit.slice(0, 7)}). Argo CD deletes it at its next refresh.`);
      await reload();
    });
  };
  const saved = async (message: string) => {
    setDone(message);
    await reload();
  };

  return (
    <>
      <PageHeader
        title="Deployments"
        subtitle="Models on the clusters, deployed by Argo CD from the platform's repository with the model-deployer chart: each model's recipe, and each cluster's own values on top."
      >
        <button disabled={!usable || !data?.models.length} onClick={() => setDialog({ kind: "model" })}>
          <Plus />
          New model
        </button>
        <button className="primary" disabled={!usable || !data?.models.length} onClick={() => setDialog({ kind: "deploy" })}>
          <Rocket />
          Deploy a model
        </button>
      </PageHeader>
      <ErrorBanner message={error || action.error || data?.error || ""} />
      {data?.argocd_error && <div className="banner warn">Argo CD's status could not be read: {data.argocd_error}</div>}
      {done && <div className="banner">{done}</div>}

      {data && !data.enabled && (
        <Empty icon={Rocket}>
          Deployments are off: the hub has no git repository. Set <span className="mono">gitops.repo</span> and a Secret that may
          push in the hub's chart (Docs: Deployments).
        </Empty>
      )}
      {data?.enabled && (
        <p className="hint">
          <span className="mono">{data.repo}</span> · branch <span className="mono">{data.branch}</span> · directory{" "}
          <span className="mono">{data.dir}</span>
          {data.commit && (
            <>
              {" "}
              · at <span className="mono">{data.commit.slice(0, 7)}</span>
            </>
          )}
          . A change is a commit; Argo CD applies it at its next refresh (about 3 minutes, at once with a webhook).
        </p>
      )}

      {usable && data && (
        <>
          <h2 className="section-title">On the clusters</h2>
          {data.deployments.length === 0 ? (
            <Empty icon={Rocket}>No model is deployed on a cluster yet.</Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Model</th>
                    <th>Runs on</th>
                    <th>This cluster's own values</th>
                    <th>Argo CD</th>
                    <th>On the cluster</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {data.deployments.map((d) => (
                    <tr key={d.path}>
                      <td>
                        <div className="strong">{d.model_name}</div>
                        <div className="detail mono">
                          {d.cluster} / {d.namespace} / {d.object}
                        </div>
                        {d.problem && <div className="warn-text">{d.problem}</div>}
                      </td>
                      <td>
                        {shape(d.values)}
                        <div className="detail mono clamp" title={weights(d.values)}>
                          {weights(d.values)}
                        </div>
                      </td>
                      <td>
                        <OwnValues layer={d.override} />
                      </td>
                      <td>
                        <ArgoStatus gitops={data} d={d} />
                      </td>
                      <td>
                        <LiveStatus d={d} />
                      </td>
                      <td className="row-actions">
                        <button onClick={() => setDialog({ kind: "edit", d })}>Edit</button>
                        <button className="danger" disabled={action.busy} onClick={() => remove(d)}>
                          Remove
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          <h2 className="section-title">Models</h2>
          {data.models.length === 0 ? (
            <Empty icon={Boxes}>No model under operators/ yet: an operator is a model when its values have model.name.</Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Model</th>
                    <th>Recipe</th>
                    <th>Chart</th>
                    <th>Clusters</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {data.models.map((m) => (
                    <tr key={m.name}>
                      <td>
                        <div className="strong">{m.model_name}</div>
                        <div className="detail mono">
                          operators/{m.name} · {m.namespace}
                        </div>
                      </td>
                      <td>
                        {shape(m.values)}
                        <div className="detail mono clamp" title={weights(m.values)}>
                          {weights(m.values)}
                        </div>
                      </td>
                      <td className="mono detail">{m.chart}</td>
                      <td>
                        {data.deployments
                          .filter((d) => d.model === m.name)
                          .map((d) => d.cluster)
                          .join(", ") || "—"}
                      </td>
                      <td className="row-actions">
                        <button onClick={() => setDialog({ kind: "recipe", m })}>Edit recipe</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}

      {dialog?.kind === "deploy" && data && <DeployDialog gitops={data} onClose={() => setDialog(null)} onSaved={saved} />}
      {dialog?.kind === "edit" && data && (
        <FormDialog
          title={`${dialog.d.model_name} on ${dialog.d.cluster}`}
          submitLabel="Commit the change"
          start={dialog.d.values}
          note={`Saved into ${dialog.d.path} as what differs from the layers below (${dialog.d.layers.slice(0, -1).join(", ") || "none"}). The recipe stays as it is.`}
          onClose={() => setDialog(null)}
          onSubmit={async (values) => {
            const { commit } = await api.updateDeployment(dialog.d.cluster, dialog.d.model, values, dialog.d.revision);
            await saved(`Changed ${dialog.d.model} on ${dialog.d.cluster}: commit ${commit.slice(0, 7)}. Argo CD applies it at its next refresh.`);
          }}
        />
      )}
      {dialog?.kind === "recipe" && data && (
        <FormDialog
          title={`Recipe of ${dialog.m.model_name}`}
          submitLabel="Commit the recipe"
          start={dialog.m.values}
          note={`operators/${dialog.m.name}/values.yaml: the first layer on every cluster that runs it (${
            data.deployments
              .filter((d) => d.model === dialog.m.name)
              .map((d) => d.cluster)
              .join(", ") || "none yet"
          }). A cluster's own values still win.`}
          onClose={() => setDialog(null)}
          onSubmit={async (values) => {
            const { commit } = await api.updateGitModel(dialog.m.name, values, dialog.m.revision);
            await saved(`Changed the recipe of ${dialog.m.name}: commit ${commit.slice(0, 7)}. Every cluster that runs it follows at Argo CD's next refresh.`);
          }}
        />
      )}
      {dialog?.kind === "model" && data && <NewModelDialog gitops={data} onClose={() => setDialog(null)} onSaved={saved} />}
    </>
  );
}

function OwnValues({ layer }: { layer: Record<string, unknown> }) {
  const keys = overridden(layer);
  if (keys.length === 0) return <span className="muted">none: the recipe as it is</span>;
  return (
    <div className="detail mono clamp" title={keys.join("\n")}>
      {keys.slice(0, 4).join(", ")}
      {keys.length > 4 && ` and ${keys.length - 4} more`}
    </div>
  );
}

function badgeOf(state: string): string {
  if (state === "Synced" || state === "Healthy" || state === "Succeeded") return "synced";
  if (state === "OutOfSync" || state === "Progressing" || state === "Running") return "pending";
  if (state === "" || state === "Unknown" || state === "Suspended") return "idle";
  return "error";
}

function ArgoStatus({ gitops, d }: { gitops: GitOps; d: GitDeployment }) {
  if (!gitops.argocd)
    return (
      <span className="muted" title="Set gitops.argocdNamespace in the hub's chart to see Argo CD's status here.">
        not read
      </span>
    );
  if (!d.app)
    return (
      <span className="tag warn" title={`No Application named ${d.app_name} yet: the platform makes it within a minute of the commit.`}>
        no Application yet
      </span>
    );
  const behind = gitops.commit && d.app.revision && d.app.revision !== gitops.commit;
  return (
    <>
      <span className={`badge ${badgeOf(d.app.sync)}`}>{d.app.sync || "Unknown"}</span>{" "}
      <span className={`badge ${badgeOf(d.app.health)}`}>{d.app.health || "Unknown"}</span>
      {behind && (
        <div className="detail">
          at {d.app.revision.slice(0, 7)}, git is at {gitops.commit?.slice(0, 7)}
        </div>
      )}
      {d.app.message && (
        <div className="detail clamp" title={d.app.message}>
          {d.app.operation && `${d.app.operation}: `}
          {d.app.message}
        </div>
      )}
    </>
  );
}

function LiveStatus({ d }: { d: GitDeployment }) {
  if (!d.cluster_id) return <span className="muted" title="The hub has no cluster of this name.">not a cluster of the hub</span>;
  if (!d.live)
    return (
      <span className="muted" title="Discovery reads the cluster every minute.">
        not seen yet
      </span>
    );
  const ready = d.live.instances;
  return (
    <>
      {ready === null ? (
        <span className="badge pending">starting</span>
      ) : (
        <span className={`badge ${ready >= d.live.desired ? "synced" : "pending"}`}>
          {ready} of {d.live.desired} ready
        </span>
      )}
      <div className="detail">seen {formatTime(d.live.observed_at)}</div>
    </>
  );
}

function DeployDialog(props: { gitops: GitOps; onClose: () => void; onSaved: (m: string) => Promise<void> }) {
  const { gitops } = props;
  const [model, setModel] = useState(gitops.models[0]?.name ?? "");
  const free = gitops.clusters.filter((c) => !gitops.deployments.some((d) => d.cluster === c.name && d.model === model));
  const [cluster, setCluster] = useState("");
  const chosen = free.some((c) => c.name === cluster) ? cluster : (free[0]?.name ?? "");
  const [start, setStart] = useState<{ key: string; values: Values } | null>(null);
  const [error, setError] = useState("");
  const key = `${chosen}/${model}`;

  useEffect(() => {
    if (!chosen || !model) return;
    let live = true;
    api
      .gitopsBase(chosen, model)
      .then((b) => live && (setStart({ key, values: b.values }), setError("")))
      .catch((e: unknown) => live && setError(e instanceof Error ? e.message : String(e)));
    return () => {
      live = false;
    };
  }, [chosen, model, key]);

  return (
    <FormDialog
      key={start?.key ?? "loading"}
      title="Deploy a model"
      submitLabel="Commit and deploy"
      start={start?.key === key ? start.values : null}
      note={`Registers ${model || "the model"} on ${chosen || "the cluster"}: the directory ${
        gitops.clusters.find((c) => c.name === chosen)?.path ?? "sites/…"
      }/${model}/ with what you change below as the cluster's own values. The form starts from the recipe and the site's layers.`}
      onClose={props.onClose}
      onSubmit={async (values) => {
        const { commit } = await api.createDeployment(chosen, model, values);
        await props.onSaved(`Deployed ${model} on ${chosen}: commit ${commit.slice(0, 7)}. The platform makes its Application, then Argo CD applies it.`);
      }}
      head={
        <>
          <ErrorBanner message={error} />
          <div className="grid-2">
            <Field label="Model">
              <select value={model} onChange={(e) => setModel(e.target.value)}>
                {gitops.models.map((m) => (
                  <option key={m.name} value={m.name}>
                    {m.model_name} ({m.name})
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Cluster" hint="A cluster directory under sites/; its name in Argo CD and in the hub.">
              <select value={chosen} onChange={(e) => setCluster(e.target.value)}>
                {free.map((c) => (
                  <option key={c.path} value={c.name}>
                    {c.name} ({c.site}/{c.env}){c.cluster_id ? "" : " — not in the hub"}
                  </option>
                ))}
              </select>
            </Field>
          </div>
        </>
      }
    />
  );
}

function NewModelDialog(props: { gitops: GitOps; onClose: () => void; onSaved: (m: string) => Promise<void> }) {
  const { gitops } = props;
  const [from, setFrom] = useState(gitops.models[0]?.name ?? "");
  const [name, setName] = useState("");
  const [namespace, setNamespace] = useState("");
  const [cluster, setCluster] = useState("");
  const source = gitops.models.find((m) => m.name === from);
  const ns = namespace || (name ? `llm-${name}` : "");
  return (
    <FormDialog
      key={from}
      title="New model"
      submitLabel={cluster ? "Commit and deploy" : "Commit the model"}
      start={source ? setIn(source.values, "namespace", undefined) : null}
      note={`Adds operators/${name || "<name>"}/: an app config copied from ${from}'s (chart, sync options) and the recipe below. Object names that start with ${from} (SCC, StorageClass, PVCs, LMCache engine) start with ${name || "the new name"} instead, so the two models never share them.`}
      onClose={props.onClose}
      onSubmit={async (values) => {
        const { commit } = await api.createGitModel({ name, namespace: ns, from, values: setIn(values, "namespace", ns), cluster: cluster || undefined });
        await props.onSaved(`Added ${name}${cluster ? ` and deployed it on ${cluster}` : ""}: commit ${commit.slice(0, 7)}.`);
      }}
      head={
        <div className="grid-2">
          <Field label="Operator name" hint="Its directory under operators/ and its Argo CD Application.">
            <input required value={name} onChange={(e) => setName(e.target.value)} placeholder="qwen38-h200" />
          </Field>
          <Field label="Namespace" hint="Its projectNamespace: one model per namespace.">
            <input value={ns} onChange={(e) => setNamespace(e.target.value)} />
          </Field>
          <Field label="Start from" hint="Its recipe fills the form; its app config is copied.">
            <select value={from} onChange={(e) => setFrom(e.target.value)}>
              {gitops.models.map((m) => (
                <option key={m.name} value={m.name}>
                  {m.model_name} ({m.name})
                </option>
              ))}
            </select>
          </Field>
          <Field label="Deploy on" hint="Optional: also register it on a cluster, in the same commit.">
            <select value={cluster} onChange={(e) => setCluster(e.target.value)}>
              <option value="">Not yet</option>
              {gitops.clusters.map((c) => (
                <option key={c.path} value={c.name}>
                  {c.name} ({c.site}/{c.env})
                </option>
              ))}
            </select>
          </Field>
        </div>
      }
    />
  );
}

/**
 * The values of a model (recipe or one cluster's) as a form: the keys that
 * change between models and clusters, and the whole file as JSON.
 */
function FormDialog(props: {
  title: string;
  submitLabel: string;
  start: Values | null;
  note: string;
  head?: ReactNode;
  onClose: () => void;
  onSubmit: (values: Values) => Promise<void>;
}) {
  const [values, setValues] = useState<Values>(props.start ?? {});
  const [argsText, setArgsText] = useState(() => argsToText(getIn(values, "llmisvc.args")));
  const [envText, setEnvText] = useState(() => envToText(getIn(values, "llmisvc.env")));
  const [lmArgsText, setLmArgsText] = useState(() => argsToText(getIn(values, "lmcache.extraArgs")));
  const [extraText, setExtraText] = useState(() => argsToText(getIn(values, "llmisvc.extraArgs")));
  // The JSON box shows the values, except while someone edits it.
  const [json, setJson] = useState<string | null>(null);
  const set = (path: string, value: unknown) => setValues((v) => setIn(v, path, value));
  const text = (path: string) => String(getIn(values, path) ?? "");
  const argProblems = [...textToArgs(argsText), ...textToArgs(extraText)].map(argProblem).filter(Boolean);
  const envProblem = textToEnv(envText).problem;
  const lmcache = getIn(values, "lmcache.enabled") === true;

  const number = (path: string, label: string, hint?: string) => (
    <Field label={label} hint={hint}>
      <input type="number" min={0} value={text(path)} onChange={(e) => set(path, numberOrEmpty(e.target.value))} />
    </Field>
  );
  const field = (path: string, label: string, hint?: string, placeholder?: string) => (
    <Field label={label} hint={hint}>
      <input value={text(path)} placeholder={placeholder} onChange={(e) => set(path, e.target.value)} />
    </Field>
  );
  const check = (path: string, label: string, hint: string) => (
    <label className="check">
      <input type="checkbox" checked={getIn(values, path) === true} onChange={(e) => set(path, e.target.checked)} />
      {label}
      <small>{hint}</small>
    </label>
  );

  return (
    <FormModal
      title={props.title}
      submitLabel={props.submitLabel}
      onClose={props.onClose}
      onSubmit={async () => {
        if (!props.start) throw new Error("The values are still loading.");
        if (argProblems.length > 0) throw new Error(argProblems[0]);
        if (envProblem) throw new Error(envProblem);
        await props.onSubmit(values);
      }}
      wide
    >
      {props.head}
      <p className="hint">{props.note}</p>
      {!props.start ? (
        <p className="muted">Loading the values…</p>
      ) : (
        <>
          <h3>Model and weights</h3>
          <div className="grid-2">
            {field("model.name", "Model name", "What tenants send as model.")}
            {field("model.s3.path", "S3 bucket and path", "s3://<this>/*: what the per-node pull jobs download.", "llms/zai-org/GLM-5.3")}
            {field("model.subPath", "Directory in the models PVC", "Set: the weights are on the node disks (node prep + pull jobs).")}
            {field("model.uri", "Weights URI", "Without node disks: hf://... or s3://..., into a PVC or each pod.")}
            {field("model.revision", "Revision", "The checkpoint; sites with different revisions do not share traffic.")}
            {number("model.maxModelLen", "Context length", "Empty: as in the vLLM flags, or vLLM's choice.")}
          </div>
          <h3>Images</h3>
          <div className="grid-3">
            {field("images.vllm", "vLLM image")}
            {field("lmcache.image.repository", "LMCache image", "The same build as vLLM.")}
            {field("lmcache.image.tag", "LMCache tag")}
          </div>
          <h3>Shape on the nodes</h3>
          <div className="grid-3">
            {number("llmisvc.replicas", "Replicas", "Copies of the model; 0 stops it.")}
            <Field label="GPUs per pod" hint="Requests and limits of nvidia.com/gpu.">
              <input type="number" min={0} value={gpusOf(values) || ""} onChange={(e) => setValues((v) => setGpus(v, numberOrEmpty(e.target.value)))} />
            </Field>
            {field("nodes.gpuProduct", "GPU product", "Node label: NVIDIA-B300, NVIDIA-H200, ...")}
            {number("llmisvc.parallelism.tensor", "Tensor parallel", "GPUs one rank splits the model over (H200: 8, or 2 with DP).")}
            {number("llmisvc.dataParallelExternalLB.ranks", "Data-parallel ranks, one port each", "One node: each rank its own scheduler endpoint (B300/B200: 8).")}
            {check("llmisvc.parallelism.expert", "Expert parallel", "Experts spread over data × tensor GPUs (or --enable-expert-parallel below).")}
          </div>
          <div className="grid-3">
            {check("llmisvc.multiNode", "Over several nodes", "A leader and workers (LeaderWorkerSet), one pod per node.")}
            {getIn(values, "llmisvc.multiNode") === true && number("llmisvc.parallelism.data", "Data-parallel ranks in all", "2 × H200, DP8 TP2: 8.")}
            {getIn(values, "llmisvc.multiNode") === true && number("llmisvc.parallelism.dataLocal", "Ranks per pod", "2 × H200, DP8 TP2: 4.")}
            {check("llmisvc.roce", "RoCE", "KServe finds the active HCAs and the GID index.")}
            {getIn(values, "llmisvc.roce") === true && (
              <Field label="RDMA network" hint="k8s.v1.cni.cncf.io/networks of the pods.">
                <input
                  value={String(getIn(values, "llmisvc.podAnnotations.k8s\u0000v1\u0000cni\u0000cncf\u0000io/networks") ?? "")}
                  onChange={(e) => set("llmisvc.podAnnotations.k8s\u0000v1\u0000cni\u0000cncf\u0000io/networks", e.target.value)}
                />
              </Field>
            )}
          </div>
          <div className="grid-3">
            {field("llmisvc.resources.requests.cpu", "CPU request")}
            {field("llmisvc.resources.requests.memory", "Memory request")}
            {field("llmisvc.volumes.dshmSize", "/dev/shm", "Tensor and data parallel need room here.")}
          </div>
          <Field label="Nodes" hint="Hostnames, comma-separated: the local PVs' nodes (node disks only).">
            <input
              value={Array.isArray(getIn(values, "nodes.hostnames")) ? (getIn(values, "nodes.hostnames") as string[]).join(", ") : ""}
              onChange={(e) =>
                set(
                  "nodes.hostnames",
                  e.target.value
                    .split(",")
                    .map((h) => h.trim())
                    .filter(Boolean),
                )
              }
            />
          </Field>
          <Field label="This shape's vLLM flags" hint="Before the model's flags: what this cluster adds (--enable-expert-parallel, --all2all-backend=...). One per line.">
            <textarea
              className="mono"
              rows={3}
              value={extraText}
              onChange={(e) => {
                setExtraText(e.target.value);
                set("llmisvc.extraArgs", textToArgs(e.target.value));
              }}
            />
          </Field>
          <Field label="The model's vLLM flags" hint="One per line, options in dot notation, or --option='value' (JSON in single quotes). A cluster that changes one repeats the list: Helm replaces lists.">
            <textarea
              className="mono"
              rows={8}
              value={argsText}
              onChange={(e) => {
                setArgsText(e.target.value);
                set("llmisvc.args", textToArgs(e.target.value));
              }}
            />
          </Field>
          {argProblems.length > 0 && <div className="warn-text">{argProblems.join("; ")}</div>}
          <Field label="vLLM environment" hint="NAME=value, one per line.">
            <textarea
              className="mono"
              rows={4}
              value={envText}
              onChange={(e) => {
                setEnvText(e.target.value);
                const parsed = textToEnv(e.target.value);
                if (!parsed.problem) set("llmisvc.env", Object.keys(parsed.env).length ? parsed.env : undefined);
              }}
            />
          </Field>
          {envProblem && <div className="warn-text">{envProblem}</div>}
          <h3>LMCache</h3>
          <div className="grid-3">
            {check("lmcache.enabled", "LMCache", "Node-local KV cache: RAM, then disk.")}
            {lmcache && number("lmcache.server.chunkSize", "Chunk size", "Tied to the model and TP shape (256 GLM-5.3, 1600 Qwen3.8 TP4).")}
            {lmcache && number("lmcache.l1.sizeGB", "RAM (GB)")}
            {lmcache && check("lmcache.l2.enabled", "Disk tier", "On the node's L2 PVC.")}
            {lmcache && number("lmcache.l2.maxCapacityGB", "Disk (GB)", "Below storage.l2.pvSize.")}
          </div>
          {lmcache && (
            <Field label="LMCache flags" hint="Appended to the engine's arguments, one per line.">
              <textarea
                className="mono"
                rows={2}
                value={lmArgsText}
                onChange={(e) => {
                  setLmArgsText(e.target.value);
                  set("lmcache.extraArgs", textToArgs(e.target.value));
                }}
              />
            </Field>
          )}
          <h3>Weights without node disks</h3>
          <div className="grid-3">
            {check("modelCache.enabled", "Keep on a PVC", "Downloaded once from the weights URI into the cluster's storage class.")}
            {getIn(values, "modelCache.enabled") === true && field("modelCache.size", "PVC size", undefined, "20Gi")}
          </div>
          <h3>Scheduler</h3>
          <div className="grid-3">
            {check("llmisvc.flowControl.enabled", "Flow control", "Best-effort waits behind everyone; tenants in turn.")}
            {getIn(values, "llmisvc.flowControl.enabled") === true &&
              number("llmisvc.flowControl.maxConcurrency", "In flight per endpoint", "Per rank; never above --max-num-seqs.")}
            {number("peer.maxParallelRequests", "Site limit", "Requests at once before 503 and the next site. 0: none.")}
          </div>
          <details>
            <summary>All values (JSON)</summary>
            <p className="hint">Every key of the chart's values. Leave the box to apply what you typed.</p>
            <textarea
              className="mono"
              rows={18}
              value={json ?? JSON.stringify(values, null, 2)}
              onChange={(e) => setJson(e.target.value)}
              onBlur={() => {
                if (json === null) return;
                try {
                  const v = JSON.parse(json) as Values;
                  setValues(v);
                  setArgsText(argsToText(getIn(v, "llmisvc.args")));
                  setEnvText(envToText(getIn(v, "llmisvc.env")));
                  setLmArgsText(argsToText(getIn(v, "lmcache.extraArgs")));
                  setExtraText(argsToText(getIn(v, "llmisvc.extraArgs")));
                  setJson(null);
                } catch {
                  // Kept as typed until it parses; the form keeps the last values.
                }
              }}
            />
          </details>
        </>
      )}
    </FormModal>
  );
}
