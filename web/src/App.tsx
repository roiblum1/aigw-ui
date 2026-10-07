import { useEffect, useState } from "react";
import {
  Activity as ActivityIcon,
  Boxes,
  ListChecks,
  LayoutDashboard,
  LogOut,
  Moon,
  Server,
  Sun,
  Users,
} from "lucide-react";
import { getToken, setToken } from "./api";
import Clusters from "./pages/Clusters";
import Models from "./pages/Models";
import Tenants from "./pages/Tenants";
import Usage from "./pages/Usage";
import Activity from "./pages/Activity";
import BrandMark from "./BrandMark";
import Login from "./pages/Login";
import Overview from "./pages/Overview";
import { UNAUTHORIZED_EVENT } from "./components";

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
