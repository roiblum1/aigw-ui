import { useEffect, useMemo, useRef, useState, type FormEvent, type MouseEvent } from "react";
import { marked } from "marked";
import DOMPurify from "dompurify";
import { api, type DocEntry, type DocHit, type UpstreamPage, type UpstreamProject } from "../api";
import { ErrorBanner, PageHeader, useLoad } from "../components";

const FIRST = "how-it-works";
/** Names of upstream pages start with this: "upstream/<project>/<path in its repository>". */
const UPSTREAM = "upstream/";

/** The document named in the address, as in "#docs/user-guide". */
function docFromHash(): string {
  const [, ...rest] = decodeURIComponent(location.hash.slice(1)).split("/");
  return rest.join("/") || FIRST;
}

/** Resolves a link such as "../architecture.md" against the document it is in. */
function resolve(from: string, href: string): string {
  const parts = from.split("/").slice(0, -1);
  for (const part of href.replace(/\.md$/, "").split("/")) {
    if (part === "..") parts.pop();
    else if (part !== "." && part !== "") parts.push(part);
  }
  return parts.join("/");
}

/** The project and the page path of an upstream document's name. */
function upstreamOf(name: string): { project: string; path: string } | null {
  if (!name.startsWith(UPSTREAM)) return null;
  const [project, ...path] = name.slice(UPSTREAM.length).split("/");
  return { project, path: path.join("/") };
}

/** A folder of a project's pages: its own pages and the folders under it. */
interface Folder {
  name: string;
  /** The folder's own page (README, index or _index), shown as its title. */
  index?: UpstreamPage;
  pages: UpstreamPage[];
  folders: Folder[];
}

const isIndex = (path: string) => /(^|\/)(readme|index|_index)$/i.test(path);

/** Sorts pages as the project orders them: by weight, then by title; no weight comes last. */
const byWeight = (a: UpstreamPage, b: UpstreamPage) =>
  (a.weight || 1e9) - (b.weight || 1e9) || a.title.localeCompare(b.title);

/** The project's pages as a tree of folders, from its roots down. */
function treeOf(p: UpstreamProject): Folder {
  const top: Folder = { name: p.name, pages: [], folders: [] };
  for (const page of p.pages) {
    const root = p.roots.find((r) => page.path.startsWith(r + "/")) ?? "";
    let rel = page.path.slice(root.length + 1).split("/");
    // With several roots, each is a folder of its own.
    if (p.roots.length > 1) rel = [root.split("/").pop() ?? root, ...rel];
    let folder = top;
    for (const part of rel.slice(0, -1)) {
      let next = folder.folders.find((f) => f.name === part);
      if (!next) {
        next = { name: part, pages: [], folders: [] };
        folder.folders.push(next);
      }
      folder = next;
    }
    if (isIndex(page.path) && folder !== top) folder.index = page;
    else folder.pages.push(page);
  }
  const sort = (f: Folder) => {
    f.pages.sort(byWeight);
    f.folders.sort((a, b) => byWeight(a.index ?? { path: "", title: a.name }, b.index ?? { path: "", title: b.name }));
    f.folders.forEach(sort);
  };
  sort(top);
  return top;
}

/** Whether the folder holds the page: its own, or one further down. */
function holds(f: Folder, path: string): boolean {
  return f.index?.path === path || f.pages.some((p) => p.path === path) || f.folders.some((g) => holds(g, path));
}

function FolderNav(props: { folder: Folder; project: string; current: string; top?: boolean }) {
  const { folder, project, current } = props;
  const link = (p: UpstreamPage, title = p.title) => {
    const name = UPSTREAM + project + "/" + p.path;
    return (
      <a key={p.path} href={`#docs/${name}`} aria-current={name === current ? "page" : undefined}>
        {title}
      </a>
    );
  };
  const body = (
    <>
      {folder.index && link(folder.index, "Overview")}
      {folder.pages.map((p) => link(p))}
      {folder.folders.map((f) => (
        <FolderNav key={f.name} folder={f} project={project} current={current} />
      ))}
    </>
  );
  if (props.top) return body;
  const here = upstreamOf(current);
  return (
    <details open={here?.project === project && holds(folder, here.path)}>
      <summary>{folder.index?.title ?? folder.name.replace(/[-_]/g, " ")}</summary>
      <div className="docs-folder">{body}</div>
    </details>
  );
}

