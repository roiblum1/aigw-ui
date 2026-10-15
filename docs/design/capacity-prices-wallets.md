# Capacity, prices and wallets: the target design

## Summary

**Price belongs to the model; capacity belongs to the LLMI.** GLM 5.3 costs the same per million tokens on every site and GPU. What differs between the B300 deployment and the 8×H100 one is only how many of those dollars a second one instance serves.

Up to 0.12.3, aigw-ui does dynamic token management and routing between sites. This design adds a planning layer on top, so the person who hands out budgets reads the answer instead of working it out:

1. **Measure.** Every LLMInferenceService (LLMI) is benchmarked with one versioned profile, isolated from users when possible.
2. **Price.** Every model has one price, by default its OpenRouter price, so a dollar means "what this traffic would cost on OpenRouter".
3. **Plan.** The hub turns measurements and prices into dollars a day the fleet can give, what is given, what is left, and which model runs out first.
4. **Wallets.** A tenant can get one budget per hour or per day across all models, with optional per-model caps inside it.

The starting point is main at 0.14.2: measurements keyed by shape, "can be given / ceiling / left" per model (0.14.0), site weights from measurements (0.14.1), prices by hand with optional GPU costs (0.14.2). This design keeps those features, re-keys measurements from shape to LLMI, defaults prices to the market, and adds the fleet view and wallets.

How the parts fit:

```
Benchmark every LLMI ───┐
(one profile,           │
 drained or live)       ├──► Budget planner ──────────► Budgets and wallets
                        │    (left to give per day,      (set by the token manager,
One price per model ────┘     busiest hour per model)     per model or per tenant)
(OpenRouter by default,             ▲                              │
 or set by hand)                    │ 30 days of usage             ▼
                             Gateway enforces ◄─────────────── Hub renders
                             (refuses at the limit,            (QuotaPolicy per model,
                              counts spend in Redis)            wallet rule per tenant)
```

## Decisions

Six decisions are taken; three questions stay open and have a default until answered.

| Question | Decision |
| --- | --- |
| Price per model or per deployment | One price per model, the same on every site, GPU and topology |
| Where prices come from | The OpenRouter price of the model by default; any price can be overridden by hand |
| GPU costs | Not part of any budget or price calculation. Kept only for an optional report: fleet cost against market value |
| Wallet and per-model budget together | The wallet caps the total; a per-model budget caps that model inside the wallet. Both are enforced by the gateway |
| Wallet window | Per hour or per day, chosen per tenant |
| Benchmarking an LLMI that serves users | Allowed, after a warning that names the users affected and a confirmation; the result is marked as measured under live traffic |

Open, with the default the build uses until decided:

- **Is GLM at FP4 on B200 the same model as GLM at FP8 on H200?** Default: yes when served under the same name, so the same price and the entry route mixes them. The Capacity page shows each LLMI's quantization so the difference is visible. Recommended before relying on it: a small accuracy check of both on the same questions. If they differ enough to matter, serve them under two names.
- **Wallet enforcement in the gateway or in the hub.** Default: in the gateway if lab check A passes (section Wallets), else in the hub first.
- **One Redis for wallets across sites.** Default: every site's rate-limit service uses the hub's Redis, as the original hub design says; otherwise a tenant gets one wallet per entry site.

## Measuring every LLMI

Each LLMI is measured on its own, with the same profile as every other LLMI, and the result is what one instance of it serves a second at the SLO. Today's runner, Job, log reading and the CI test against the real AIPerf stay; what changes is the key, the profile, the sweep and the isolation.

### The unit is the LLMI, not the shape

The shape key in 0.14 (GPU, GPUs per instance, topology, revision) cannot tell B200 FP4 with LMCache from B200 FP8 without LMCache. The LLMI spec holds all of it: image, args, env, quantization, LMCache, resources, topology.

- **Key:** the LLMI (cluster, namespace, name). Shape stays as information on the page.
- **Spec hash:** SHA-256 of the LLMI's `spec` with every `replicas` field removed. Do not use `metadata.generation`: it also changes when the LLMI is scaled.
- **For prefill/decode**, add the ratio prefill replicas : decode replicas to the hash, since the ratio changes what one unit serves.
- **What one instance is:**
  - single node: one pod;
  - multi-node: one LeaderWorkerSet group, which is how KServe already counts ready instances;
  - prefill/decode: there is no fixed instance. The whole LLMI is measured, and per-instance capacity = the LLMI's result ÷ its decode replicas during the run.
