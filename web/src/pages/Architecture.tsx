import { useEffect, useState } from "react";
import { api } from "../api";
import { ErrorBanner, PageHeader, useLoad } from "../components";

/** Follows the app's light or dark mode, which is a class on the root element. */
function useIsDark() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));
  useEffect(() => {
    const observer = new MutationObserver(() => setDark(document.documentElement.classList.contains("dark")));
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    return () => observer.disconnect();
  }, []);
  return dark;
}

/** What this tool does today, next to what the design asks of the hub. */
const status: { part: string; today: string; built: "yes" | "partly" | "no" }[] = [
  { part: "Tenants, API keys, model catalog, quotas", today: "Built.", built: "yes" },
  {
    part: "The path of a request",
    today: "As designed: client, gateway, model. No request passes through this tool. It writes objects to the clusters and reads the counters.",
    built: "yes",
  },
  {
    part: "Prices and budgets in money",
    today: "Built: prices per model for input, cached input and output. The gateway computes each request's cost, and quotas and usage are in dollars, per minute, hour or day. Monthly budgets are not built: the gateway's longest window is a day.",
    built: "partly",
  },
  { part: "Live usage", today: "Built, read from the quota counters in Redis.", built: "yes" },
  { part: "Config to every cluster", today: "Built. This tool applies it itself with each cluster's kubeconfig, not through ACM and Argo CD.", built: "partly" },
  { part: "API keys in Postgres", today: "Stored encrypted, not hashed: every sync has to write them to the clusters.", built: "partly" },
  {
    part: "Site weights",
    today: "Built: ready instances of the model times the declared capacity of one instance. The hub renders them into each model's entry route. See the Models page.",
    built: "partly",
  },
  {
    part: "Priority class",
    today: "Built per model, not per key: a tenant whose budget is spent is marked as best-effort, or tenants share a pool. The class changes what a site does only when its scheduler acts on priorities. See the Models page.",
    built: "partly",
  },
  {
    part: "Usage collector and past usage",
    today: "Built differently: no collector. The hub reads the quota counters once a minute and keeps what was used per tenant, model and hour. See the Overview. Token counts by kind, such as cached tokens, are not kept.",
    built: "partly",
  },
  {
    part: "Choosing a site",
    today: "Built: a weighted hash of the session header in each entry gateway, from the entry route this tool renders. There is no separate service that picks a site, and a site is not chosen by what it has cached.",
    built: "yes",
  },
  { part: "Peer listener, EPP settings", today: "Not part of this tool. They belong to the cluster charts.", built: "no" },
];

const badge = { yes: ["synced", "Built"], partly: ["pending", "Differs"], no: ["idle", "Not built"] } as const;

export default function Architecture() {
  const { data: page, error } = useLoad(api.platformArchitecture);
  const dark = useIsDark();
  // The frame has no script rights, so the theme is set in its markup.
  const themed = page?.replace("<html>", `<html data-theme="${dark ? "dark" : "light"}">`);

  return (
    <>
      <PageHeader
        title="Architecture"
        subtitle="The design of the whole platform: the hub, the LLM clusters and how a request finds its site."
      />
      <ErrorBanner message={error} />
      <details className="task arch-status">
        <summary>
          <span className="badge pending">Target design</span>
          <span className="strong">This page describes where the platform is going. Open to see what this tool does today.</span>
        </summary>
        <table className="plain">
          <tbody>
            {status.map((s) => (
              <tr key={s.part}>
                <td className="strong">{s.part}</td>
                <td>
                  <span className={`badge ${badge[s.built][0]}`}>{badge[s.built][1]}</span>
                </td>
                <td>{s.today}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
      {themed && (
        <iframe
          className="arch-frame"
          title="Multi-site LLM platform architecture"
          srcDoc={themed}
          // No scripts and no access to this app; links may open in a new tab.
          sandbox="allow-popups allow-popups-to-escape-sandbox"
        />
      )}
    </>
  );
}
