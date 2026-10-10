import { useState } from "react";
import { api, type Model, type Price } from "../api";
import { CREDITS_PER_DOLLAR, ErrorBanner, Field, FormModal, formatTime, useAction, useLoad } from "../components";

/** A price for a million tokens, in dollars, with the decimals it needs. */
function perMillion(credits: number): string {
  return (credits / CREDITS_PER_DOLLAR).toLocaleString("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 5,
  });
}

const dollars = (credits: number | undefined) => (credits === undefined ? "" : String(credits / CREDITS_PER_DOLLAR));

/** Rounds away what a multiplication of decimals leaves behind. */
const tidy = (n: number) => String(Math.round(n * CREDITS_PER_DOLLAR) / CREDITS_PER_DOLLAR);

function PriceLine({ p }: { p: Price }) {
  return (
    <>
      {perMillion(p.price_input)} input, {perMillion(p.price_cached)} cached input, {perMillion(p.price_output)} output
    </>
  );
}

/**
 * The prices of one model: what a million tokens cost. The server turns them
 * into the model's cost expression, and the model's quotas and usage are
 * then counted in dollars.
 */
export default function ModelPrices(props: { model: Model; onClose: () => void; onChanged: () => Promise<void> }) {
  const { model } = props;
  const from = model.pending_prices ?? model.prices;
  const [input, setInput] = useState(dollars(from?.price_input));
  const [cached, setCached] = useState(dollars(from?.price_cached));
  const [output, setOutput] = useState(dollars(from?.price_output));
  const [now, setNow] = useState(false);
  const [note, setNote] = useState("");
  const history = useLoad(() => api.prices(model.id));
  const action = useAction();

  // Until measured on the model: a cached prompt token at a tenth of an
  // input token, an output token at four times.
  const changeInput = (value: string) => {
    const before = Number(input);
    const next = Number(value);
    setInput(value);
    if (!(next > 0)) return;
    if (cached === "" || Number(cached) === Number(tidy(before * 0.1))) setCached(tidy(next * 0.1));
    if (output === "" || Number(output) === Number(tidy(before * 4))) setOutput(tidy(next * 4));
  };

  const save = async () => {
    await api.setPrices(model.id, {
      input_usd: Number(input),
      cached_usd: Number(cached),
      output_usd: Number(output),
      now,
      note: note.trim(),
    });
    await props.onChanged();
  };

  const act = (fn: () => Promise<unknown>) =>
    action.run(async () => {
      await fn();
      await props.onChanged();
      await history.reload();
    });

  const firstTime = !model.prices;
  return (
    <FormModal title={`Prices of ${model.name}`} submitLabel="Save prices" onClose={props.onClose} onSubmit={save} wide>
      <ErrorBanner message={action.error || history.error} />
      <p className="hint">
        What a million tokens of this model cost you to serve. Each request is charged by these prices, rounded down,
        and the model's quotas and usage are counted in dollars. Reasoning tokens are part of the output.
      </p>

      {model.prices ? (
        <p>
          <span className="strong">In use:</span> <PriceLine p={model.prices} />, since{" "}
          {formatTime(model.prices.applied_at)}.
        </p>
      ) : (
        <p>
          <span className="strong">No prices yet.</span> The model is counted in tokens.
        </p>
      )}
      {model.pending_prices && (
        <p>
          <span className="strong">Waiting:</span> <PriceLine p={model.pending_prices} />, from{" "}
          {formatTime(model.pending_prices.effective_at)}.{" "}
          <button
            type="button"
            className="link"
            disabled={action.busy}
            onClick={() => act(() => api.deletePendingPrices(model.id))}
          >
            Drop
          </button>
        </p>
      )}

      {model.prices && (
        <div className="endpoint">
          <label className="check">
            <input
              type="checkbox"
              checked={model.price_dry_run}
              disabled={action.busy}
              onChange={(e) => {
                const on = e.target.checked;
                if (
                  !on &&
                  !confirm(
                    `Enforce the quotas of ${model.name} in dollars? Tenants past their limit are refused from the next sync. Check the limits on the Tenants page first: they were converted from tokens.`,
                  )
                )
                  return;
                act(() => api.setPriceDryRun(model.id, on));
              }}
            />
            <span>Dry-run</span>
          </label>
          <p className="detail">
            {model.price_dry_run
              ? "Every tenant is counted in dollars and nobody is refused. Look at the Usage page for a few days, correct the limits, then switch this off."
              : "The quotas are enforced in dollars."}
          </p>
        </div>
      )}

      <h3>{firstTime ? "Set prices" : "New prices"}</h3>
      <div className="grid-3">
        <Field label="Input, $ per 1M tokens" hint="A prompt token the model had to compute.">
          <input required type="number" min={0.00001} max={1000} step="any" value={input} onChange={(e) => changeInput(e.target.value)} />
        </Field>
        <Field label="Cached input, $ per 1M" hint="A prompt token taken from the prefix cache. A tenth of input until measured.">
          <input required type="number" min={0} max={1000} step="any" value={cached} onChange={(e) => setCached(e.target.value)} />
        </Field>
        <Field label="Output, $ per 1M" hint="Four times input until measured.">
          <input required type="number" min={0} max={1000} step="any" value={output} onChange={(e) => setOutput(e.target.value)} />
        </Field>
      </div>
      <Field label="Note" hint="Optional. Where the figures come from, for the next person.">
        <input value={note} maxLength={500} onChange={(e) => setNote(e.target.value)} />
      </Field>
      <label className="check">
        <input type="checkbox" checked={now} onChange={(e) => setNow(e.target.checked)} />
        <span>Start now</span>
      </label>
      <p className="hint">
        {now
          ? "Only possible while no tenant has a quota on the model."
          : "The prices start at the next 00:00 UTC. Every quota window begins anew then, so no counter holds amounts at two prices."}
        {firstTime &&
          " At that moment the model's pool and its tenants' limits are converted from tokens to dollars at the input price, and the model starts in dry-run: nobody is refused until you switch that off."}
      </p>

      {history.data && history.data.length > 0 && (
        <>
          <h3>History</h3>
          <table className="plain">
            <thead>
              <tr>
                <th>From</th>
                <th>Prices for a million tokens</th>
                <th>Note</th>
              </tr>
            </thead>
            <tbody>
              {history.data.map((p) => (
                <tr key={p.version}>
                  <td>
                    {formatTime(p.effective_at)}
                    {!p.applied_at && <span className="tag warn">waiting</span>}
                  </td>
                  <td>
                    <PriceLine p={p} />
                  </td>
                  <td className="detail">{p.note}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </FormModal>
  );
}
