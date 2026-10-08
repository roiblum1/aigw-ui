import { useEffect, useState } from "react";
import { api, type Cluster, type SelfTestRun, type StepStatus } from "../api";
import { ErrorBanner, Field, Modal, formatTime, useAction, useLoad } from "../components";

const REFRESH_MS = 2000;

const stepBadge: Record<StepStatus, { cls: string; label: string }> = {
  pending: { cls: "idle", label: "Waiting" },
  running: { cls: "pending", label: "Running" },
  passed: { cls: "synced", label: "Passed" },
  failed: { cls: "error", label: "Failed" },
  warning: { cls: "pending", label: "Unclear" },
  skipped: { cls: "idle", label: "Skipped" },
};

/** Runs the self-test of one cluster and shows its steps as they finish. */
export default function SelfTest({ cluster, onClose }: { cluster: Cluster; onClose: () => void }) {
  const { data: models } = useLoad(api.models);
  const [run, setRun] = useState<SelfTestRun | null>(null);
  const [modelId, setModelId] = useState("");
  const action = useAction();

  // Models this cluster serves that a quota can be attached to.
  const usable = (models ?? []).filter((m) =>
    m.endpoints.some((e) => e.cluster_id === cluster.id && (e.source === "manual" || (e.backends?.length ?? 0) > 0)),
  );

  useEffect(() => {
    let stop = false;
    const load = () =>
      api.selfTest(cluster.id).then(
        (r) => !stop && r && setRun(r),
        () => {},
      );
    load();
    const timer = setInterval(() => run?.status === "running" && load(), REFRESH_MS);
    return () => {
      stop = true;
      clearInterval(timer);
    };
  }, [cluster.id, run?.status]);

  const running = run?.status === "running";
  const start = () => action.run(async () => setRun(await api.startSelfTest(cluster.id, modelId)));

  return (
    <Modal title={`Self-test: ${cluster.name}`} onClose={onClose} wide>
      <p className="hint">
        Sends a few real one-token requests through this cluster's gateway to check that a key is accepted, that usage
        is counted where the Usage page reads it, that a quota refuses and that a reset frees. It adds a temporary
        tenant with a quota on the chosen model and removes it again; this takes about a minute.
      </p>
      <ErrorBanner message={action.error} />
      <div className="selftest-start">
        <Field label="Model to test with">
          <select value={modelId} disabled={running} onChange={(e) => setModelId(e.target.value)}>
            <option value="">First suitable model</option>
            {usable.map((m) => (
              <option key={m.id} value={m.id}>
                {m.name}
              </option>
            ))}
          </select>
        </Field>
        <button className="primary" disabled={running || action.busy || (models !== null && usable.length === 0)} onClick={start}>
          {running ? "Running…" : run ? "Run again" : "Run self-test"}
        </button>
      </div>
      {models !== null && usable.length === 0 && (
        <p className="hint">This cluster serves no model a quota can be attached to, so there is nothing to test.</p>
      )}

      {run && (
        <>
          <div className="endpoint-line">
            <span className={`badge ${run.status === "passed" ? "synced" : run.status === "failed" ? "error" : "pending"}`}>
              {run.status === "passed" ? "Passed" : run.status === "failed" ? "Failed" : "Running"}
            </span>
            <span className="detail">
              With {run.model_name}, started {formatTime(run.started_at)}
            </span>
          </div>
          <ol className="selftest-steps">
            {run.steps.map((s) => (
              <li key={s.id}>
                <span className={`badge ${stepBadge[s.status].cls}`}>{stepBadge[s.status].label}</span>
                <div>
                  <div className="strong">{s.title}</div>
                  {s.detail && <div className="detail">{s.detail}</div>}
                </div>
              </li>
            ))}
          </ol>
        </>
      )}
    </Modal>
  );
}
