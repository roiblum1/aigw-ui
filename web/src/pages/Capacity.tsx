import { useState } from "react";
import { Gauge } from "lucide-react";
import { api, type CapacityModel, type CapacityPlan, type CapacityRun, type CapacitySettings, type CapacityStep } from "../api";
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

/** Tokens a second, short: 12.3k. */
const rate = (n: number) => (n >= 1000 ? `${(n / 1000).toFixed(1)}k` : Math.round(n).toLocaleString("en-US"));
const percent = (n: number) => `${Math.round(n * 100)}%`;
/** A sentence from the server that starts in the middle: "at 32 in flight: …". */
const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
const seconds = (ms: number) => (ms >= 1000 ? `${(ms / 1000).toFixed(1)} s` : `${Math.round(ms)} ms`);

/** An amount per day: dollars, or tokens written short. */
function perDay(n: number, unit: Unit): string {
  return unit === "credits" ? formatDollars(n) : `${short(n)} tokens`;
}

function cachedShare(s: CapacityStep): number | null {
  if (s.server_prompt_tokens == null || s.server_cached_tokens == null || s.server_prompt_tokens <= 0) return null;
  return Math.min(Math.max(s.server_cached_tokens / s.server_prompt_tokens, 0), 1);
}

const perSecond = (n: number, s: CapacityStep) => (s.duration_s > 0 ? n / s.duration_s : 0);

function meets(s: CapacityStep, set: CapacitySettings): boolean {
  const total = s.requests + s.errors;
  return !s.error && s.requests > 0 && s.errors <= 0.01 * total && s.ttft_p95_ms <= set.ttft_p95_ms && s.itl_p95_ms <= set.itl_p95_ms;
}

/** The longest a measurement may take, as the server bounds it. */
function longest(set: CapacitySettings): string {
  const s = set.concurrency.length * (set.step_seconds + 300 + 60) + 900;
  const h = Math.floor(s / 3600);
  const m = Math.round((s % 3600) / 60);
  return h > 0 ? `${h} h ${m} min` : `${m} min`;
}

/**
 * What one instance of each model serves at the latency promised to the
 * tenants, measured with AIPerf in a Job on a cluster, and what follows from
 * it: what the model can be given a day, what it has been given, and the
 * price that pays for its instances.
 */
export default function Capacity() {
  const { data, error, reload } = useLoad(api.capacity);
  const running = data?.models.some((m) => m.running) ?? false;
  usePolling(reload, running ? 15_000 : 60_000);
  const [measuring, setMeasuring] = useState<CapacityModel | null>(null);
  const [history, setHistory] = useState<CapacityModel | null>(null);
  const action = useAction();

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
        subtitle="What one instance of a model serves at the latency you promise, what the model can therefore be given a day, and how much of that the tenants' budgets already take."
      />
      <ErrorBanner message={error || action.error} />
      {data && !data.image && (
        <div className="banner warn">
          No AIPerf image is set, so nothing can be measured. Set <span className="mono">capacity.image</span> in the
          chart to an image the clusters can pull; see the capacity guide under Docs.
        </div>
      )}
      {data?.models.length === 0 && <Empty icon={Gauge}>No models yet. Add one on the Models page.</Empty>}
      {data?.models.map((m) => (
        <ModelCard
          key={m.model_id}
          m={m}
          canMeasure={!!data.image && !action.busy}
          onMeasure={() => setMeasuring(m)}
          onHistory={() => setHistory(m)}
          onStop={stop}
        />
      ))}
      {measuring && <MeasureForm m={measuring} onClose={() => setMeasuring(null)} onStarted={reload} />}
      {history && <RunHistory m={history} onClose={() => setHistory(null)} onChanged={reload} />}
    </section>
  );
}