- **Same spec on two sites:** each is measured anyway. Sites differ in network, tuning and versions.

*As built (0.16.0, after review):* the LLMI's `spec` does not hold all of what it runs. KServe merges `LLMInferenceServiceConfig`s into it (the ones `baseRefs` names, and its own defaults, where the vLLM image usually is), and an image can be pushed again under the same tag. The hash therefore also covers the specs of the configs `baseRefs` names and the image digests the ready `main` containers run, by role. While a rollout runs (ready pods of one role on different images) the stored hash is kept, so results do not drop back and forth; a measurement waits until the pods agree. *Second review:* KServe's presets (the template, the scheduler) are applied without a `baseRefs` entry, so the hash takes the configs from `status.appliedConfigs`, which lists them with their namespaces; a scheduler upgrade made in a preset then changes it. And dropping a result on every hash change made a model that is tuned often switch between measured and declared site weights, moving sessions each time: a changed hash now keeps the result, marked stale, as for a new profile version.

### One benchmark profile, versioned

A profile is a row in a new table `benchmark_profiles`: name, version, workload, SLO, sweep, created at, active. Exactly one version is active. Editing a profile creates a new version; old versions stay for history.

| Part | Holds | Default (from 0.14) |
| --- | --- | --- |
| Workload | AIPerf agentic-code settings: prompt and answer lengths, shared-prefix layers, turns, pauses | ~100k-token prompts, 32k shared system prefix, no pause between turns |
| SLO | p95 time to first token, p95 time between tokens, error share | 5 s, 50 ms, 1% |
| Sweep | first concurrency, bisect steps, step time cap, minimum requests per step | 1, 2 bisect steps, 5 min, 60 requests |

Every result stores its profile id and version. Capacity uses only results of the active version.

*As built (0.16.0):* "only results of the active version" and "kept and marked stale … so capacity does not fall to zero all at once" (below) cannot both hold right after the profile changes. A model is counted with one version: the one whose results cover the most of its ready instances, the active one on a tie. After a change the old results keep counting, marked stale, until enough LLMIs are measured with the new version; a model never adds figures of two versions (`internal/llmi`). Across models, each model's own version is used. A profile also carries a highest load (`max_concurrency`, 1,024): the doubling has to stop somewhere for a model that meets the SLO at any load. A deployment that takes shorter requests than the profile's longest prompt gets its sessions scaled down by one factor, as 0.13 did.

### The sweep: up until it breaks, then bisect

1. Run concurrency 1, 2, 4, 8 … until a step misses the SLO or the error share.
2. Bisect between the last passing step and the first failing one, twice by default: (pass + fail) / 2, then again on the side that holds the limit.
3. The result is the highest concurrency that passed, with the tokens a second served at it: uncached prompt, cached prompt, output.

A step that hits the time cap before its minimum requests is marked "few samples", as today.

### Isolation

An LLMI is measured with no user traffic when the platform allows it, and with a warning when it does not.

**Drained (preferred, automatic).** Possible when the model is also served on another site.

1. The hub drains the model on this site with the existing gradual drain of the entry route weight.
2. It waits until `vllm:num_requests_running` is 0 on the LLMI's pods, for at most 10 minutes; otherwise the run fails with that reason.
3. It measures, then restores the weight with the existing gradual ramp.
4. One drained site per model at a time.

*As built (0.16.0):* the drain is the existing one (`SetDrained`), so a site an operator drained is used as it is and stays drained; a drained measurement of a model that no other site serves is refused before anything is recorded; the Job starts once the entry weight is 0, at most 30 minutes after the start. After review, the drain is made in the transaction that records the run and ended in the one that ends it, so a hub that stops between two writes cannot leave a site drained with no run to restore it.

**Live (by hand only).** For a model served on one site only, or when the operator chooses it. Before starting, the page shows:

> This LLMI serves users now. The benchmark adds load until it misses the SLO. Users of *model* on *site* will get slower answers or errors for up to *duration*, and their traffic makes the result lower than what the LLMI could serve alone.

The operator confirms by typing the model's name. The result is marked `live`. In live mode the throughput comes from the pod's own counters (`vllm:prompt_tokens_total`, `vllm:generation_tokens_total`, and the prefix-cache hit counter), which include the users' traffic; the latency still comes from AIPerf's own requests.

**Contamination check (both modes).** Each step compares the prompt tokens AIPerf sent with the growth of the pod's prompt-token counter. Foreign share = 1 − sent ÷ counted.