export default function Docs() {
  const { data: list, error: listError } = useLoad(api.docs);
  const upstream = useLoad(api.upstreamIndex, "upstream-docs");
  const [name, setName] = useState(docFromHash);
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const [filter, setFilter] = useState("");
  const [hits, setHits] = useState<{ q: string; hits: DocHit[] } | null>(null);
  const body = useRef<HTMLDivElement>(null);
  const page = upstreamOf(name);
  const project = page && upstream.data?.projects.find((p) => p.id === page.project);

  useEffect(() => {
    const onHash = () => {
      setName(docFromHash());
      setHits(null);
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  useEffect(() => {
    let current = true;
    const at = upstreamOf(name);
    const load = at
      ? api.upstreamPage(at.project, at.path).then((md) => {
          if (md === null) throw new Error("This image carries no such page.");
          return md;
        })
      : api.doc(name).then((d) => d.markdown);
    load
      .then((md) => {
        if (!current) return;
        setText(md);
        setError("");
        body.current?.scrollIntoView({ block: "start" });
      })
      .catch((err: Error) => current && setError(err.message));
    return () => {
      current = false;
    };
  }, [name]);

  // The guides are built into the server image, so their markup is ours. An
  // upstream page is another project's: its markup is cleaned of anything
  // that could run.
  const html = useMemo(() => {
    const out = marked.parse(text, { async: false, gfm: true });
    return name.startsWith(UPSTREAM) ? DOMPurify.sanitize(out) : out;
  }, [text, name]);

  /**
   * Links between documents stay inside this page; a link to a heading
   * scrolls to it; a link elsewhere opens in a new tab.
   */
  const onClick = (e: MouseEvent<HTMLDivElement>) => {
    const link = (e.target as HTMLElement).closest("a");
    const href = link?.getAttribute("href") ?? "";
    if (!link) return;
    e.preventDefault();
    if (/^[a-z]+:/i.test(href)) {
      window.open(href, "_blank", "noopener,noreferrer");
      return;
    }
    if (href.startsWith("#docs/")) {
      location.hash = href.slice(1);
      return;
    }
    const [path, anchor] = href.split("#");
    if (path.endsWith(".md") && !name.startsWith(UPSTREAM)) {
      location.hash = "docs/" + resolve(name, path);
    } else if (anchor) {
      const wanted = anchor.toLowerCase();
      const heading = [...(body.current?.querySelectorAll("h1, h2, h3, h4") ?? [])].find(
        (h) => (h.textContent ?? "").toLowerCase().replace(/[^a-z0-9 -]/g, "").trim().replace(/ +/g, "-") === wanted,
      );
      heading?.scrollIntoView({ behavior: "smooth", block: "start" });
    }
  };

  const groups = useMemo(() => {
    const out: { group: string; docs: DocEntry[] }[] = [];
    for (const d of list ?? []) {
      const last = out[out.length - 1];
      if (last?.group === d.group) last.docs.push(d);
      else out.push({ group: d.group, docs: [d] });
    }
    return out;
  }, [list]);
  const trees = useMemo(() => (upstream.data?.projects ?? []).map((p) => ({ p, tree: treeOf(p) })), [upstream.data]);

  /** Every title that has the filter in it, ours and upstream. */
  const filtered = useMemo(() => {
    const f = filter.trim().toLowerCase();
    if (f === "") return null;
    const out: { name: string; title: string; group: string }[] = [];
    for (const d of list ?? []) if (d.title.toLowerCase().includes(f)) out.push(d);
    for (const p of upstream.data?.projects ?? [])
      for (const pg of p.pages)
        if (pg.title.toLowerCase().includes(f)) out.push({ name: UPSTREAM + p.id + "/" + pg.path, title: pg.title, group: p.name });
    return out.slice(0, 200);
  }, [filter, list, upstream.data]);

  const search = (e: FormEvent) => {
    e.preventDefault();
    const q = filter.trim();
    if (q.length < 2) return;
    api
      .docSearch(q)
      .then((h) => setHits({ q, hits: h }))
      .catch((err: Error) => setError(err.message));
  };

  return (
    <>
      <PageHeader
        title="Docs"
        subtitle="What each action does, how it works, and why it was built that way; and the documentation of the projects it is built on."
      />
      <ErrorBanner message={listError || upstream.error || error} />
      <div className="docs">
        <nav className="docs-nav" aria-label="Documents">
          <form onSubmit={search} className="docs-filter">
            <input
              type="search"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Filter titles; Enter searches the text"
              aria-label="Filter the titles, or search the text with Enter"
            />
          </form>
          {filtered ? (
            <div>
              <h3>{filtered.length === 0 ? "No title has it: press Enter to search the text" : "Titles"}</h3>
              {filtered.map((d) => (
                <a key={d.name} href={`#docs/${d.name}`} aria-current={d.name === name ? "page" : undefined}>
                  {d.title}
                  <span className="detail"> · {d.group}</span>
                </a>
              ))}
            </div>
          ) : (
            <>
              {groups.filter((g) => g.group !== "Release notes").map((g) => (
                <div key={g.group}>
                  <h3>{g.group}</h3>
                  {g.docs.map((d) => (
                    <a key={d.name} href={`#docs/${d.name}`} aria-current={d.name === name ? "page" : undefined}>
                      {g.group === "Release notes" ? d.title.replace(/ \(.*\)$/, "") : d.title}
                    </a>
                  ))}
                </div>
              ))}
              {trees.length > 0 && (
                <div>
                  <h3>Upstream</h3>
                  {trees.map(({ p, tree }) => (
                    <details key={p.id} open={page?.project === p.id}>
                      <summary>
                        {p.name} <span className="detail">{p.version}</span>
                      </summary>
                      <div className="docs-folder">
                        <FolderNav folder={tree} project={p.id} current={name} top />
                      </div>
                    </details>
                  ))}
                </div>
              )}
              {groups.filter((g) => g.group === "Release notes").map((g) => (
                <div key={g.group}>
                  <h3>{g.group}</h3>
                  {g.docs.map((d) => (
                    <a key={d.name} href={`#docs/${d.name}`} aria-current={d.name === name ? "page" : undefined}>
                      {g.group === "Release notes" ? d.title.replace(/ \(.*\)$/, "") : d.title}
                    </a>
                  ))}
                </div>
              ))}
            </>
          )}
        </nav>
        {hits ? (
          <div className="doc">
            <h1>Search: {hits.q}</h1>
            {hits.hits.length === 0 && <p>Nothing has it.</p>}
            {hits.hits.map((h) => (
              <p key={h.name} className="docs-hit">
                <a href={`#docs/${h.name}`}>{h.title}</a> <span className="detail">· {h.group}</span>
                <br />
                <span className="detail">{h.snippet}</span>
              </p>
            ))}
          </div>
        ) : (
          <div className="docs-page">
            {project && page && (
              <p className="detail docs-source">
                {project.name} {project.version}, from{" "}
                <a href={`https://github.com/${project.repo}/tree/${project.ref}/${page.path.split("/").slice(0, -1).join("/")}`} target="_blank" rel="noopener noreferrer">
                  {project.repo}
                </a>{" "}
                ·{" "}
                <a href={`/upstream-docs/${project.id}/LICENSE`} target="_blank" rel="noopener noreferrer">
                  {project.license ?? "license"}
                </a>
              </p>
            )}
            {/* eslint-disable-next-line react/no-danger */}
            <div ref={body} className="doc" onClick={onClick} dangerouslySetInnerHTML={{ __html: html }} />
          </div>
        )}
      </div>
    </>
  );
}
