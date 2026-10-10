import { useCallback, useEffect, useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from "react";
import { X, type LucideIcon } from "lucide-react";
import { Unauthorized, type Unit, type Window } from "./api";

export * from "./money";

export const UNAUTHORIZED_EVENT = "aigw-unauthorized";

function report(err: unknown): string {
  if (err instanceof Unauthorized) window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
  return err instanceof Error ? err.message : String(err);
}

/** Loads data on mount and whenever reload() is called. */
export function useLoad<T>(load: () => Promise<T>) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  const loadRef = useRef(load);
  loadRef.current = load;

  const reload = useCallback(async () => {
    try {
      setData(await loadRef.current());
      setError("");
    } catch (err) {
      setError(report(err));
    }
  }, []);

  useEffect(() => {
    reload();
  }, [reload]);

  return { data, error, reload };
}

/** Calls reload on a timer, but only while the tab is visible. */
export function usePolling(reload: () => Promise<void>, intervalMs: number) {
  useEffect(() => {
    const timer = setInterval(() => {
      if (!document.hidden) reload();
    }, intervalMs);
    return () => clearInterval(timer);
  }, [reload, intervalMs]);
}

/** Runs one action at a time and keeps its error for display. */
export function useAction() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const run = useCallback(async (action: () => Promise<unknown>) => {
    setBusy(true);
    setError("");
    try {
      await action();
      return true;
    } catch (err) {
      setError(report(err));
      return false;
    } finally {
      setBusy(false);
    }
  }, []);

  return { busy, error, run, clearError: () => setError("") };
}

export function ErrorBanner({ message }: { message: string }) {
  if (!message) return null;
  return (
    <div className="banner error" role="alert">
      {message}
    </div>
  );
}

export function Modal(props: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current;
    dialog?.showModal();
    // Start on the dialog itself rather than its close button.
    if (!dialog?.querySelector("[autofocus]")) dialog?.focus();
    return () => dialog?.close();
  }, []);
  return (
    <dialog ref={ref} tabIndex={-1} className={props.wide ? "modal wide" : "modal"} onCancel={props.onClose}>
      <header>
        <h2>{props.title}</h2>
        <button type="button" className="ghost" onClick={props.onClose} aria-label="Close">
          <X />
        </button>
      </header>
      {props.children}
    </dialog>
  );
}

export function FormModal(props: {
  title: string;
  submitLabel: string;
  onClose: () => void;
  onSubmit: () => Promise<unknown>;
  children: ReactNode;
  wide?: boolean;
}) {
  const { busy, error, run } = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (await run(props.onSubmit)) props.onClose();
  };
  return (
    <Modal title={props.title} onClose={props.onClose} wide={props.wide}>
      <form onSubmit={submit}>
        <ErrorBanner message={error} />
        {props.children}
        <footer>
          <button type="button" onClick={props.onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy}>
            {busy ? "Saving…" : props.submitLabel}
          </button>
        </footer>
      </form>
    </Modal>
  );
}

export function Field(props: { label: string; hint?: string; children: ReactNode }) {
  return (
    <label className="field">
      <span>{props.label}</span>
      {props.children}
      {props.hint && <small>{props.hint}</small>}
    </label>
  );
}

export function StatusBadge({ status }: { status: string }) {
  const label = { synced: "Synced", pending: "Pending", error: "Error" }[status] ?? status;
  return <span className={`badge ${status}`}>{label}</span>;
}

export function PageHeader(props: { title: string; subtitle?: string; children?: ReactNode }) {
  return (
    <div className="page-header">
      <div>
        <h1>{props.title}</h1>
        {props.subtitle && <p>{props.subtitle}</p>}
      </div>
      <div className="actions">{props.children}</div>
    </div>
  );
}

export function Empty({ icon: Icon, children }: { icon: LucideIcon; children: ReactNode }) {
  return (
    <div className="empty">
      <Icon aria-hidden />
      <div>{children}</div>
    </div>
  );
}

export function WindowSelect(props: { value: Window; onChange: (w: Window) => void }) {
  return (
    <select value={props.value} onChange={(e) => props.onChange(e.target.value as Window)}>
      <option value="1d">per day</option>
      <option value="1h">per hour</option>
      <option value="1m">per minute</option>
    </select>
  );
}

export const windowLabel: Record<Window, string> = { "1m": "minute", "1h": "hour", "1d": "day" };


/** A field for a limit: whole tokens, or dollars and cents. */
export function LimitInput(props: {
  unit: Unit;
  value: string;
  onChange: (value: string) => void;
  label?: string;
  autoFocus?: boolean;
  onKeyDown?: (e: KeyboardEvent<HTMLInputElement>) => void;
}) {
  const money = props.unit === "credits";
  return (
    <input
      required
      type="number"
      min={money ? 0.00001 : 1}
      max={money ? 42949.67 : 4294967295}
      step={money ? "any" : 1}
      autoFocus={props.autoFocus}
      placeholder={money ? "Dollars" : "Tokens"}
      aria-label={props.label}
      value={props.value}
      onChange={(e) => props.onChange(e.target.value)}
      onKeyDown={props.onKeyDown}
    />
  );
}

export function formatTime(iso: string | null): string {
  return iso ? new Date(iso).toLocaleString() : "never";
}
