import { useEffect } from "react";
import { ListChecks } from "lucide-react";
import { api, type Task, type TaskChange, type TaskResult } from "./api";
import { Empty, ErrorBanner, PageHeader, formatTime, useLoad } from "./components";

const REFRESH_MS = 3000;

const statusLabel: Record<Task["status"], string> = {
  pending: "In progress",
  succeeded: "Succeeded",
  failed: "Failed",
};

function Status({ status, warn }: { status: Task["status"]; warn?: boolean }) {
  const cls = status === "succeeded" ? (warn ? "pending" : "synced") : status === "failed" ? "error" : "pending";
  return <span className={`badge ${cls}`}>{warn && status === "succeeded" ? "Applied, not accepted" : statusLabel[status]}</span>;
}

function ObjectLine({ c }: { c: TaskChange }) {
  return (
    <li>
      {c.action && <span className={`tag ${c.action === "deleted" ? "manual" : ""}`}>{c.action}</span>}{" "}
      <span className="mono">
        {c.kind} {c.namespace}/{c.name}
      </span>
      {c.gateway && (
        <span className={c.gateway === "NotAccepted" ? "gateway bad" : "gateway"} title={c.gateway_message}>
          {" "}
          gateway: {c.gateway}
          {c.gateway === "NotAccepted" && c.gateway_message ? `: ${c.gateway_message}` : ""}
        </span>
      )}
    </li>
  );
}

function Result({ r }: { r: TaskResult }) {
  return (
    <div className="task-result">
      <div className="endpoint-line">
        <span className="strong">{r.cluster_name}</span>
        <Status status={r.status} warn={r.rejected.length > 0} />
        <span className="detail">{r.status === "pending" ? "Waiting for the next sync." : r.message}</span>
      </div>
      {r.changes.length > 0 && (
        <ul className="task-objects">
          {r.changes.map((c) => (
            <ObjectLine key={c.kind + c.namespace + c.name} c={c} />
          ))}
        </ul>
      )}
      {r.rejected.length > 0 && (
        <>
          <div className="detail">The gateway does not accept:</div>
          <ul className="task-objects">
            {r.rejected.map((c) => (
              <ObjectLine key={c.kind + c.namespace + c.name} c={c} />
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

export default function Activity() {
  const { data: tasks, error, reload } = useLoad(() => api.tasks());
  useEffect(() => {
    const timer = setInterval(() => {
      if (!document.hidden) reload();
    }, REFRESH_MS);
    return () => clearInterval(timer);
  }, [reload]);

  return (
    <>
      <PageHeader
        title="Activity"
        subtitle="Every change, and what each cluster did with it. An object is listed once the cluster confirmed it."
      />
      <ErrorBanner message={error} />
      {tasks && tasks.length === 0 && <Empty icon={ListChecks}>Nothing has been changed yet.</Empty>}
      {tasks && tasks.length > 0 && (
        <div className="task-list">
          {tasks.map((t) => {
            const warn = t.results.some((r) => r.rejected.length > 0);
            return (
              <details key={t.id} className="task" open={t.status !== "succeeded" || warn}>
                <summary>
                  <Status status={t.status} warn={warn} />
                  <span className="strong">{t.summary}</span>
                  <span className="detail">{formatTime(t.created_at)}</span>
                </summary>
                {t.message && <p className="detail">{t.message}</p>}
                {t.results.map((r) => (
                  <Result key={r.cluster_name} r={r} />
                ))}
              </details>
            );
          })}
        </div>
      )}
    </>
  );
}