function ModelCard(props: {
  m: CapacityModel;
  canMeasure: boolean;
  onMeasure: () => void;
  onHistory: () => void;
  onStop: (run: CapacityRun) => void;
}) {
  const { m } = props;
  const sites = m.sites.map((s) => `${s.cluster_name} (${s.instances == null ? "ready count unknown" : `${s.instances} ready`})`);
  return (
    <div className="card">
      <div className="dash-head">
        <div>
          <h2>{m.model_name}</h2>
          <p className="detail">{sites.length > 0 ? `Served on ${sites.join(", ")}` : "No site serves this model."}</p>
        </div>
        <div className="row-actions">
          <button onClick={props.onHistory}>Measurements</button>
          {m.running ? (
            <button className="danger" onClick={() => props.onStop(m.running!)}>
              Stop
            </button>
          ) : (
            <button
              className="primary"
              disabled={!props.canMeasure || m.sites.length === 0}
              title="Measure one ready pod of the model with AIPerf. It runs as a Job on the cluster."
              onClick={props.onMeasure}
            >
              Measure
            </button>
          )}
        </div>
      </div>
      {m.running && <Running run={m.running} />}
      {m.plan && m.latest?.result.best ? (
        <Plan m={m} plan={m.plan} run={m.latest} best={m.latest.result.best} />
      ) : (
        !m.running && (
          <p className="detail">
            Not measured yet. A measurement finds the highest load one instance serves within your latency targets.
          </p>
        )
      )}
    </div>
  );
}

function Running({ run }: { run: CapacityRun }) {
  const last = run.steps[run.steps.length - 1];
  return (
    <div className="banner info">
      Measuring {run.namespace}/{run.instance} on {run.cluster_name} since {formatTime(run.created_at)}:{" "}
      {run.steps.length === 0
        ? "making up the sessions and starting the first step."
        : `${run.steps.length} of ${run.settings.concurrency.length} steps done, the last at ${last.concurrency} in flight.`}{" "}
      It stops at the first step that misses a target.
    </div>
  );
}