- Drained: above 5% fails the step as contaminated. It usually means clients call the site's own route directly.
- Live: expected; the share is shown with the step.

*As built (0.16.0, after review):* 1 − sent ÷ counted also counts the measurement's own failed requests, whose prompts the pod counts and AIPerf (with server token counts) does not report, so a step at its limit could fail as "contaminated". Drained, other traffic is now the prompt tokens the pods counted in a 20-second quiet spell before the step while nothing was sent, carried over the step; counted − sent (less one longest prompt per failed request) is shown only, since it also holds requests cut off at the end of a step. Live, that second figure is the step's other traffic. Failed requests are checked before contamination. Live throughput is taken over AIPerf's profiling (counters read every 2 seconds), not the Job's wall time with AIPerf's start and exit. AIPerf 0.13 keeps its first-request and last-answer timestamps to itself (they are `INTERNAL`), so the window is the profiling's exported start and end, a few seconds wider than `benchmark_duration`.

### Prefill/decode

Measured through the LLMI's own scheduler on the gateway's peer listener. The Job runs in the gateway namespace and mounts the existing peer client certificate read-only. Lab check B confirms that requests on the peer listener are not counted or limited by any QuotaPolicy; if they are, the Job uses a benchmark tenant with no limits.

*As built (0.16.0):* lab check B passed (peer requests created no quota counter; the peer route goes to the InferencePool and a QuotaPolicy attaches to an AIServiceBackend only), so no benchmark tenant. AIPerf cannot present a client certificate, so the Job's script runs a small local proxy that passes AIPerf's requests to the peer listener with the mounted certificate and streams the answers back. The contamination check and the idle wait read every decode pod's counters.

*After review:* measured on Linux against a stand-in, the first proxy set the result: a listen backlog of 5 dropped most of a step's connections (first-token p95 5.4 s at 64 streams, 27 s at 256), and one Python process could not pass a few hundred streams on in time. It now runs in 8 processes with a large backlog, sends each token at once (no Nagle), keeps its gateway connections between steps and opens a step's connections before it starts; the Job asks for 4 CPUs. At 512 streams it then adds nothing measurable to the time between tokens or to the first token.

### When an LLMI is measured again

| Trigger | Old result |
| --- | --- |
| Spec hash changes | Dropped: the LLMI counts as not measured until measured again. *As built (second review):* kept and marked stale until measured again, so the site weights do not switch to declared with each tuning |
| Active profile version changes | Kept and marked stale, until measured again, so capacity does not fall to zero all at once |
| Result older than 30 days | Kept and marked stale |

### The campaign

Stale and unmeasured LLMIs go into a queue that the hub works through by itself:

- only inside a window set in the chart, for example `capacity.window: "Fri 22:00 - Sat 06:00 Asia/Jerusalem"`;
- one run per site at a time, and one run per model at a time;
- drained mode only. An LLMI that can only be measured live waits for an operator, and the page says so;
- the Capacity page shows the queue, what runs now and what waits for whom.

Site weights from measurements (0.14.1) keep their rule: a model uses measured weights only when every ready LLMI of it is measured, otherwise the annotation.

*As built (0.16.0, after review):* the campaign drains a site only when the model's other sites can take the load: their ready, undrained, measured instances, at no more than 85% of what they serve within the targets, must cover the model's busiest hour inside the window over the last 30 days. Otherwise the LLMI waits for an operator, with the share shown. The busiest hour in the window, not of the whole day: that is the load the others take during the run, and the overall peak would keep the campaign from ever draining a model's biggest site. A model with traffic whose other sites are not measured waits too, so its first measurement is started by hand.

## Prices

A model has one set of prices, by default its OpenRouter price, used as-is. A dollar then means "what this traffic would cost on OpenRouter", with no scaling, no GPU costs and no profit. The ratios between models come from the market, and every budget is in a unit people already understand.

### The market snapshot

OpenRouter's catalog is public and needs no key: `GET https://openrouter.ai/api/v1/models` returns `data[]` with `id`, `hugging_face_id`, `name`, `context_length` and `pricing`. Prices are USD per token, as strings: `"0.0000028"` is 2.80 dollars per million.

- `hack/fetch-market.sh` writes `internal/market/openrouter.json`, which is committed and embedded with `go:embed`. A disconnected hub gets the snapshot inside its binary; the build never depends on a live site.
- Stored per entry: `snapshot_date`, `id`, `hf_id`, `name`, `context`, and per million tokens `prompt`, `completion`, `cache_read`.
  - *As built (0.15.0):* `snapshot_date` is stored once for the file. OpenRouter calls the cached price `input_cache_read`; the script stores it as `cache_read`. One entry per line, so that a weekly snapshot reads as a short diff.
