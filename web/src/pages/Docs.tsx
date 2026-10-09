import { useEffect, useMemo, useRef, useState, type MouseEvent } from "react";
import { marked } from "marked";
import { api, type DocEntry } from "../api";
import { ErrorBanner, PageHeader, useLoad } from "../components";

const FIRST = "how-it-works";

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

export default function Docs() {
  const { data: list, error: listError } = useLoad(api.docs);
  const [name, setName] = useState(docFromHash);
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const body = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onHash = () => setName(docFromHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  useEffect(() => {
    let current = true;
    api
      .doc(name)
      .then((d) => {
        if (!current) return;
        setText(d.markdown);
        setError("");
        body.current?.scrollIntoView({ block: "start" });
      })
      .catch((err: Error) => current && setError(err.message));
    return () => {
      current = false;
    };
  }, [name]);

  // The documents are built into the server image, so their markup is ours.
  const html = useMemo(() => marked.parse(text, { async: false, gfm: true }), [text]);

  /** Links between documents stay inside this page; a link to a heading scrolls to it. */
  const onClick = (e: MouseEvent<HTMLDivElement>) => {
    const link = (e.target as HTMLElement).closest("a");
    const href = link?.getAttribute("href") ?? "";
    if (!link || /^[a-z]+:/i.test(href)) return;
    e.preventDefault();
    const [path, anchor] = href.split("#");
    if (path.endsWith(".md")) {
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

  return (
    <>
      <PageHeader title="Docs" subtitle="What each action does, how it works, and why it was built that way." />
      <ErrorBanner message={listError || error} />
      <div className="docs">
        <nav className="docs-nav" aria-label="Documents">
          {groups.map((g) => (
            <div key={g.group}>
              <h3>{g.group}</h3>
              {g.docs.map((d) => (
                <a key={d.name} href={`#docs/${d.name}`} aria-current={d.name === name ? "page" : undefined}>
                  {g.group === "Release notes" ? d.title.replace(/ \(.*\)$/, "") : d.title}
                </a>
              ))}
            </div>
          ))}
        </nav>
        {/* eslint-disable-next-line react/no-danger */}
        <div ref={body} className="doc" onClick={onClick} dangerouslySetInnerHTML={{ __html: html }} />
      </div>
    </>
  );
}