function Plan(props: { m: CapacityModel; plan: CapacityPlan; run: CapacityRun; best: CapacityStep }) {
  const { plan, run, best } = props;
  const set = run.settings;
  const share = cachedShare(best);
  const full = plan.per_instance_day * plan.instances;
  const show = (n: number) => perDay(n, plan.unit);
  return (
    <>
      <div className="dash-grid">
        <div className="panel">
          <h3>One instance</h3>
          <div className="rate-now">
            {best.concurrency}
            <span className="detail"> requests in flight within the targets</span>
          </div>
          <p className="detail">
            {rate(perSecond(best.input_tokens, best))} prompt tokens a second
            {share != null ? `, ${percent(share)} of them from the cache` : ""}, and{" "}
            {rate(perSecond(best.output_tokens, best))} output tokens a second.
          </p>
          <p className="detail">
            Time to first token p95 {seconds(best.ttft_p95_ms)} (target {seconds(set.ttft_p95_ms)}), between tokens p95{" "}
            {seconds(best.itl_p95_ms)} (target {seconds(set.itl_p95_ms)}). Average prompt {Math.round(best.isl_mean).toLocaleString("en-US")} tokens.
          </p>
          <p className="detail">
            Measured {formatTime(run.created_at)} on {run.cluster_name}, pod {run.instance}. {sentence(run.result.limit)}.
          </p>
        </div>

        <div className="panel">
          <h3>What the model can be given a day</h3>
          {plan.capacity_day > 0 ? (
            <Bar label="Given to tenants" used={plan.allocated_day} limit={plan.capacity_day} format={show} />
          ) : (
            <p className="detail">No instance is ready, so the model can be given nothing now.</p>
          )}
          <p className="detail">
            {plan.instances} {plan.instances === 1 ? "instance" : "instances"} × {show(plan.per_instance_day)} each when busy all
            day × {percent(set.utilization)} busy = <span className="strong">{show(plan.capacity_day)}</span>.
          </p>
          {plan.overcommit != null && plan.overcommit > 1 && (
            <p className="detail warn-text">
              The budgets add up to {plan.overcommit.toFixed(2)}× what the model serves. That holds only while tenants do not
              use them all on the same day.
            </p>
          )}
          {plan.overcommit != null && plan.overcommit <= 1 && (
            <p className="detail">
              The budgets take {percent(plan.overcommit)} of it; {show(plan.capacity_day - plan.allocated_day)} a day is
              left to give.
            </p>
          )}
        </div>

        <div className="panel">
          <h3>Busiest day in 30 days</h3>
          {plan.busiest_share != null && full > 0 ? (
            <>
              <Bar label="Used" used={plan.busiest_day} limit={full} format={show} />
              <p className="detail">Out of what every ready instance serves busy all day.</p>
            </>
          ) : (
            <p className="detail">No usage recorded yet.</p>
          )}
        </div>

        <div className="panel">
          <h3>Cost and price</h3>
          {plan.cost_per_instance_hour == null ? (
            <p className="detail">Give what an instance costs an hour when you measure, to see the price that pays for it.</p>
          ) : (
            <p className="detail">An instance costs {formatDollars(plan.cost_per_instance_hour)} an hour.</p>
          )}
          {plan.charged_per_instance_hour != null && (
            <p className="detail">
              At the model's prices, one busy instance is charged {formatDollars(plan.charged_per_instance_hour)} an hour
              {plan.cost_per_instance_hour
                ? `: ${(plan.charged_per_instance_hour / plan.cost_per_instance_hour).toFixed(2)}× its cost`
                : ""}
              .
            </p>
          )}
          {plan.break_even && (
            <p className="detail">
              Prices that pay for the instances at {percent(set.utilization)} busy, per 1M tokens:{" "}
              <span className="strong">
                {formatPrice(plan.break_even.price_input)} input, {formatPrice(plan.break_even.price_cached)} cached,{" "}
                {formatPrice(plan.break_even.price_output)} output
              </span>
              . Set them on the Models page under Prices.
            </p>
          )}
        </div>
      </div>
      {plan.notes.map((n) => (
        <p key={n} className="detail">
          {n}
        </p>
      ))}
      <h3>Load steps</h3>
      <LoadChart
        steps={run.steps.filter((s) => !s.error)}
        ttftTarget={set.ttft_p95_ms}
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
            <th className="text-right">Prompt tok/s</th>
            <th className="text-right">Output tok/s</th>
            <th className="text-right">From cache</th>
            <th className="text-right">First token p50 / p95</th>
            <th className="text-right">Between tokens p50 / p95</th>
            <th>Targets</th>
          </tr>
        </thead>
        <tbody>
          {run.steps.map((s) => {
            const share = cachedShare(s);
            const ok = meets(s, run.settings);
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
                    <td className="text-right tabular-nums">{rate(perSecond(s.input_tokens, s))}</td>
                    <td className="text-right tabular-nums">{rate(perSecond(s.output_tokens, s))}</td>
                    <td className="text-right tabular-nums">{share == null ? "–" : percent(share)}</td>
                    <td className="text-right tabular-nums">
                      {seconds(s.ttft_p50_ms)} / {seconds(s.ttft_p95_ms)}
                    </td>
                    <td className="text-right tabular-nums">
                      {seconds(s.itl_p50_ms)} / {seconds(s.itl_p95_ms)}
                    </td>
                  </>
                )}
                <td>{ok ? <span className="tag">met</span> : <span className="tag warn">missed</span>}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function MeasureForm(props: { m: CapacityModel; onClose: () => void; onStarted: () => Promise<void> }) {
  const { m } = props;
  const d = m.defaults;
  const first = m.sites.find((s) => (s.instances ?? 0) > 0) ?? m.sites[0];
  const [site, setSite] = useState(first?.cluster_id ?? "");
  const [pod, setPod] = useState("");
  const [steps, setSteps] = useState(d.concurrency.join(", "));
  const [stepSeconds, setStepSeconds] = useState(String(d.step_seconds));
  const [sessions, setSessions] = useState(String(d.sessions));
  const [maxPrompt, setMaxPrompt] = useState(String(d.max_prompt_tokens));
  const [maxOutput, setMaxOutput] = useState(String(d.max_output_tokens));
  const [ttft, setTTFT] = useState(String(d.ttft_p95_ms));
  const [itl, setITL] = useState(String(d.itl_p95_ms));
  const [cost, setCost] = useState(m.latest ? String(m.latest.settings.instance_cost_per_hour_usd || "") : "");
  const [utilization, setUtilization] = useState(String(Math.round(d.utilization * 100)));

  const settings: CapacitySettings = {
    concurrency: steps
      .split(/[\s,]+/)
      .filter(Boolean)
      .map(Number),
    step_seconds: Number(stepSeconds),
    sessions: Number(sessions),
    max_prompt_tokens: Number(maxPrompt),
    max_output_tokens: Number(maxOutput),
    ttft_p95_ms: Number(ttft),
    itl_p95_ms: Number(itl),
    instance_cost_per_hour_usd: Number(cost || 0),
    utilization: Number(utilization) / 100,
  };
  const chosen = m.sites.find((s) => s.cluster_id === site);

  const start = async () => {
    await api.startCapacityRun(m.model_id, { cluster_id: site, pod: pod.trim() || undefined, settings });
    await props.onStarted();
  };

  return (
    <FormModal title={`Measure ${m.model_name}`} submitLabel="Start" onClose={props.onClose} onSubmit={start} wide>
      <p className="hint">
        AIPerf runs in a Job on the cluster and sends made-up coding-agent sessions straight to one pod, past the gateway
        and the scheduler: a shared system prompt and tools, repository files, and a history that grows each turn, as
        Claude Code or Cursor send. Every step keeps a number of requests in flight; the first step that misses a target
        ends the measurement. Nothing of a prompt reaches this server, and no tenant is charged.
      </p>
      <p className="banner warn">
        The pod keeps its own traffic while it is measured, and its users see slower answers. Measure a pod that takes
        no traffic, or a quiet hour.
      </p>
      <div className="grid-2">
        <Field label="Site" hint={chosen?.max_model_len ? `Its deployment takes up to ${chosen.max_model_len.toLocaleString("en-US")} tokens.` : undefined}>
          <select value={site} onChange={(e) => setSite(e.target.value)}>
            {m.sites.map((s) => (
              <option key={s.cluster_id} value={s.cluster_id}>
                {s.cluster_name}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Pod" hint="Optional. Empty takes the first ready pod of the model.">
          <input value={pod} onChange={(e) => setPod(e.target.value)} placeholder="any ready pod" />
        </Field>
      </div>
      <h3>Load</h3>
      <div className="grid-3">
        <Field label="Requests in flight, by step" hint="Ascending, up to 12 steps.">
          <input required value={steps} onChange={(e) => setSteps(e.target.value)} />
        </Field>
        <Field label="Seconds per step" hint="60 to 3600. Long prompts need minutes to settle.">
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
      <h3>Cost</h3>
      <div className="grid-3">
        <Field label="One instance, $ per hour" hint="GPUs, power, rack and people. Optional.">
          <input type="number" min={0} max={100000} step="any" value={cost} onChange={(e) => setCost(e.target.value)} />
        </Field>
        <Field label="Expected busy share, %" hint="Of the day, across the instances.">
          <input required type="number" min={1} max={100} value={utilization} onChange={(e) => setUtilization(e.target.value)} />
        </Field>
      </div>
      <p className="hint">It takes at most {longest(settings)}, and usually less.</p>
    </FormModal>
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
              <span className="strong">{formatTime(run.created_at)}</span> · {run.cluster_name}, pod {run.instance}{" "}
              <span className={run.status === "succeeded" ? "tag" : "tag warn"}>{run.status}</span>
              <p className="detail">
                {run.result.best
                  ? `${run.result.best.concurrency} in flight within the targets. ${sentence(run.result.limit)}.`
                  : run.detail || "No step has finished."}
              </p>
              {run.dataset && (
                <p className="detail">
                  {run.dataset.sessions} sessions, {run.dataset.requests} requests, prompts of{" "}
                  {Math.round(run.dataset.prompt_mean).toLocaleString("en-US")} tokens on average and up to{" "}
                  {run.dataset.prompt_max.toLocaleString("en-US")}.
                </p>
              )}
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
          {open === run.id && <Steps run={run} />}
        </div>
      ))}
    </Modal>
  );
}
