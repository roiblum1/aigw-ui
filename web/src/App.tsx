import { useEffect, useState, type FormEvent } from "react";
import {
  Activity as ActivityIcon,
  Boxes,
  ListChecks,
  Gauge,
  KeyRound,
  LayoutDashboard,
  LogOut,
  Moon,
  Server,
  Sun,
  Users,
  Waypoints,
  type LucideIcon,
} from "lucide-react";
import { api, getToken, setToken } from "./api";
import Clusters from "./Clusters";
import Models from "./Models";
import Tenants from "./Tenants";
import Usage from "./Usage";
import Activity from "./Activity";
import Dashboard from "./Dashboard";
import { ErrorBanner, PageHeader, StatusBadge, UNAUTHORIZED_EVENT, formatTime, useLoad } from "./components";

const pages = [
  { name: "Overview", icon: LayoutDashboard },
  { name: "Clusters", icon: Server },
  { name: "Models", icon: Boxes },
  { name: "Tenants", icon: Users },
  { name: "Usage", icon: ActivityIcon },
  { name: "Activity", icon: ListChecks },
] as const;
type Page = (typeof pages)[number]["name"];

function pageFromHash(): Page {
  const name = decodeURIComponent(location.hash.slice(1));
  return pages.find((p) => p.name.toLowerCase() === name)?.name ?? "Overview";
}

const THEME_KEY = "aigw-ui-theme";

function useDarkMode() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));
  const toggle = () => {
    const next = !dark;
    document.documentElement.classList.toggle("dark", next);
    try {
      localStorage.setItem(THEME_KEY, next ? "dark" : "light");
    } catch {
      // The choice then lasts for this page load only.
    }
    setDark(next);
  };
  return { dark, toggle };
}

function BrandMark({ className = "" }: { className?: string }) {
  return (
    <span
      className={`grid size-8 flex-none place-items-center rounded-lg bg-primary text-primary-foreground ${className}`}
      aria-hidden
    >
      <Waypoints className="size-[18px]" />
    </span>
  );
}

const navItem =
  "flex h-9 items-center justify-start gap-2.5 rounded-lg border-0 bg-transparent px-3 text-[13.5px] font-medium " +
  "text-sidebar-foreground no-underline shadow-none transition-colors hover:bg-white/10 hover:text-white";

export default function App() {
  const [authed, setAuthed] = useState(() => getToken() !== "");
  const [page, setPage] = useState<Page>(pageFromHash);
  const theme = useDarkMode();

  useEffect(() => {
    const onHash = () => setPage(pageFromHash());
    const onUnauthorized = () => {
      setToken("");
      setAuthed(false);
    };
    window.addEventListener("hashchange", onHash);
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => {
      window.removeEventListener("hashchange", onHash);
      window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    };
  }, []);

  if (!authed) return <Login onLogin={() => setAuthed(true)} />;

  return (
    <div className="min-h-screen md:grid md:grid-cols-[236px_minmax(0,1fr)]">
      <nav className="flex items-center gap-1 overflow-x-auto bg-sidebar px-3 py-2.5 md:sticky md:top-0 md:h-screen md:flex-col md:items-stretch md:overflow-visible md:py-5">
        <div className="flex items-center gap-2.5 px-2 md:mb-6">
          <BrandMark />
          <div className="hidden leading-tight md:block">
            <div className="text-sm font-semibold text-white">AI Gateway</div>
            <div className="text-xs text-sidebar-foreground">Control plane</div>
          </div>
        </div>
        {pages.map(({ name, icon: Icon }) => (
          <a
            key={name}
            href={`#${name.toLowerCase()}`}
            aria-label={name}
            aria-current={name === page ? "page" : undefined}
            className={`${navItem} ${name === page ? "bg-white/10 text-white" : ""}`}
          >
            <Icon className="size-4 flex-none" />
            <span className="hidden sm:inline">{name}</span>
          </a>
        ))}
        <div className="ml-auto flex gap-1 md:ml-0 md:mt-auto md:flex-col">
          <button className={navItem} onClick={theme.toggle} aria-label="Toggle dark mode">
            {theme.dark ? <Sun className="size-4" /> : <Moon className="size-4" />}
            <span className="hidden md:inline">{theme.dark ? "Light mode" : "Dark mode"}</span>
          </button>
          <button
            className={navItem}
            aria-label="Sign out"
            onClick={() => {
              setToken("");
              setAuthed(false);
            }}
          >
            <LogOut className="size-4" />
            <span className="hidden md:inline">Sign out</span>
          </button>
        </div>
      </nav>
      <main className="mx-auto w-full max-w-[1240px] px-4 pb-12 pt-6 md:px-9 md:pt-8">
        {page === "Overview" && <Overview />}
        {page === "Clusters" && <Clusters />}
        {page === "Models" && <Models />}
        {page === "Tenants" && <Tenants />}
        {page === "Usage" && <Usage />}
        {page === "Activity" && <Activity />}
      </main>
    </div>
  );
}

