import { useEffect, useState } from "react";
import { api, type MarketEntry, type MarketPrice, type Model, type Price } from "../api";
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

/** Prices as they will be: those waiting for their day, or those in use. */
const nextPrices = (m: Model) => m.pending_prices ?? m.prices;

/** A market price: dollars for a million tokens. */
const usd = (n: number) =>
  n.toLocaleString("en-US", { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 5 });

const marketSource: Record<MarketPrice["source"], string> = {
  set: "chosen here",
  annotation: "named by aigw-ui.io/market-id",
  name: "matched by its served name",
};

const marketState: Record<MarketPrice["state"], string> = {
  "no-prices": "The model has no prices yet. Use market prices fills them in below.",
  "in-use": "The model's prices are the market's.",
  moved:
    "The market moved since the model's prices were taken from it. Use market prices fills in the new ones; nothing changes until you save them.",
  custom: "The model's prices are set by hand.",
};

/** 6×, 1.5×. */
const times = (n: number) => `${Number(n.toFixed(1))}×`;

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
 * then counted in dollars. One price for the model on every site and GPU; it
 * is set by hand, next to the other models' prices.
 */
export default function ModelPrices(props: {
  model: Model;
  /** Prices to start from instead of the model's, such as the break-even prices of a capacity measurement. */
  suggested?: { price_input: number; price_cached: number; price_output: number };
  suggestedNote?: string;
  onClose: () => void;
  onChanged: () => Promise<void>;
}) {
  const { model } = props;
  const from = props.suggested ?? model.pending_prices ?? model.prices;
  const [input, setInput] = useState(dollars(from?.price_input));
  const [cached, setCached] = useState(dollars(from?.price_cached));
  const [output, setOutput] = useState(dollars(from?.price_output));
  const [now, setNow] = useState(false);
  const [note, setNote] = useState(props.suggestedNote ?? "");
  const history = useLoad(() => api.prices(model.id));
  const models = useLoad(api.models);
  // The market entry is changed here without closing the dialog.
  const [market, setMarket] = useState({ price: model.market, note: model.market_note, set: model.market_id_set });
  const [choosing, setChoosing] = useState(false);
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

  const useMarket = () => {
    const p = market.price?.prices;
    if (!p || !market.price) return;
    setInput(dollars(p.price_input));
    setCached(dollars(p.price_cached));
    setOutput(dollars(p.price_output));
    setNote(`OpenRouter ${market.price.id}, snapshot ${market.price.snapshot_date}`);
  };

  const chooseMarket = (id: string | null) =>
    action.run(async () => {
      await api.setMarketID(model.id, id);
      const fresh = (await api.models()).find((x) => x.id === model.id);
      if (fresh) setMarket({ price: fresh.market, note: fresh.market_note, set: fresh.market_id_set });
      setChoosing(false);
    });

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

  // This model as typed among the others with prices, dearest first.
  const credits = (text: string) => Math.round(Number(text) * CREDITS_PER_DOLLAR);
  const others = (models.data ?? [])
    .filter((m) => m.id !== model.id && nextPrices(m))
    .map((m) => ({ name: m.name, input: nextPrices(m)!.price_input, output: nextPrices(m)!.price_output, waiting: !!m.pending_prices, self: false }));
  const compared = [
    ...others,
    ...(Number(input) > 0 ? [{ name: model.name, input: credits(input), output: credits(output), waiting: false, self: true }] : []),
  ].sort((a, b) => b.input - a.input);
  const cheapest = Math.min(...compared.map((r) => r.input));
  return (
    <FormModal title={`Prices of ${model.name}`} submitLabel="Save prices" onClose={props.onClose} onSubmit={save} wide>
      <ErrorBanner message={action.error || history.error || models.error} />
      <p className="hint">
        What tenants are charged for a million tokens of this model: one price, the same whichever site or GPU serves
        the request. By default it is the model's OpenRouter price, so a dollar means what the same traffic would cost
        there and the models compare as the market does; any price can be set by hand. Each request is charged by these
        prices, rounded down, and the model's quotas and usage are counted in dollars. Reasoning tokens are part of the
        output.
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

      <div className="endpoint">
        {market.price ? (
          <>
            <p>
              <span className="strong">Market:</span> OpenRouter <span className="mono">{market.price.id}</span>,{" "}
              {marketSource[market.price.source]}, snapshot {market.price.snapshot_date}: {usd(market.price.prompt)} in
              {market.price.cache_read != null && `, ${usd(market.price.cache_read)} cached`}, {usd(market.price.completion)}{" "}
              out per million.{" "}
              {market.price.prices && (
                <button type="button" className="link" onClick={useMarket}>
                  Use market prices
                </button>
              )}
            </p>
            <p className="detail">
              {market.price.prices
                ? marketState[market.price.state]
                : "The market's prompt price is free or variable, so it cannot be used as a price."}
            </p>
          </>
        ) : (
          <p>
            <span className="strong">No market price.</span>{" "}
            {market.set === ""
              ? "It was set to none here."
              : market.note
                ? `${market.note.charAt(0).toUpperCase()}${market.note.slice(1)}.`
                : "No name the model is served under is a Hugging Face id in the market snapshot."}{" "}
            Choose its entry, or set the prices by hand.
          </p>
        )}
        {choosing ? (
          <MarketChooser
            busy={action.busy}
            initial={market.price?.id ?? ""}
            onChoose={chooseMarket}
            onCancel={() => setChoosing(false)}
          />
        ) : (
          <button type="button" className="link" onClick={() => setChoosing(true)}>
            Choose the market entry
          </button>
        )}
      </div>

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
      {others.length > 0 && (
        <>
          <h3>Next to the other models</h3>
          <p className="hint">
            Dearest first, with prices waiting for their day in place of those in use. The order and the ratios are what
            matter: a tenant's budget buys more of a cheaper model, so easy work is worth sending to a smaller one.
          </p>
          <div className="table-wrap compare">
            <table className="plain">
              <thead>
                <tr>
                  <th>Model</th>
                  <th className="text-right">Input, per 1M</th>
                  <th className="text-right">Output, per 1M</th>
                  <th className="text-right">Input, × the cheapest</th>
                </tr>
              </thead>
              <tbody>
                {compared.map((r) => (
                  <tr key={r.name}>
                    <td>
                      {r.self ? <span className="strong">{r.name}</span> : r.name}
                      {r.self && <span className="tag">as typed</span>}
                      {r.waiting && <span className="tag warn">waiting</span>}
                    </td>
                    <td className="text-right tabular-nums">{perMillion(r.input)}</td>
                    <td className="text-right tabular-nums">{perMillion(r.output)}</td>
                    <td className="text-right tabular-nums">{cheapest > 0 ? times(r.input / cheapest) : "–"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
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

/**
 * Picks a model's entry in the market snapshot by search, or goes back to
 * the automatic match, or sets none.
 */
function MarketChooser(props: {
  busy: boolean;
  initial: string;
  onChoose: (id: string | null) => void;
  onCancel: () => void;
}) {
  const [query, setQuery] = useState(props.initial);
  const [found, setFound] = useState<MarketEntry[]>([]);
  useEffect(() => {
    const t = setTimeout(() => {
      api
        .market(query)
        .then((r) => setFound(r.models))
        .catch(() => setFound([]));
    }, 200);
    return () => clearTimeout(t);
  }, [query]);
  const exact = found.some((e) => e.id === query.trim());
  return (
    <div className="inline-form">
      <input
        list="market-entries"
        placeholder="An OpenRouter id, such as z-ai/glm-5.3"
        aria-label="Market entry"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={(e) => e.key === "Enter" && e.preventDefault()}
      />
      <datalist id="market-entries">
        {found.map((e) => (
          <option key={e.id} value={e.id}>
            {e.name} · {usd(e.prompt)} in / {usd(e.completion)} out
          </option>
        ))}
      </datalist>
      <button type="button" className="primary" disabled={props.busy || !exact} onClick={() => props.onChoose(query.trim())}>
        Use this entry
      </button>
      <button type="button" disabled={props.busy} onClick={() => props.onChoose(null)} title="Follow aigw-ui.io/market-id and the served names">
        Automatic
      </button>
      <button type="button" disabled={props.busy} onClick={() => props.onChoose("")}>
        None
      </button>
      <button type="button" className="link" onClick={props.onCancel}>
        Cancel
      </button>
    </div>
  );
}
