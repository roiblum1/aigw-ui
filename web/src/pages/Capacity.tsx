import { Fragment, useState } from "react";
import { Gauge } from "lucide-react";
import {
  api,
  type CapacityGPU,
  type CapacityModel,
  type CapacityPlan,
  type CapacityPrices,
  type CapacityRun,
  type CapacityShapePlan,
  type Model,
} from "../api";
import { Bar, LoadChart, short } from "../charts";
import {
  Empty,
  ErrorBanner,
  Field,
  FormModal,
  Modal,
  PageHeader,
  formatDollars,
  formatPrice,
  formatTime,
  useAction,
  useLoad,
  usePolling,
  type Unit,
} from "../components";
import ModelPrices from "./ModelPrices";

/** Tokens a second, short: 12.3k. */
const rate = (n: number) => (n >= 1000 ? `${(n / 1000).toFixed(1)}k` : Math.round(n).toLocaleString("en-US"));
const percent = (n: number) => `${Math.round(n * 100)}%`;
const seconds = (ms: number) => (ms >= 1000 ? `${(ms / 1000).toFixed(1)} s` : `${Math.round(ms)} ms`);
/** A sentence from the server that starts in the middle: "at 32 in flight: …". */
const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

/** An amount per day: dollars, or tokens written short. */
function perDay(n: number, unit: Unit): string {
  return unit === "credits" ? formatDollars(n) : `${short(n)} tokens`;
}

/**
 * What one instance of each shape of a model serves at the latency promised
 * to the tenants, measured with AIPerf in a Job on a cluster, and what
 * follows from it: what the model can be given a day, what it has been
 * given, and the one set of prices that pays for its instances.
 */
