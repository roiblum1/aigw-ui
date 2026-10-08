import { useState, type FormEvent } from "react";
import { api, getName, setName, setToken } from "../api";
import BrandMark from "../BrandMark";
import { ErrorBanner } from "../components";

export default function Login({ onLogin }: { onLogin: () => void }) {
  const [token, setValue] = useState("");
  const [name, setNameValue] = useState(getName);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await api.checkToken(token);
      setToken(token);
      setName(name.trim());
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
        <label className="field">
          <span>Your name</span>
          <input value={name} maxLength={100} autoComplete="name" onChange={(e) => setNameValue(e.target.value)} />
          <small>Optional. Shown next to your changes in the audit log.</small>
        </label>
        <button type="submit" className="primary mt-1 w-full" disabled={busy}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}