- A scheduled workflow runs the script weekly and opens a pull request titled "Market snapshot YYYY-MM-DD".
- The release workflow refuses a snapshot older than 60 days. `deploy/offline/build-bundle.sh` refreshes it, since it runs on a connected machine.
- A price of `-1` means variable and counts as no price. Long-context tiers in `overrides` are ignored; the base price is used.

### Matching a model to the market

Each model gets a `market_id`:

1. Matched automatically when a served model name equals an entry's `hugging_face_id`.
2. Otherwise taken from the annotation `aigw-ui.io/market-id` on its LLMIs.
3. Editable in the Prices dialog. A model with no match has no market price and is priced by hand.

*As built (0.15.0):*

- OpenRouter lists one model several times with the same Hugging Face id, such as `z-ai/glm-5.3` and `z-ai/glm-5.3:batch`. The match takes the entry without a `:` variant; more than one such entry is no match.
- The order is the other way round: an entry chosen in the dialog wins over the annotation, which wins over the automatic match, so that an operator can correct either. The dialog can also set none, or go back to automatic.

### The Prices dialog

- A **Market** line, for example "OpenRouter, snapshot 2026-10-12: 2.80 in / 8.80 out per million", and **Use market prices**.
- Cached price = the market's `cache_read` when present, else a tenth of the input price, as today.
- Any price can be changed afterwards; that is the custom price. The dialog keeps showing the other models as multiples of the cheapest (0.14.2).
- Prices still start at the next 00:00 UTC, the first prices still convert a model's limits once and start it in dry-run, and later prices change no limit. Nothing changes a price by itself, including a new snapshot: the page shows that the market moved and offers the new price.
  - *As built (0.15.0):* to know that, prices saved unchanged from the market record `<entry>@<snapshot date>` in `model_prices.market`. The server sets it when the saved prices equal the market's, so prices edited after **Use market prices** are custom.

### GPU costs

Optional, and outside every budget figure. With them, one report line compares what the fleet costs a day with the market value of what it served (Budget planner).

## Budget planner

The planner tells the person who hands out budgets how many dollars a day are left to give, and which model runs out first. It extends 0.14.0's "can be given / ceiling / given / left" from shapes to LLMIs, from hand prices to market prices, and adds a fleet line, wallets and a peak-hour check. All of it lives in a pure package, `internal/budget`, like `weights` and `capacity`.

### What one LLMI serves, in dollars

For LLMI *i* of model *m*, at the highest concurrency that met the SLO with the active profile:

```
r_i = (u_i · P_in(m) + c_i · P_cached(m) + o_i · P_out(m)) / 1,000,000
```

*u*, *c*, *o* are uncached prompt, cached prompt and output tokens a second; *P* are the model's prices per million tokens. *r* is market dollars a second for one instance.

### Per model

```
R_m               = Σ_i n_i · r_i
can_be_given_m    = 86,400 · b_m · R_m
ceiling_m         = can_be_given_m · k_m
```

- *n* = ready instances now. An LLMI not measured counts 0 and is listed as such.
- *b* = busy share, 0.6 by default; *k* = overcommit limit, 1.5 by default. Both exist per model since 0.14.0.
- **Given on a model** = the budgets on it of tenants without a wallet, each at the most it allows in a day (one dollar an hour counts as 24), as in 0.14.0. A wallet tenant chooses the model per request, so it is not added to any model; its per-model caps are shown beside the figure.

### The fleet line

```
can_be_given = Σ_m can_be_given_m
ceiling      = Σ_m ceiling_m
given        = Σ_t a_t,  where a_t = 24 · W_t      if tenant t has a wallet per hour
                                     W_t           if tenant t has a wallet per day
                                     Σ_m B_{t,m}   if tenant t has no wallet
left_to_give = ceiling − given
```

Left to give is the first line the page shows, in this form:

`The fleet serves V dollars a day at market prices. X can be given, up to Y with overcommit. Z is given. L is left.`

*As built (0.17.0):* the line says "at the models' prices": a price may be set by hand, and only then is "market" true of every model.

Here V = 86,400 × the sum of every model's *R*: the fleet busy all day at the SLO.

### The overcommit suggestion

