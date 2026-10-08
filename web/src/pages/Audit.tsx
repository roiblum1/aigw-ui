import { useEffect, useState } from "react";
import { ScrollText } from "lucide-react";
import { api, type AuditEntry } from "../api";
import { Empty, ErrorBanner, formatTime, useAction } from "../components";

// The server returns this many entries per request.
const PAGE = 100;

function Outcome({ status }: { status: number }) {
  if (status < 400) return <span className="badge synced">Done</span>;
  if (status < 500) return <span className="badge pending" title={`HTTP ${status}`}>Refused ({status})</span>;
  return <span className="badge error" title={`HTTP ${status}`}>Error ({status})</span>;
}

/** The address the request came from: the last hop the router added, or the peer. */
function source(e: AuditEntry): string {
  return e.forwarded_for.split(",").pop()?.trim() || e.remote_addr;
}

export default function Audit() {
  const [entries, setEntries] = useState<AuditEntry[] | null>(null);
  const [more, setMore] = useState(false);
  const action = useAction();
  const { run } = action;

  useEffect(() => {
    run(async () => {
      const first = await api.audit();
      setEntries(first);
      setMore(first.length === PAGE);
    });
  }, [run]);

  const loadMore = () =>
    run(async () => {
      const older = await api.audit(entries![entries!.length - 1].id);
      setEntries([...entries!, ...older]);
      setMore(older.length === PAGE);
    });

  return (
    <>
      <ErrorBanner message={action.error} />
      {entries && entries.length === 0 && <Empty icon={ScrollText}>No change has been requested yet.</Empty>}
      {entries && entries.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>When</th>
                <th>Who</th>
                <th>What</th>
                <th>Result</th>
                <th>From</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => (
                <tr key={e.id}>
                  <td>{formatTime(e.at)}</td>
                  <td>
                    <span className="strong">{e.on_behalf_of || e.actor}</span>
                    {e.on_behalf_of && (
                      <div className="detail" title="The name is what the caller gave; it is not verified.">
                        using the {e.actor}
                      </div>
                    )}
                  </td>
                  <td>
                    {e.summary || <span className="mono">{e.method} {e.path}</span>}
                    {e.summary && (
                      <div className="detail mono clamp" title={`${e.method} ${e.path}`}>
                        {e.method} {e.path}
                      </div>
                    )}
                  </td>
                  <td>
                    <Outcome status={e.status} />
                  </td>
                  <td className="mono" title={e.user_agent}>
                    {source(e)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {more && (
        <div className="load-more">
          <button disabled={action.busy} onClick={loadMore}>
            Load older entries
          </button>
        </div>
      )}
    </>
  );
}