export default function Capacity() {
  const { data, error, reload } = useLoad(api.capacity);
  const running = data?.models.some((m) => m.running) ?? false;
  usePolling(reload, running ? 15_000 : 60_000);
  const [measuring, setMeasuring] = useState<{ m: CapacityModel; shape: string } | null>(null);
  const [history, setHistory] = useState<CapacityModel | null>(null);
  const [pricing, setPricing] = useState(false);
  const [pricesFor, setPricesFor] = useState<{ model: Model; suggested: CapacityPrices; note: string } | null>(null);
  const action = useAction();

  // The prices form edits a model as the Models page has it.
  const usePrices = (m: CapacityModel) =>
    action.run(async () => {
      const model = (await api.models()).find((x) => x.id === m.model_id);
      if (!model || !m.plan.break_even) return;
      setPricesFor({
        model,
        suggested: m.plan.break_even,
        note: `Break-even from the capacity measurement at ${percent(m.plan.utilization)} busy`,
      });
    });
  const dollarModels = data?.models.filter((m) => m.unit === "credits" && m.plan.measured_instances > 0) ?? [];
  const total = (f: (p: CapacityPlan) => number) => dollarModels.reduce((n, m) => n + f(m.plan), 0);

  const stop = (run: CapacityRun) => {
    if (!confirm(`Stop measuring ${run.model_name}? Its Job is deleted; the steps it finished are kept.`)) return;
    action.run(async () => {
      await api.cancelCapacityRun(run.id);
      await reload();
    });
  };
  const unpriced = data?.gpus.filter((g) => g.usd_per_hour == null && g.deployments > 0) ?? [];

  return (
    <section>
      <PageHeader
        title="Capacity"
        subtitle="What each shape of a model serves at the latency you promise, what the model can therefore be given a day, how much of that the budgets take, and the prices that pay for its GPUs and no more."
      >
        <button onClick={() => setPricing(true)}>GPU prices</button>
      </PageHeader>
      <ErrorBanner message={error || action.error} />
      {data && !data.image && (
        <div className="banner warn">
          No AIPerf image is set, so nothing can be measured. Set <span className="mono">capacity.image</span> in the
          chart to an image the clusters can pull; see the capacity guide under Docs.
        </div>
      )}
      {unpriced.length > 0 && (
        <div className="banner warn">
          {unpriced.map((g) => g.gpu).join(", ")} {unpriced.length === 1 ? "has" : "have"} no price, so the cost of the
          models on {unpriced.length === 1 ? "it" : "them"} is not worked out.{" "}
          <button className="link" onClick={() => setPricing(true)}>
            Set GPU prices
          </button>
        </div>
      )}
      {dollarModels.length > 0 && (
        <div className="banner info">
          The {dollarModels.length === 1 ? "measured model" : `${dollarModels.length} measured models`} counted in
          dollars can be given <span className="strong">{formatDollars(total((p) => p.capacity_day))}</span> a day;
          with {dollarModels.length === 1 ? "its overcommit limit" : "their overcommit limits"}, up to{" "}
          {formatDollars(total((p) => p.ceiling_day))}. The budgets take{" "}
          {formatDollars(total((p) => p.given_day))}, so{" "}
          <LeftToGive left={total((p) => p.left_day)} format={formatDollars} /> a day.
        </div>
      )}
      {data?.models.length === 0 && <Empty icon={Gauge}>No models yet. Add one on the Models page.</Empty>}
      {data?.models.map((m) => (
        <ModelCard
          key={m.model_id}
          m={m}
          canMeasure={!!data.image && !action.busy}
          onMeasure={(shape) => setMeasuring({ m, shape })}
          onHistory={() => setHistory(m)}
          onStop={stop}
          onChanged={reload}
          onUsePrices={() => usePrices(m)}
        />
      ))}
      {measuring && <MeasureForm m={measuring.m} shape={measuring.shape} onClose={() => setMeasuring(null)} onStarted={reload} />}
      {history && <RunHistory m={history} onClose={() => setHistory(null)} onChanged={reload} />}
      {pricing && data && <GPUPrices gpus={data.gpus} onClose={() => setPricing(false)} onChanged={reload} />}
      {pricesFor && (
        <ModelPrices
          model={pricesFor.model}
          suggested={pricesFor.suggested}
          suggestedNote={pricesFor.note}
          onClose={() => setPricesFor(null)}
          onChanged={async () => {
            setPricesFor(null);
            await reload();
          }}
        />
      )}
    </section>
  );
}