function Login({ onLogin }: { onLogin: () => void }) {
  const [token, setValue] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await api.checkToken(token);
      setToken(token);
      onLogin();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid min-h-screen place-items-center bg-sidebar p-4">
      <form
        onSubmit={submit}
        className="w-full max-w-[380px] rounded-2xl border border-border bg-card p-7 shadow-2xl shadow-black/30"
      >
        <div className="mb-6 flex items-center gap-3">
          <BrandMark className="size-10" />
          <div>
            <h1 className="text-lg">AI Gateway Control</h1>
            <p className="text-[13px] text-muted-foreground">Sign in to manage your clusters</p>
          </div>
        </div>
        <ErrorBanner message={error} />
        <label className="field">
          <span>Admin token</span>
          <input type="password" required autoFocus value={token} onChange={(e) => setValue(e.target.value)} />
        </label>
        <button type="submit" className="primary mt-1 w-full" disabled={busy}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}

function Stat(props: { label: string; value: number; icon: LucideIcon; note?: string; alert?: boolean }) {
  const Icon = props.icon;
  return (
    <div className="flex items-center gap-3.5 rounded-xl border border-border bg-card p-4 shadow-(--shadow)">
      <span className="grid size-10 flex-none place-items-center rounded-lg bg-primary-soft text-primary">
        <Icon className="size-5" />
      </span>
      <div className="min-w-0">
        <div className="text-2xl font-semibold leading-tight tabular-nums">{props.value}</div>
        <div className="text-[13px] text-muted-foreground">{props.label}</div>
        {props.note && (
          <div className={`text-xs font-medium ${props.alert ? "text-danger" : "text-success"}`}>{props.note}</div>
        )}
      </div>
    </div>
  );
}

function Overview() {
  const { data, error } = useLoad(async () => ({ overview: await api.overview(), clusters: await api.clusters() }));
  const o = data?.overview;
  return (
    <>
      <PageHeader title="Overview" subtitle="State of the gateway configuration across all sites." />
      <ErrorBanner message={error} />
      {o && (
        <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-5">
          <Stat
            label="Clusters"
            value={o.clusters}
            icon={Server}
            alert={o.clusters_error > 0}
            note={o.clusters_error > 0 ? `${o.clusters_error} failing` : undefined}
          />
          <Stat label="Models" value={o.models} icon={Boxes} />
          <Stat label="Tenants" value={o.tenants} icon={Users} />
          <Stat label="Active keys" value={o.active_keys} icon={KeyRound} />
          <Stat label="Quotas" value={o.quotas} icon={Gauge} />
        </div>
      )}
      <Dashboard />
      {data && data.clusters.length > 0 && (
        <section className="card">
          <h2>Cluster sync</h2>
          <table className="plain">
            <thead>
              <tr>
                <th>Cluster</th>
                <th>Site</th>
                <th>Status</th>
                <th>Detail</th>
                <th>Last sync</th>
              </tr>
            </thead>
            <tbody>
              {data.clusters.map((c) => (
                <tr key={c.id}>
                  <td className="strong">{c.name}</td>
                  <td>{c.site || "—"}</td>
                  <td>
                    <StatusBadge status={c.sync_status} />
                  </td>
                  <td className="detail">{c.sync_message}</td>
                  <td className="whitespace-nowrap">{formatTime(c.synced_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
      {data && data.clusters.length === 0 && (
        <section className="card">
          <h2>Get started</h2>
          <ol className="steps">
            <li>
              Add your LLM clusters under <a href="#clusters">Clusters</a>.
            </li>
            <li>
              Register the models they serve under <a href="#models">Models</a>.
            </li>
            <li>
              Create a tenant, issue a key and set its quotas under <a href="#tenants">Tenants</a>.
            </li>
          </ol>
        </section>
      )}
    </>
  );
}