0.14.0's 30-day suggestion stays per model, and the fleet line gets the same over all spend, wallets included:

```
s           = busiest day's spend in 30 days / given on that day
k_suggested = min(5, max(1, 1/s))
```

With less than 14 days of history there is no suggestion. A suggestion is applied by hand.

*As built (0.17.0):* budgets are not kept by day, so "given on that day" is what is given now.

### Which model runs out first

With wallets, tenants choose the model, so demand moves between models. The planner checks every model at its busiest hour:

```
ρ_m = busiest hour's spend on m in 30 days / (3,600 · R_m)
```

- Spend includes best-effort and wallet spend on the model; per-model counters exist for wallet tenants too (Wallets).
- Above 0.85 the model is marked saturated at its peak hour.
- The models sorted by *ρ* answer where the next GPU should go.

### Splitting what is left

**Split** takes a set of tenants and an amount up to what is left, and proposes equal wallets, or equal budgets on one model. The proposal is shown for review and applied by hand; nothing changes a budget by itself.

*As built (0.17.0):* equal budgets on one model only, up to what is left on that model and in the fleet; equal wallets come with the wallets (step 5).

### Market value and cost (optional)

An Overview card: yesterday's spend is what the same traffic would have cost on OpenRouter. With GPU costs entered, the fleet's cost a day stands beside it.

### API

`GET /api/v1/budgets` gains:

- per model: `value_per_second` (*R*), `peak_hour_share` (*ρ*), and `llmis[]` with cluster, namespace, name, ready, `value_per_second` (*r*), measured at, profile version, mode (`drained` or `live`), stale;
- a top-level `fleet`: `value_day`, `can_be_given_day`, `ceiling_day`, `given_day`, `wallets_day`, `left_day`, `suggested_overcommit`.

A portal, and later the semantic router, reads these instead of computing them.

## Wallets

A wallet is one budget for a tenant across all priced models, per hour or per day. Per-model budgets stay; for a tenant with a wallet they become caps inside it. The tenant, or later the semantic router, picks the model per request and pays that model's price from the same wallet.

| The tenant has | It can spend |
| --- | --- |
| Per-model budgets, no wallet | As today: each model up to its budget |
| A wallet of 10 dollars a day | Up to 10 dollars a day in total, on any priced model |
| A wallet of 10 dollars a day and a GLM budget of 3 dollars a day | Up to 10 dollars in total, at most 3 of it on GLM |
| A Qwen budget only | Only Qwen, up to its budget, as today |

- A wallet covers **priced models only**. A model counted in tokens needs a per-model budget, as today.
- The amount is in the same credits as priced budgets (1 credit = 0.00001 dollars). The window starts at 00:00 UTC, or on the hour.
- **Best-effort:** on a model with best-effort on, a tenant whose wallet is spent is served as best-effort, exactly like a spent per-model budget, with the same best-effort limit.
- **Database:** `tenants.wallet_credits` and `tenants.wallet_window` (`1h` or `1d`); NULL means no wallet.
- **UI:** a Wallet field on the tenant; per-model budgets of a wallet tenant are labelled "cap inside the wallet"; the tenant's own page (`/my-usage`) shows the wallet, what is spent, and the split by model.

### Enforcement in the gateway (preferred)

A request must pass two checks:

1. **Per model: QuotaPolicy, unchanged.** A wallet tenant with no cap on a model gets a bucket on it equal to its wallet, in the wallet's window. That bucket never binds before the wallet, but it keeps a per-model counter, which the Usage page and the peak-hour check need.
2. **Wallet: one Envoy Gateway BackendTrafficPolicy**, `rateLimit.type: Global`, on the entry listener only, so each request is charged once, where it enters:
   - one rule with `clientSelectors` on the tenant header, `type: Distinct`: one bucket per tenant, no model in the key;
   - the limit read per request with `limit.fromMetadata` (`requests_per_unit`, `unit`), so one rule gives every tenant its own amount and window, instead of one rule per tenant;
   - request cost 0; response cost from the `io.envoy.ai_gateway` metadata that each model's AIGatewayRoute fills through `llmRequestCosts` with that model's price, the same CEL expression its QuotaPolicy uses today;
   - the static `requests` fallback is the largest amount, so a tenant without a wallet is never limited by this rule.

**Who writes the limit.** A Lua filter (EnvoyExtensionPolicy) that the hub renders with a table tenant → amount and unit. It reads the tenant header set by the API-key policy and writes the metadata. It sees headers only, never a prompt, and it is configuration the hub renders, like the cost expressions it renders already.