function ModelCard(props: {
  m: CapacityModel;
  canMeasure: boolean;
  onMeasure: (shape: string) => void;
  onHistory: () => void;
  onStop: (run: CapacityRun) => void;
  onChanged: () => Promise<void>;
  onUsePrices: () => void;
}) {
  const { m } = props;
  const [open, setOpen] = useState("");
  const action = useAction();
  const editOvercommit = () => {
    const text = prompt(
      `How far ${m.model_name}'s budgets may add up beyond what its GPUs serve a day at the expected busy share, as a factor from 1 to 10. Tenants rarely use all of their budgets on the same day, so more than 1 keeps the GPUs busier.`,
      String(m.plan.overcommit_limit),
    );
    if (text == null) return;
    const n = Number(text);
    if (!(n >= 1 && n <= 10)) {
      alert("Give a number from 1 to 10.");
      return;
    }
    action.run(async () => {
      await api.setOvercommitLimit(m.model_id, n);
      await props.onChanged();
    });
  };
  const editTarget = () => {
    const text = prompt(
      `The share of the day ${m.model_name}'s instances are expected to be busy, in percent. The prices that pay for them are worked out at it.`,
      String(Math.round(m.target_utilization * 100)),
    );
    if (text == null) return;
    const n = Number(text);
    if (!(n >= 5 && n <= 100)) {
      alert("Give a number from 5 to 100.");
      return;
    }
    action.run(async () => {
      await api.setTargetUtilization(m.model_id, n / 100);
      await props.onChanged();
    });
  };
  const latest = (key: string) => m.latest.find((r) => r.shape.key === key);

  return (
    <div className="card">
      <div className="dash-head">
        <div>
          <h2>{m.model_name}</h2>
          <p className="detail">
            Expected busy share {percent(m.target_utilization)}{" "}
            <button className="link" disabled={action.busy} onClick={editTarget}>
              change
            </button>
            {m.plan.observed_utilization != null && <> · an average day uses {percent(m.plan.observed_utilization)}</>}
            {" · "}overcommit limit {m.plan.overcommit_limit}×{" "}
            <button className="link" disabled={action.busy} onClick={editOvercommit}>
              change
            </button>
          </p>
        </div>
        <div className="row-actions">
          <button onClick={props.onHistory}>Measurements</button>
          {m.running && (
            <button className="danger" onClick={() => props.onStop(m.running!)}>
              Stop
            </button>
          )}
        </div>
      </div>
      <ErrorBanner message={action.error} />
      {m.running && <Running run={m.running} />}
      {m.deployments.length === 0 ? (
        <p className="detail">No site has a deployment of this model, or discovery has not read it yet.</p>
      ) : (
        <>
          <Summary plan={m.plan} onUsePrices={props.onUsePrices} />
          <div className="table-wrap">
            <table className="plain">
              <thead>
                <tr>
                  <th>Shape</th>
                  <th>Sites</th>
                  <th className="text-right">Ready</th>
                  <th className="text-right wrap">In flight within targets</th>
                  <th className="text-right wrap">One instance a day</th>
                  <th className="text-right wrap">Cost an hour</th>
                  <th className="text-right wrap" title="What a million input-equivalent tokens cost on this shape busy all day">
                    Cost per 1M
                  </th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {m.plan.shapes.map((s) => {
                  const run = latest(s.shape.key);
                  return (
                    <Fragment key={s.shape.key}>
                      <ShapeRow
                        s={s}
                        unit={m.unit}
                        run={run}
                        canMeasure={props.canMeasure && !m.running && s.shape.topology !== "prefill-decode"}
                        open={open === s.shape.key}
                        onToggle={() => setOpen(open === s.shape.key ? "" : s.shape.key)}
                        onMeasure={() => props.onMeasure(s.shape.key)}
                      />
                      {open === s.shape.key && run && (
                        <tr>
                          <td colSpan={8}>
                            <div className="run-cell">
                              <RunDetail run={run} />
                            </div>
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          </div>
        </>
      )}
      {m.plan.notes.map((n) => (
        <p key={n} className="detail">
          {n}
        </p>
      ))}
    </div>
  );
}

function ShapeRow(props: {
  s: CapacityShapePlan;
  unit: Unit;
  run?: CapacityRun;
  canMeasure: boolean;
  open: boolean;
  onToggle: () => void;
  onMeasure: () => void;
}) {
  const { s, run } = props;
  const pd = s.shape.topology === "prefill-decode";
  return (
    <tr>
      <td className="shape-cell">
        {s.shape.label}
        {run && (
          <div className="detail">
            Measured {formatTime(run.created_at)} on {run.cluster_name}
          </div>
        )}
      </td>
      <td className="nowrap">{s.sites.join(", ")}</td>
      <td className="text-right tabular-nums">{s.instances}</td>
      <td className="text-right tabular-nums">{s.measured ? s.concurrency : <span className="tag warn">not measured</span>}</td>
      <td className="text-right tabular-nums">{s.measured ? perDay(s.per_instance_day, props.unit) : "–"}</td>
      <td className="text-right tabular-nums">{s.cost_per_instance_hour == null ? "–" : formatDollars(s.cost_per_instance_hour)}</td>
      <td className="text-right tabular-nums">{s.cost_per_million == null ? "–" : formatPrice(s.cost_per_million)}</td>
      <td className="row-actions">
        {run && <button onClick={props.onToggle}>{props.open ? "Hide steps" : "Steps"}</button>}
        <button
          className={s.measured ? undefined : "primary"}
          disabled={!props.canMeasure}
          title={
            pd
              ? "An instance of this shape is prefill and decode pods together, which is not measured yet."
              : "Measure one ready pod of this shape with AIPerf. It runs as a Job on its cluster."
          }
          onClick={props.onMeasure}
        >
          Measure
        </button>
      </td>
    </tr>
  );
}

/** What is left to give, or how far the budgets are past the ceiling. */
function LeftToGive({ left, format }: { left: number; format: (n: number) => string }) {
  return left >= 0 ? (
    <>
      <span className="strong ok-text">{format(left)}</span> is left to give
    </>
  ) : (
    <>
      the budgets are <span className="strong warn-text">{format(-left)}</span> past the ceiling
    </>
  );
}

function Summary({ plan, onUsePrices }: { plan: CapacityPlan; onUsePrices: () => void }) {
  const show = (n: number) => perDay(n, plan.unit);
  return (
    <div className="dash-grid">
      <div className="panel">
        <h3>What the model can be given a day</h3>
        {plan.capacity_day > 0 ? (
          <Bar label="Given to tenants, against the ceiling" used={plan.given_day} limit={plan.ceiling_day} format={show} />
        ) : (
          <p className="detail">Nothing yet: no measured shape has a ready instance.</p>
        )}
        {plan.measured_instances > 0 && (
          <>
            <p className="detail">
              What the {plan.measured_instances} measured{" "}
              {plan.measured_instances === 1 ? "instance serves" : "instances serve"} busy all day, {show(plan.full_day)},
              × {percent(plan.utilization)} busy = <span className="strong">{show(plan.capacity_day)}</span>; ×{" "}
              {plan.overcommit_limit} overcommit = a ceiling of {show(plan.ceiling_day)}.
            </p>
            <p className="detail">
              The budgets take {show(plan.given_day)}
              {plan.short_windows > 0 &&
                ` (${plan.short_windows} hourly or per-minute ${plan.short_windows === 1 ? "budget" : "budgets"} at the most a day allows)`}
              {plan.overcommit != null && `, ${plan.overcommit.toFixed(2)}× what the GPUs serve`}, so{" "}
              <LeftToGive left={plan.left_day} format={show} />.
            </p>
          </>
        )}
        {plan.pool_day != null && (
          <p className="detail">
            The shared pool lets tenants past their budget use up to {show(plan.pool_day)} a day between them.
          </p>
        )}
        {plan.safe_allocated_day != null && (
          <p className="detail">
            If tenants keep using their budgets as on the busiest day, the budgets could add up to{" "}
            <span className="strong">{show(plan.safe_allocated_day)}</span> a day before such a day fills it.
          </p>
        )}
      </div>

      <div className="panel">
        <h3>Busiest day in 30 days</h3>
        {plan.busiest_share != null && plan.full_day > 0 ? (
          <>
            <Bar label="Used" used={plan.busiest_day} limit={plan.full_day} format={show} />
            <p className="detail">Out of what the measured instances serve busy all day.</p>
          </>
        ) : (
          <p className="detail">No usage recorded yet, or nothing measured.</p>
        )}
        {plan.observed_utilization != null && (
          <p className="detail">
            An average day used {percent(plan.observed_utilization)} of it; the prices are worked out at{" "}
            {percent(plan.utilization)}.
          </p>
        )}
      </div>

      <div className="panel">
        <h3>Cost and price</h3>
        {plan.cost_per_hour == null ? (
          <p className="detail">Set the price of every GPU the measured shapes run on to see what they cost.</p>
        ) : (
          <p className="detail">
            The measured instances cost {formatDollars(plan.cost_per_hour)} an hour,{" "}
            <span className="strong">{formatDollars(plan.cost_per_hour * 24)}</span> a day.
          </p>
        )}
        {plan.charged_per_hour != null && (
          <p className="detail">
            Busy for an hour at the model's prices, they are charged {formatDollars(plan.charged_per_hour)}
            {plan.cost_per_hour ? `: ${(plan.charged_per_hour / plan.cost_per_hour).toFixed(2)}× their cost` : ""}.
          </p>
        )}
        {plan.break_even && (
          <p className="detail">
            One set of prices that pays for them at {percent(plan.utilization)} busy, per 1M tokens:{" "}
            <span className="strong">
              {formatPrice(plan.break_even.price_input)} input, {formatPrice(plan.break_even.price_cached)} cached,{" "}
              {formatPrice(plan.break_even.price_output)} output
            </span>
            . Tenants pay it whichever shape serves them, and at these prices what the GPUs serve a day at{" "}
            {percent(plan.utilization)} busy is worth what they cost a day.{" "}
            <button className="link" onClick={onUsePrices}>
              Use these prices
            </button>
          </p>
        )}
      </div>
    </div>
  );
}

function Running({ run }: { run: CapacityRun }) {
  const last = run.steps[run.steps.length - 1];
  return (
    <div className="banner info">
      Measuring {run.shape.label}: pod {run.namespace}/{run.instance} on {run.cluster_name} since{" "}
      {formatTime(run.created_at)}.{" "}
      {last
        ? `${run.steps.length} of ${run.settings.concurrency.length} steps done, the last at ${last.concurrency} in flight.`
        : run.dataset
          ? `${run.dataset.sessions} sessions made up, prompts up to ${run.dataset.prompt_max.toLocaleString("en-US")} tokens; the first step is running.`
          : "Making up the sessions."}{" "}
      It stops at the first step that misses a target.
    </div>
  );
}

function RunDetail({ run }: { run: CapacityRun }) {
  return (
    <>
      <p className="detail">
        Pod {run.namespace}/{run.instance}.{" "}
        {run.dataset &&
          `${run.dataset.sessions} sessions, prompts of ${Math.round(run.dataset.prompt_mean).toLocaleString("en-US")} tokens on average and up to ${run.dataset.prompt_max.toLocaleString("en-US")}. `}
        {run.result.limit && `${sentence(run.result.limit)}.`}
      </p>
      <LoadChart
        steps={run.steps.filter((s) => !s.error)}
        ttftTarget={run.settings.ttft_p95_ms}
        title={`Output tokens a second and time to first token at each load step of ${run.model_name}`}
      />
      <Steps run={run} />
    </>
  );
}

function Steps({ run }: { run: CapacityRun }) {
  if (run.steps.length === 0) return <p className="detail">No step has finished.</p>;
  return (
    <div className="table-wrap">
      <table className="plain">
        <thead>
          <tr>
            <th className="text-right">In flight</th>
            <th className="text-right">Answered</th>
            <th className="text-right wrap">Prompt tok/s</th>
            <th className="text-right wrap">Output tok/s</th>
            <th className="text-right">From cache</th>
            <th className="text-right wrap">First token p50 / p95</th>
            <th className="text-right wrap">Between tokens p50 / p95</th>
            <th>Targets</th>
          </tr>
        </thead>
        <tbody>
          {run.steps.map((s, i) => {
            const v = run.result.steps[i];
            return (
              <tr key={s.concurrency} className={run.result.best?.concurrency === s.concurrency ? "best" : undefined}>
                <td className="text-right tabular-nums">{s.concurrency}</td>
                {s.error ? (
                  <td colSpan={6} className="detail">
                    {s.error}
                  </td>
                ) : (
                  <>
                    <td className="text-right tabular-nums">
                      {s.requests}
                      {s.errors > 0 && <span className="detail warn-text"> ({s.errors} failed)</span>}
                    </td>
                    <td className="text-right tabular-nums">{rate(v.input_per_s)}</td>
                    <td className="text-right tabular-nums">{rate(v.output_per_s)}</td>
                    <td className="text-right tabular-nums">{v.cached_share == null ? "–" : percent(v.cached_share)}</td>
                    <td className="text-right tabular-nums">
                      {seconds(s.ttft_p50_ms)} / {seconds(s.ttft_p95_ms)}
                    </td>
                    <td className="text-right tabular-nums">
                      {seconds(s.itl_p50_ms)} / {seconds(s.itl_p95_ms)}
                    </td>
                  </>
                )}
                <td>
                  {v.met ? (
                    <span className="tag">met</span>
                  ) : (
                    <span className="tag warn" title={v.reason}>
                      missed
                    </span>
                  )}
                  {v.few_samples && (
                    <span className="tag warn" title="The step reached its longest time before enough requests were answered">
                      few samples
                    </span>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

const targetKey = (x: { cluster_id: string; namespace: string; name: string }) => `${x.cluster_id}/${x.namespace}/${x.name}`;

function MeasureForm(props: { m: CapacityModel; shape: string; onClose: () => void; onStarted: () => Promise<void> }) {
  const { m } = props;
  const d = m.defaults;
  const candidates = m.deployments.filter((x) => x.shape.key === props.shape && !x.drained);
  const [target, setTarget] = useState(candidates[0] ? targetKey(candidates[0]) : "");
  const chosen = candidates.find((x) => targetKey(x) === target);
  const pods = useLoad(() => (chosen ? api.deploymentPods(chosen.cluster_id, chosen.namespace, chosen.name) : Promise.resolve([])));
  const [pod, setPod] = useState("");
  const [accept, setAccept] = useState(false);
  const [steps, setSteps] = useState(d.concurrency.join(", "));
  const [minRequests, setMinRequests] = useState(String(d.min_requests));
  const [stepSeconds, setStepSeconds] = useState(String(d.step_seconds));
  const [sessions, setSessions] = useState(String(d.sessions));
  const longest = chosen?.max_model_len ? Math.min(d.max_prompt_tokens, chosen.max_model_len - d.max_output_tokens) : d.max_prompt_tokens;
  const [maxPrompt, setMaxPrompt] = useState(String(longest));
  const [maxOutput, setMaxOutput] = useState(String(d.max_output_tokens));
  const [ttft, setTTFT] = useState(String(d.ttft_p95_ms));
  const [itl, setITL] = useState(String(d.itl_p95_ms));

  const start = async () => {
    if (!chosen) throw new Error("Choose a deployment.");
    if (!pod) throw new Error("Choose the pod to measure.");
    if (!accept) throw new Error("Confirm that the pod's users may get slower answers.");
    await api.startCapacityRun(m.model_id, {
      cluster_id: chosen.cluster_id,
      namespace: chosen.namespace,
      deployment: chosen.name,
      pod,
      settings: {
        concurrency: steps
          .split(/[\s,]+/)
          .filter(Boolean)
          .map(Number),
        min_requests: Number(minRequests),
        step_seconds: Number(stepSeconds),
        sessions: Number(sessions),
        max_prompt_tokens: Number(maxPrompt),
        max_output_tokens: Number(maxOutput),
        ttft_p95_ms: Number(ttft),
        itl_p95_ms: Number(itl),
      },
    });
    await props.onStarted();
  };

  return (
    <FormModal
      title={`Measure ${m.model_name}: ${chosen?.shape.label ?? ""}`}
      submitLabel="Start"
      onClose={props.onClose}
      onSubmit={start}
      wide
    >
      <p className="hint">
        AIPerf runs in a Job on the cluster and sends made-up coding-agent sessions straight to one pod, past the gateway
        and the scheduler: a shared system prompt and tools, repository files, and a history that grows each turn. Every
        step keeps a number of requests in flight until enough are answered; the first step that misses a target ends the
        measurement. The result counts for every deployment of this shape. Nothing of a prompt reaches this server, and no
        tenant is charged.
      </p>
      <div className="grid-2">
        <Field
          label="Deployment"
          hint={
            chosen?.max_model_len
              ? `Takes up to ${chosen.max_model_len.toLocaleString("en-US")} tokens.`
              : "It does not declare how long a request it takes (aigw-ui.io/max-model-len). The Job asks the pod and stops at once if the longest prompt and answer do not fit."
          }
        >
          <select
            value={target}
            onChange={(e) => {
              setTarget(e.target.value);
              setPod("");
              setTimeout(pods.reload);
            }}
          >
            {candidates.map((x) => (
              <option key={targetKey(x)} value={targetKey(x)}>
                {x.cluster_name}: {x.namespace}/{x.name} ({x.instances ?? "?"} ready)
              </option>
            ))}
          </select>
        </Field>
        <Field label="Pod" hint={pods.error || "A pod that takes no traffic gives the cleanest figure, such as one extra replica."}>
          <select required value={pod} onChange={(e) => setPod(e.target.value)}>
            <option value="">
              {pods.error ? "The pods could not be read" : !pods.data ? "Reading the pods…" : pods.data.length ? "Choose a ready pod" : "No ready pod"}
            </option>
            {pods.data?.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        </Field>
      </div>
      <label className="check">
        <input type="checkbox" checked={accept} onChange={(e) => setAccept(e.target.checked)} />
        <span>The pod keeps its own traffic while it is measured, and its users get slower answers until it ends.</span>
      </label>
      <h3>Load</h3>
      <div className="grid-3">
        <Field label="Requests in flight, by step" hint="Ascending, up to 12 steps.">
          <input required value={steps} onChange={(e) => setSteps(e.target.value)} />
        </Field>
        <Field label="Requests per step, at least" hint="Four per request in flight above it. A p95 needs enough.">
          <input required type="number" min={10} max={2000} value={minRequests} onChange={(e) => setMinRequests(e.target.value)} />
        </Field>
        <Field label="Longest a step may take, s" hint="A step that runs out first is marked as having few samples.">
          <input required type="number" min={60} max={3600} value={stepSeconds} onChange={(e) => setStepSeconds(e.target.value)} />
        </Field>
        <Field label="Sessions" hint="Made up once, replayed at every step.">
          <input required type="number" min={10} max={5000} value={sessions} onChange={(e) => setSessions(e.target.value)} />
        </Field>
        <Field label="Longest prompt, tokens" hint="Sessions grow to this; about 60% of it on average.">
          <input required type="number" min={1000} max={1000000} value={maxPrompt} onChange={(e) => setMaxPrompt(e.target.value)} />
        </Field>
        <Field label="Longest answer, tokens">
          <input required type="number" min={16} max={32768} value={maxOutput} onChange={(e) => setMaxOutput(e.target.value)} />
        </Field>
      </div>
      <h3>What you promise</h3>
      <div className="grid-3">
        <Field label="Time to first token p95, ms">
          <input required type="number" min={1} max={600000} value={ttft} onChange={(e) => setTTFT(e.target.value)} />
        </Field>
        <Field label="Time between tokens p95, ms" hint="50 ms is 20 tokens a second for each user.">
          <input required type="number" min={1} max={10000} step="any" value={itl} onChange={(e) => setITL(e.target.value)} />
        </Field>
      </div>
    </FormModal>
  );
}

function GPUPrices(props: { gpus: CapacityGPU[]; onClose: () => void; onChanged: () => Promise<void> }) {
  const [rows, setRows] = useState(() => props.gpus.map((g) => ({ ...g, text: g.usd_per_hour == null ? "" : String(g.usd_per_hour) })));
  const [added, setAdded] = useState("");
  const action = useAction();
  const change = (i: number, patch: Partial<(typeof rows)[number]>) => setRows(rows.map((x, j) => (j === i ? { ...x, ...patch } : x)));
  const save = (i: number) =>
    action.run(async () => {
      await api.setGPUPrice(rows[i].gpu, Number(rows[i].text), rows[i].note);
      change(i, { usd_per_hour: Number(rows[i].text) });
      await props.onChanged();
    });
  const remove = (i: number) =>
    action.run(async () => {
      await api.deleteGPUPrice(rows[i].gpu);
      change(i, { usd_per_hour: null, text: "" });
      await props.onChanged();
    });
  return (
    <Modal title="GPU prices" onClose={props.onClose} wide>
      <ErrorBanner message={action.error} />
      <p className="hint">
        What one GPU costs an hour, all in: the hardware over its life, power, cooling, rack, network and the people who
        run it. An instance costs its GPUs times this. The GPUs are read from the node label{" "}
        <span className="mono">nvidia.com/gpu.product</span> of the model's pods.
      </p>
      <div className="table-wrap">
        <table className="plain">
          <thead>
            <tr>
              <th>GPU</th>
              <th className="text-right">Deployments</th>
              <th>$ an hour</th>
              <th>Note</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => (
              <tr key={r.gpu}>
                <td className="mono">{r.gpu}</td>
                <td className="text-right tabular-nums">{r.deployments}</td>
                <td>
                  <input
                    type="number"
                    min={0.01}
                    max={1000}
                    step="any"
                    value={r.text}
                    aria-label={`Price of ${r.gpu}`}
                    onChange={(e) => change(i, { text: e.target.value })}
                  />
                </td>
                <td>
                  <input value={r.note} maxLength={500} aria-label={`Note on ${r.gpu}`} onChange={(e) => change(i, { note: e.target.value })} />
                </td>
                <td className="row-actions">
                  <button className="primary" disabled={action.busy || !(Number(r.text) > 0)} onClick={() => save(i)}>
                    Save
                  </button>
                  {r.usd_per_hour != null && (
                    <button className="danger" disabled={action.busy} onClick={() => remove(i)}>
                      Remove
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="inline-form">
        <input placeholder="Another GPU, as its node label names it" value={added} onChange={(e) => setAdded(e.target.value)} />
        <button
          disabled={!added.trim() || rows.some((r) => r.gpu === added.trim())}
          onClick={() => {
            setRows([...rows, { gpu: added.trim(), usd_per_hour: null, note: "", deployments: 0, text: "" }]);
            setAdded("");
          }}
        >
          Add
        </button>
      </div>
    </Modal>
  );
}

function RunHistory(props: { m: CapacityModel; onClose: () => void; onChanged: () => Promise<void> }) {
  const runs = useLoad(() => api.capacityRuns(props.m.model_id));
  const [open, setOpen] = useState("");
  const action = useAction();
  const remove = (run: CapacityRun) =>
    action.run(async () => {
      await api.deleteCapacityRun(run.id);
      await runs.reload();
      await props.onChanged();
    });
  return (
    <Modal title={`Measurements of ${props.m.model_name}`} onClose={props.onClose} wide>
      <ErrorBanner message={runs.error || action.error} />
      {runs.data?.length === 0 && <p className="detail">None yet.</p>}
      {runs.data?.map((run) => (
        <div key={run.id} className="endpoint">
          <div className="dash-head">
            <div>
              <span className="strong">{formatTime(run.created_at)}</span> · {run.shape.label} · {run.cluster_name}, pod{" "}
              {run.instance} <span className={run.status === "succeeded" ? "tag" : "tag warn"}>{run.status}</span>
              <p className="detail">
                {run.result.best
                  ? `${run.result.best.concurrency} in flight within the targets. ${sentence(run.result.limit)}.`
                  : run.detail || "No step has finished."}
              </p>
            </div>
            <div className="row-actions">
              <button onClick={() => setOpen(open === run.id ? "" : run.id)}>{open === run.id ? "Hide steps" : "Steps"}</button>
              {run.status !== "running" && (
                <button className="danger" disabled={action.busy} onClick={() => remove(run)}>
                  Delete
                </button>
              )}
            </div>
          </div>
          {open === run.id && <RunDetail run={run} />}
        </div>
      ))}
    </Modal>
  );
}
