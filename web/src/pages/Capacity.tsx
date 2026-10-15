import { Fragment, useState } from "react";
import { Gauge } from "lucide-react";
import {
  api,
  type Budgets,
  type BenchmarkProfile,
  type Campaign,
  type CapacityGPU,
  type CapacityLLMI,
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
  const budgets = useLoad(api.budgets);
  const [splitting, setSplitting] = useState(false);
  const running = data?.models.some((m) => m.running) ?? false;
  usePolling(async () => {
    await reload();
    await budgets.reload();
  }, running ? 15_000 : 60_000);
  const [measuring, setMeasuring] = useState<{ m: CapacityModel; llmi: CapacityLLMI } | null>(null);
  const [profiling, setProfiling] = useState(false);
  const [history, setHistory] = useState<CapacityModel | null>(null);
  const [costing, setCosting] = useState(false);
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

  const stop = (run: CapacityRun) => {
    if (!confirm(`Stop measuring ${run.model_name}? Its Job is deleted; the steps it finished are kept.`)) return;
    action.run(async () => {
      await api.cancelCapacityRun(run.id);
      await reload();
    });
  };

  return (
    <section>
      <PageHeader
        title="Capacity"
        subtitle="What each LLMInferenceService of a model serves at the latency you promise, measured with one benchmark profile, what the model can therefore be given a day at its prices, and how much of that the budgets take. GPU costs are optional: they add what the GPUs cost and a price that would pay for them, to check the prices against."
      >
        <button onClick={() => setProfiling(true)} title="The one benchmark every LLMInferenceService is measured with. Editing it makes a new version.">
          Benchmark profile
        </button>
        <button
          onClick={() => setCosting(true)}
          title="Optional. What each GPU costs an hour, used only on this page. Tenants pay the model's prices, the same on every site and GPU."
        >
          GPU costs
        </button>
      </PageHeader>
      <ErrorBanner message={error || action.error} />
      {data && !data.image && (
        <div className="banner warn">
          No AIPerf image is set, so nothing can be measured. Set <span className="mono">capacity.image</span> in the
          chart to an image the clusters can pull; see the capacity guide under Docs.
        </div>
      )}
      {budgets.data && <FleetPanel b={budgets.data} onSplit={() => setSplitting(true)} />}
      {data && <ProfileLine profile={data.profile} onEdit={() => setProfiling(true)} />}
      {data && <CampaignPanel campaign={data.campaign} models={data.models} />}
      {data?.models.length === 0 && <Empty icon={Gauge}>No models yet. Add one on the Models page.</Empty>}
      {data?.models.map((m) => (
        <ModelCard
          key={m.model_id}
          m={m}
          canMeasure={!!data.image && !action.busy}
          peerMeasurements={data.peer_measurements}
          onMeasure={(llmi) => setMeasuring({ m, llmi })}
          onHistory={() => setHistory(m)}
          onStop={stop}
          onChanged={reload}
          onUsePrices={() => usePrices(m)}
        />
      ))}
      {measuring && data && (
        <MeasureForm
          m={measuring.m}
          llmi={measuring.llmi}
          profile={data.profile}
          onClose={() => setMeasuring(null)}
          onStarted={reload}
        />
      )}
      {splitting && budgets.data && (
        <SplitForm
          b={budgets.data}
          onClose={() => setSplitting(false)}
          onApplied={async () => {
            setSplitting(false);
            await budgets.reload();
            await reload();
          }}
        />
      )}
      {profiling && data && (
        <ProfileForm
          profile={data.profile}
          onClose={() => setProfiling(false)}
          onSaved={async () => {
            setProfiling(false);
            await reload();
          }}
        />
      )}
      {history && <RunHistory m={history} onClose={() => setHistory(null)} onChanged={reload} />}
      {costing && data && <GPUCosts gpus={data.gpus} onClose={() => setCosting(false)} onChanged={reload} />}
      {pricesFor && (
        <ModelPrices
          key={pricesFor.model.id}
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
  peerMeasurements: boolean;
  onMeasure: (llmi: CapacityLLMI) => void;
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
      {m.llmis.length === 0 ? (
        <p className="detail">No site has a deployment of this model, or discovery has not read it yet.</p>
      ) : (
        <>
          <Summary plan={m.plan} onUsePrices={props.onUsePrices} />
          <div className="table-wrap">
            <table className="plain">
              <thead>
                <tr>
                  <th>LLMInferenceService</th>
                  <th className="text-right">Ready</th>
                  <th className="text-right wrap">In flight within targets</th>
                  <th className="text-right wrap">One instance a day</th>
                  <th className="text-right wrap">Cost an hour</th>
                  <th className="text-right wrap" title="What a million input-equivalent tokens cost on this LLMI busy all day">
                    Cost per 1M
                  </th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {m.llmis.map((l, i) => {
                  const key = `${l.cluster_id}/${l.namespace}/${l.name}`;
                  return (
                    <Fragment key={key}>
                      <LLMIRow
                        l={l}
                        s={m.plan.shapes[i]}
                        unit={m.unit}
                        version={m.profile_version}
                        canMeasure={props.canMeasure && !m.running}
                        peerMeasurements={props.peerMeasurements}
                        open={open === key}
                        onToggle={() => setOpen(open === key ? "" : key)}
                        onMeasure={() => props.onMeasure(l)}
                      />
                      {open === key && l.run && (
                        <tr>
                          <td colSpan={7}>
                            <div className="run-cell">
                              <RunDetail run={l.run} />
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

/** Why a result should be measured again. */
const staleText: Record<string, string> = {
  spec: "its spec, a config it is built on or its image changed since",
  profile: "measured with an earlier version of the profile",
  age: "older than 30 days",
};

function LLMIRow(props: {
  l: CapacityLLMI;
  s: CapacityShapePlan;
  unit: Unit;
  version: number;
  canMeasure: boolean;
  peerMeasurements: boolean;
  open: boolean;
  onToggle: () => void;
  onMeasure: () => void;
}) {
  const { l, s } = props;
  const run = l.run;
  const serving = [l.quantization && `quantization ${l.quantization}`, l.lmcache && "LMCache"].filter(Boolean).join(", ");
  return (
    <tr>
      <td className="shape-cell">
        <span className="strong">{l.cluster_name}</span> · {l.namespace}/{l.name}
        <div className="detail">
          {l.shape.label}
          {serving && ` · ${serving}`}
          {l.spec_hash && (
            <span className="mono" title={`What it runs, whatever its scale: ${l.spec_hash}`}>
              {" "}
              · spec {l.spec_hash.slice(0, 8)}
            </span>
          )}
        </div>
        {run ? (
          <div className="detail">
            Measured {formatTime(run.finished_at ?? run.created_at)}, profile version {props.version}
            {run.mode === "live" && (
              <span className="tag warn" title="Measured while it served users: their traffic makes the result lower than what it serves alone">
                live
              </span>
            )}
            {l.stale && <span className="tag warn">stale: {staleText[l.stale] ?? l.stale}</span>}
          </div>
        ) : (
          <div className="detail warn-text">{l.why}</div>
        )}
      </td>
      <td className="text-right tabular-nums">{s.instances}</td>
      <td className="text-right tabular-nums">{s.measured ? s.concurrency : <span className="tag warn">not measured</span>}</td>
      <td className="text-right tabular-nums">{s.measured ? perDay(s.per_instance_day, props.unit) : "–"}</td>
      <td className="text-right tabular-nums">{s.cost_per_instance_hour == null ? "–" : formatDollars(s.cost_per_instance_hour)}</td>
      <td className="text-right tabular-nums">{s.cost_per_million == null ? "–" : formatPrice(s.cost_per_million)}</td>
      <td className="row-actions">
        {run && <button onClick={props.onToggle}>{props.open ? "Hide steps" : "Steps"}</button>}
        {l.topology === "prefill-decode" && !props.peerMeasurements ? (
          <span
            className="detail"
            title="Its Job would run in the gateway's namespace with the site's peer client certificate, which reaches every model on the peer listener with no key and no quota. Turn on capacity.peerMeasurements to measure it; see the security guide."
          >
            Measuring prefill/decode is off
          </span>
        ) : (
          <button
            className={s.measured && !l.stale ? undefined : "primary"}
            disabled={!props.canMeasure}
            title="Measure it with the benchmark profile: AIPerf in a Job on its cluster."
            onClick={props.onMeasure}
          >
            Measure
          </button>
        )}
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
          <p className="detail">Nothing yet: no measured LLMInferenceService has a ready instance.</p>
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
        <h3>Prices and cost</h3>
        {plan.unit !== "credits" && (
          <p className="detail">
            The model has no prices, so its budgets are in tokens. Give it prices under{" "}
            <span className="strong">Prices</span> on the Models page to count it in dollars with the others.
          </p>
        )}
        {plan.cost_per_hour == null ? (
          <p className="detail">
            Optional: with a cost for its GPUs under <span className="strong">GPU costs</span>, this shows what the
            measured instances cost a day and prices that would pay for them, to check the model's prices against.
            Tenants pay the model's prices, the same on every site and GPU.
          </p>
        ) : (
          <p className="detail">
            The measured instances cost {formatDollars(plan.cost_per_hour)} an hour,{" "}
            <span className="strong">{formatDollars(plan.cost_per_hour * 24)}</span> a day.
          </p>
        )}
        {plan.charged_per_hour != null && (
          <p className="detail">
            Busy for an hour at the model's prices, the measured instances are charged{" "}
            {formatDollars(plan.charged_per_hour)}
            {plan.cost_per_hour ? `: ${(plan.charged_per_hour / plan.cost_per_hour).toFixed(2)}× what they cost` : ""}.
          </p>
        )}
        {plan.break_even && (
          <p className="detail">
            Prices that would pay for them at {percent(plan.utilization)} busy, per 1M tokens:{" "}
            <span className="strong">
              {formatPrice(plan.break_even.price_input)} input, {formatPrice(plan.break_even.price_cached)} cached,{" "}
              {formatPrice(plan.break_even.price_output)} output
            </span>
            . At these, what the GPUs serve a day at {percent(plan.utilization)} busy is worth what they cost a day: a
            check for the prices you set by hand, or a start.{" "}
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
  const what = run.instance ? `pod ${run.namespace}/${run.instance}` : `${run.namespace}/${run.deployment}, every decode pod`;
  if (run.phase === "draining") {
    return (
      <div className="banner info">
        Draining {run.cluster_name} for {run.model_name} since {formatTime(run.created_at)}: the entry route's weight steps
        down to 0, then {what} is measured with no user traffic. The site comes back with the usual steps afterwards.
      </div>
    );
  }
  return (
    <div className={run.mode === "live" ? "banner warn" : "banner info"}>
      Measuring {what} on {run.cluster_name}
      {run.mode === "live" ? ", with its users," : ", drained,"} since {formatTime(run.created_at)}.{" "}
      {last
        ? `${run.steps.length} ${run.steps.length === 1 ? "step" : "steps"} done, the last at ${last.concurrency} in flight.`
        : run.dataset
          ? `${run.dataset.sessions} sessions made up, prompts up to ${run.dataset.prompt_max.toLocaleString("en-US")} tokens; the first step is running.`
          : "Making up the sessions."}{" "}
      The load doubles until a step misses a target, then the gap is halved.
    </div>
  );
}

function RunDetail({ run }: { run: CapacityRun }) {
  return (
    <>
      <p className="detail">
        {run.instance ? `Pod ${run.namespace}/${run.instance}` : `${run.namespace}/${run.deployment} through its scheduler, over ${run.instances_during} decode pods`}
        {run.profile_version != null && `, profile version ${run.profile_version}`}
        {run.mode && `, ${run.mode}`}.{" "}
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
                  {v.foreign_share != null && v.foreign_share > 0.01 && (
                    <span
                      className={v.met ? "tag" : "tag warn"}
                      title={
                        "The part of the prompt tokens the instance counted that the measurement did not send" +
                        (v.beyond_sent != null ? `; counted beyond what was sent: ${percent(v.beyond_sent)}` : "")
                      }
                    >
                      {percent(v.foreign_share)} other traffic
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

/** The longest a measurement may take, as the server works it out: every step, its grace and start, the wait for an idle instance and the start of the Job. */
function longest(p: BenchmarkProfile): string {
  let steps = 1;
  for (let c = Math.max(p.sweep.first_concurrency, 1); c < p.sweep.max_concurrency; c *= 2) steps++;
  steps += p.sweep.bisect_steps;
  const hours = (steps * (p.sweep.step_seconds + 300 + 60) + 600 + 900) / 3600;
  return hours < 1 ? `${Math.round(hours * 60)} minutes` : `${hours.toFixed(1)} hours`;
}

function MeasureForm(props: {
  m: CapacityModel;
  llmi: CapacityLLMI;
  profile: BenchmarkProfile;
  onClose: () => void;
  onStarted: () => Promise<void>;
}) {
  const { m, llmi: l, profile: p } = props;
  const pd = l.topology === "prefill-decode";
  // A drained measurement needs another site to serve the model meanwhile;
  // a site that is drained already needs none.
  const canDrain = l.drained || m.sites > 1;
  const [mode, setMode] = useState<"drained" | "live">(canDrain ? "drained" : "live");
  const pods = useLoad(() => (pd ? Promise.resolve([]) : api.deploymentPods(l.cluster_id, l.namespace, l.name)));
  const [pod, setPod] = useState("");
  const [confirmName, setConfirmName] = useState("");

  const start = async () => {
    if (!pd && !pod) throw new Error("Choose the pod to measure.");
    if (mode === "live" && confirmName !== m.model_name) throw new Error(`Type ${m.model_name} to confirm.`);
    await api.startCapacityRun(m.model_id, {
      cluster_id: l.cluster_id,
      namespace: l.namespace,
      deployment: l.name,
      pod: pd ? "" : pod,
      mode,
      confirm: mode === "live" ? confirmName : undefined,
    });
    await props.onStarted();
  };

  return (
    <FormModal title={`Measure ${m.model_name} on ${l.cluster_name}: ${l.namespace}/${l.name}`} submitLabel="Start" onClose={props.onClose} onSubmit={start} wide>
      <p className="hint">
        AIPerf runs in a Job on the cluster with the benchmark profile {p.name}, version {p.version}: made-up coding-agent
        sessions sent {pd ? "through the deployment's scheduler on the gateway's peer listener" : "straight to one pod"}, the
        load doubling from {p.sweep.first_concurrency} in flight until a step misses p95 {seconds(p.slo.ttft_p95_ms)} to
        the first token or {seconds(p.slo.itl_p95_ms)} between tokens, then the gap halved {p.sweep.bisect_steps === 1 ? "once" : `${p.sweep.bisect_steps} times`}.
        It takes up to {longest(p)}. Nothing of a prompt reaches this server, and no tenant is charged.
      </p>
      <div className="grid-2">
        <Field label="Mode">
          <select value={mode} onChange={(e) => setMode(e.target.value as "drained" | "live")}>
            <option value="drained" disabled={!canDrain}>
              Drained: no user traffic{canDrain ? "" : " (no other site serves the model)"}
            </option>
            <option value="live">Live: while it serves its users</option>
          </select>
        </Field>
        {!pd && (
          <Field label="Pod" hint={pods.error || "Its instance is what is measured."}>
            <select required value={pod} onChange={(e) => setPod(e.target.value)}>
              <option value="">
                {pods.error ? "The pods could not be read" : !pods.data ? "Reading the pods…" : pods.data.length ? "Choose a ready pod" : "No ready pod"}
              </option>
              {pods.data?.map((x) => (
                <option key={x} value={x}>
                  {x}
                </option>
              ))}
            </select>
          </Field>
        )}
      </div>
      {mode === "drained" ? (
        <p className="detail">
          The entry route's weight for {l.cluster_name} steps down to 0, so its users go to the other sites; the measurement
          starts when the site has no request in flight, and the weight comes back afterwards with the usual steps. A step
          in which the instance served other traffic, such as a client of the site's own route, fails.
        </p>
      ) : (
        <>
          <div className="banner warn">
            This LLMI serves users now. The benchmark adds load until it misses the SLO. Users of {m.model_name} on{" "}
            {l.cluster_name} will get slower answers or errors for up to {longest(p)}, and their traffic makes the result
            lower than what the LLMI could serve alone.
          </div>
          <Field label={`Type ${m.model_name} to confirm`}>
            <input value={confirmName} onChange={(e) => setConfirmName(e.target.value)} autoComplete="off" />
          </Field>
        </>
      )}
    </FormModal>
  );
}

/**
 * The planner's first line: what the fleet serves a day at the models'
 * prices, what can be given, what is given and what is left, then which model
 * runs out first at its busiest hour.
 */
function FleetPanel({ b, onSplit }: { b: Budgets; onSplit: () => void }) {
  const f = b.fleet;
  if (f.value_day === 0 && f.given_day === 0) return null;
  const byID = new Map(b.models.map((m) => [m.model_id, m]));
  const peaks = b.by_peak.map((id) => byID.get(id)).filter((m) => m && m.peak_hour_share != null);
  return (
    <div className="banner info">
      <p>
        The fleet serves <span className="strong">{formatDollars(f.value_day)}</span> a day at the models' prices.{" "}
        <span className="strong">{formatDollars(f.can_be_given_day)}</span> can be given, up to{" "}
        {formatDollars(f.ceiling_day)} with overcommit. {formatDollars(f.given_day)} is given.{" "}
        <LeftToGive left={f.left_day} format={formatDollars} />.{" "}
        <button className="link" onClick={onSplit} disabled={f.left_day <= 0}>
          Split what is left
        </button>
      </p>
      {f.suggested_overcommit != null && (
        <p className="detail">
          The busiest of the last 30 days would have held with an overcommit of up to {f.suggested_overcommit.toFixed(2)}×
          over what is given; change it per model below.
        </p>
      )}
      {peaks.length > 0 && (
        <p className="detail">
          At their busiest hour:{" "}
          {peaks.map((m, i) => (
            <Fragment key={m!.model_id}>
              {i > 0 && " · "}
              {m!.model_name} {percent(m!.peak_hour_share!)}
              {m!.saturated && <span className="tag warn">saturated</span>}
            </Fragment>
          ))}
          . The first is where the next GPU helps most.
        </p>
      )}
    </div>
  );
}

/** Proposes equal daily budgets on one model out of what is left, and applies them once confirmed. */
function SplitForm(props: { b: Budgets; onClose: () => void; onApplied: () => Promise<void> }) {
  const priced = props.b.models.filter((m) => m.unit === "credits" && m.measured);
  const tenants = useLoad(api.tenants);
  const [modelID, setModelID] = useState(priced[0]?.model_id ?? "");
  const model = priced.find((m) => m.model_id === modelID);
  const left = Math.min(model?.left_day ?? 0, props.b.fleet.left_day);
  const [amount, setAmount] = useState("");
  const [chosen, setChosen] = useState<string[]>([]);
  const [proposals, setProposals] = useState<{ tenant_id: string; amount: number }[] | null>(null);
  const action = useAction();
  const slug = (id: string) => tenants.data?.find((t) => t.id === id)?.slug ?? id;

  const propose = () =>
    action.run(async () => {
      const r = await api.split({ amount: Math.round(Number(amount) * 100000), tenant_ids: chosen, model_id: modelID });
      setProposals(r.proposals);
    });
  const apply = async () => {
    if (!proposals || !model) throw new Error("Propose a split first.");
    if (!confirm(`Set the daily budget on ${model.model_name} of ${proposals.length} tenants to ${formatDollars(proposals[0].amount)}? It replaces the budget each has on it.`))
      return;
    for (const p of proposals) await api.setQuota(p.tenant_id, modelID, p.amount, "1d", false);
    await props.onApplied();
  };
  return (
    <FormModal title="Split what is left" submitLabel="Apply" onClose={props.onClose} onSubmit={apply} wide>
      <ErrorBanner message={action.error || tenants.error} />
      <p className="hint">
        Equal daily budgets on one model, out of what is left to give on it and in the fleet. Nothing changes until you
        apply it.
      </p>
      <div className="grid-2">
        <Field label="Model">
          <select value={modelID} onChange={(e) => (setModelID(e.target.value), setProposals(null))}>
            {priced.map((m) => (
              <option key={m.model_id} value={m.model_id}>
                {m.model_name}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Dollars a day, in all" hint={`Up to ${formatDollars(Math.max(left, 0))}.`}>
          <input type="number" min={0.01} step="any" value={amount} onChange={(e) => (setAmount(e.target.value), setProposals(null))} />
        </Field>
      </div>
      <h3>Tenants</h3>
      <div className="grid-3">
        {tenants.data?.map((t) => (
          <label key={t.id} className="check">
            <input
              type="checkbox"
              checked={chosen.includes(t.id)}
              onChange={(e) => (setChosen(e.target.checked ? [...chosen, t.id] : chosen.filter((x) => x !== t.id)), setProposals(null))}
            />
            <span>{t.slug}</span>
          </label>
        ))}
      </div>
      <button type="button" disabled={action.busy || !chosen.length || !(Number(amount) > 0)} onClick={propose}>
        Propose
      </button>
      {proposals && (
        <table className="plain">
          <thead>
            <tr>
              <th>Tenant</th>
              <th className="text-right">A day on {model?.model_name}</th>
            </tr>
          </thead>
          <tbody>
            {proposals.map((p) => (
              <tr key={p.tenant_id}>
                <td>{slug(p.tenant_id)}</td>
                <td className="text-right tabular-nums">{formatDollars(p.amount)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </FormModal>
  );
}

/** The active benchmark profile, in one line. */
function ProfileLine({ profile: p, onEdit }: { profile: BenchmarkProfile; onEdit: () => void }) {
  return (
    <p className="detail">
      <span className="strong">
        Benchmark profile {p.name}, version {p.version}:
      </span>{" "}
      prompts up to {short(p.workload.max_prompt_tokens)} tokens with a {short(p.workload.shared_prefix_tokens)} shared prefix,{" "}
      {p.workload.pause_ms ? `pauses up to ${seconds(p.workload.pause_ms)}` : "no pause between turns"}; p95{" "}
      {seconds(p.slo.ttft_p95_ms)} to the first token, {seconds(p.slo.itl_p95_ms)} between tokens, at most{" "}
      {percent(p.slo.max_error_share)} errors; steps of up to {Math.round(p.sweep.step_seconds / 60)} minutes.{" "}
      <button className="link" onClick={onEdit}>
        change
      </button>
    </p>
  );
}

/** What the hub measures by itself, and when. */
function CampaignPanel({ campaign: c, models }: { campaign: Campaign; models: CapacityModel[] }) {
  const name = (id: string) => models.find((m) => m.model_id === id)?.model_name ?? id;
  return (
    <div className="card">
      <div className="dash-head">
        <div>
          <h2>Measuring by itself</h2>
          <p className="detail">
            {c.window
              ? c.open
                ? `Inside its window now (${c.window}): one drained measurement per site and per model at a time.`
                : `In its window, ${c.window}; next from ${c.next ? formatTime(c.next) : "–"}.`
              : "Never: set capacity.window in the chart, such as \"Fri 22:00 - Sat 06:00 Asia/Jerusalem\"."}{" "}
            What is not measured comes first, then what is stale.
          </p>
        </div>
      </div>
      {c.queue.length === 0 ? (
        <p className="detail">Every LLMInferenceService with a ready instance has a fresh result.</p>
      ) : (
        <div className="table-wrap">
          <table className="plain">
            <thead>
              <tr>
                <th>Model</th>
                <th>LLMInferenceService</th>
                <th>Why</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {c.queue.map((q) => (
                <tr key={`${q.cluster_id}/${q.namespace}/${q.name}`}>
                  <td>{name(q.model_id)}</td>
                  <td>
                    {q.cluster_name} · {q.namespace}/{q.name}
                  </td>
                  <td className="detail">{staleText[q.reason] ?? q.reason}</td>
                  <td>
                    <span className={q.state === "next" || q.state === "running" ? "tag" : "tag warn"}>{q.state}</span>
                    {q.cover != null && q.cover < 1 && (
                      <div className="detail">
                        The other sites take {percent(q.cover)} of its busiest hour in the window.
                      </div>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

/** Edits the benchmark profile: saving makes a new version, which every measurement uses from then on. */
function ProfileForm(props: { profile: BenchmarkProfile; onClose: () => void; onSaved: () => Promise<void> }) {
  const p = props.profile;
  const versions = useLoad(api.benchmarkProfiles);
  const [f, setF] = useState({
    name: p.name,
    max_prompt_tokens: String(p.workload.max_prompt_tokens),
    max_output_tokens: String(p.workload.max_output_tokens),
    sessions: String(p.workload.sessions),
    shared_prefix_tokens: String(p.workload.shared_prefix_tokens),
    pause_ms: String(p.workload.pause_ms),
    ttft_p95_ms: String(p.slo.ttft_p95_ms),
    itl_p95_ms: String(p.slo.itl_p95_ms),
    max_error_pct: String(p.slo.max_error_share * 100),
    first_concurrency: String(p.sweep.first_concurrency),
    bisect_steps: String(p.sweep.bisect_steps),
    step_seconds: String(p.sweep.step_seconds),
    min_requests: String(p.sweep.min_requests),
    max_concurrency: String(p.sweep.max_concurrency),
    note: "",
  });
  const field = (key: keyof typeof f, label: string, hint?: string) => (
    <Field label={label} hint={hint}>
      <input required={key !== "note"} value={f[key]} onChange={(e) => setF({ ...f, [key]: e.target.value })} />
    </Field>
  );
  const save = async () => {
    const n = (k: keyof typeof f) => Number(f[k]);
    await api.setBenchmarkProfile({
      name: f.name.trim(),
      workload: {
        max_prompt_tokens: n("max_prompt_tokens"),
        max_output_tokens: n("max_output_tokens"),
        sessions: n("sessions"),
        shared_prefix_tokens: n("shared_prefix_tokens"),
        pause_ms: n("pause_ms"),
      },
      slo: { ttft_p95_ms: n("ttft_p95_ms"), itl_p95_ms: n("itl_p95_ms"), max_error_share: n("max_error_pct") / 100 },
      sweep: {
        first_concurrency: n("first_concurrency"),
        bisect_steps: n("bisect_steps"),
        step_seconds: n("step_seconds"),
        min_requests: n("min_requests"),
        max_concurrency: n("max_concurrency"),
      },
      note: f.note.trim(),
    });
    await props.onSaved();
  };
  return (
    <FormModal title={`Benchmark profile, version ${p.version}`} submitLabel="Save as a new version" onClose={props.onClose} onSubmit={save} wide>
      <ErrorBanner message={versions.error} />
      <p className="hint">
        Every LLMInferenceService is measured with this one profile, so their results compare. Saving makes version{" "}
        {p.version + 1}: the results of version {p.version} keep counting, marked stale, until the LLMIs are measured again,
        and a model never adds figures of two versions. A deployment that takes shorter requests gets every part of a
        session scaled down by the same factor.
      </p>
      {field("name", "Name")}
      <h3>Workload</h3>
      <div className="grid-3">
        {field("max_prompt_tokens", "Longest prompt, tokens", "Sessions grow to this; about 60% of it on average.")}
        {field("max_output_tokens", "Longest answer, tokens")}
        {field("sessions", "Sessions", "Made up once, replayed at every step.")}
        {field("shared_prefix_tokens", "Shared prefix, tokens", "The system prompt every session shares.")}
        {field("pause_ms", "Longest pause between turns, ms", "0: turns follow each other.")}
      </div>
      <h3>What you promise</h3>
      <div className="grid-3">
        {field("ttft_p95_ms", "Time to first token p95, ms")}
        {field("itl_p95_ms", "Time between tokens p95, ms", "50 ms is 20 tokens a second for each user.")}
        {field("max_error_pct", "Failed requests, at most %")}
      </div>
      <h3>Sweep</h3>
      <div className="grid-3">
        {field("first_concurrency", "First step, requests in flight")}
        {field("max_concurrency", "Most in flight", "A model that meets the targets there serves at least that.")}
        {field("bisect_steps", "Halvings of the gap")}
        {field("step_seconds", "Longest a step may take, s")}
        {field("min_requests", "Requests per step, at least")}
      </div>
      {field("note", "Note", "Optional. Why it changed, for the next person.")}
      {versions.data && versions.data.length > 1 && (
        <p className="detail">
          Earlier versions:{" "}
          {versions.data
            .filter((v) => !v.active)
            .map((v) => `${v.version} (${formatTime(v.created_at)}${v.note ? `: ${v.note}` : ""})`)
            .join(", ")}
          .
        </p>
      )}
    </FormModal>
  );
}

/**
 * What each GPU costs an hour. Optional: only this page uses it, for what the
 * measured instances cost and the prices that would pay for them. A model's
 * prices are its own, the same on every site and GPU.
 */
function GPUCosts(props: { gpus: CapacityGPU[]; onClose: () => void; onChanged: () => Promise<void> }) {
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
    <Modal title="GPU costs" onClose={props.onClose} wide>
      <ErrorBanner message={action.error} />
      <p className="hint">
        Optional. What one GPU costs an hour, all in: the hardware over its life, power, cooling, rack, network and the
        people who run it. An instance costs its GPUs times this. The GPUs are read from the node label{" "}
        <span className="mono">nvidia.com/gpu.product</span> of the model's pods.
      </p>
      <p className="hint">
        Only the Capacity page uses these: for what each model's instances cost, and prices that would pay for them, to
        check the prices you set against. Nothing a tenant pays depends on them. A model has one price, the same
        whichever site or GPU serves the request, set under Prices on the Models page.
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
                    aria-label={`Cost of ${r.gpu}`}
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
              <span className="strong">{formatTime(run.created_at)}</span> · {run.cluster_name} · {run.namespace}/{run.deployment}
              {run.instance && `, pod ${run.instance}`} <span className={run.status === "succeeded" ? "tag" : "tag warn"}>{run.status}</span>
              {run.spec_hash ? (
                <span className="tag">
                  profile {run.profile_version} · {run.mode}
                </span>
              ) : (
                <span className="tag" title="Measured by shape before 0.16: kept as history, counted for nothing">
                  shape, history
                </span>
              )}
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