**Best-effort and the wallet rule.** When the hub moves a wallet tenant to best-effort, it renders that tenant's Lua entry at the largest amount until the window ends, so the wallet rule stops refusing it; the best-effort route counts it apart, as today.

**Redis.** Every site's rate-limit service uses the hub's Redis. Otherwise each entry site keeps its own counter, and a tenant effectively has one wallet per site.

### Lab check A, before any wallet code

1. The Envoy Gateway version in use has `limit.fromMetadata` (envoyproxy/gateway#9216, https://github.com/envoyproxy/gateway/pull/9216). Older releases drop the field silently, so prove it with a small limit that refuses.
2. The Lua filter writes the metadata before the rate-limit filter reads it.
3. One tenant, two models priced 1 and 10 credits per token: the wallet counter grows by each model's own amount.
4. A spent wallet refuses on every model; a spent cap inside the wallet refuses only its model.
5. Two sites with one Redis keep one counter per tenant.

### Fallback: enforcement by the hub

If checks 1 to 3 fail, ship this first. It uses only what the hub already does for best-effort:

- Every 15 seconds the hub adds up the tenant's per-model counters in the wallet's window.
- At the wallet, it renders the tenant's bucket on every model at 1 credit until the window ends, so the gateway refuses it everywhere (or serves best-effort where that is on).
- Overshoot: what the tenant spends in about 15 seconds plus one sync, the same as best-effort moves today.
- While the hub is down the wallet is not enforced; each model still stops the tenant at the wallet amount, so the worst case is the wallet times the number of models in one window. The release note says so.

Moving from the fallback to the gateway later changes no data: the wallet amounts and windows stay where they are.

## Order of work

Five steps, one pull request each after the lab checks; step 3 can run beside step 2.

1. **Lab checks, no code.** Check A (Wallets) and check B: requests through the peer listener for prefill/decode measurements are not counted or limited by a QuotaPolicy. The results decide gateway or hub wallets, and how P/D is measured.
2. **LLMI measurements.** Key by LLMI, spec hash, `benchmark_profiles`, the bisecting sweep, drained and live isolation with the warning, the contamination check, the campaign queue and window. Done when:
   - two LLMIs of one model on two sites are measured and shown apart;
   - scaling an LLMI leaves its result valid, and changing one of its args makes it unmeasured;
   - a drained run takes the site's weight to 0 and back in the existing gradual steps;
   - a live run starts only after the model's name is typed, and is marked `live`;
   - a drained step with more than 5% foreign traffic fails, tested with a stand-in that sends side traffic;
   - the CI job against the real AIPerf still passes.
3. **Market prices.** Snapshot script, embedded file, weekly workflow, release guard, `market_id`, the Market line in the Prices dialog. Done when:
   - the offline bundle builds and the hub runs with no network;
   - a model matched by its Hugging Face id shows its market price, and **Use market prices** fills the dialog with the usual 00:00 UTC start;
   - a new snapshot changes no price.
4. **Budget planner.** `internal/budget`, the fleet line, the peak-hour check, the fleet suggestion, Split, the API fields. Done when:
   - every formula has a unit test with figures worked out by hand;
   - the demo data shows the fleet line and marks a saturated model above 0.85.
5. **Wallets.** Database, tenant UI, rendering (in the gateway or in the hub, as check A decided), usage and the tenant's own page, wallets counted in the planner. Done when:
   - each row of the table in Wallets is tested on a real gateway;
   - the self-test gains "a spent wallet refuses on every model".

## Rules every pull request keeps

- **The hub is a control plane.** No prompt passes through code written for this platform. The Lua filter for wallets sees headers only; the measurement Job makes up its own prompts and the hub reads only numbers.
- **Budgets hold while the hub is down.** Gateway enforcement keeps that true; the hub fallback for wallets is the one documented exception.
- **Nothing changes a price or a budget by itself.** Market moves, overcommit suggestions and Split are shown and applied by hand.
- **One profile version at a time.** Figures from different profile versions are never added together.
- **Release notes say what was tested and what was not**, as every note since 0.7 does, with a migration for any database change and the guides updated: `capacity.md`, `pricing.md`, `how-it-works.md`, `api.md`.
- **One uniform set of settings.** New chart values get defaults that work everywhere, and harmless settings stay in place rather than becoming conditional.
- The existing checks pass: Go tests with the race detector, store tests, the API-server tests, the duplication check, the chart check, and the AIPerf job.
