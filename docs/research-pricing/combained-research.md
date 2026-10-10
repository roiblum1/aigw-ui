# Cost-recovery pricing for the aigw-ui LLM hub

Oct 10, 2026 · @Roi BLum

This merges three research reports into one design for zero-profit, cache-aware pricing in aigw-ui. It was checked against the repo (`main` at 9267053) and against upstream pages where the reports disagreed.

## Decisions

Price each request in dollar-pegged credits from a per-model rate card. Enforce a daily cap in the gateway and 3-hour blocks in the hub. Show dollars in dry-run before enforcing them.

| Topic | Decision | Source |
| --- | --- | --- |
| Unit | 1 credit = $0.00001 (10 µ$). People see dollars; "credits" appears only in config | Report 1 (report 3 proposed 1 µ$) |
| Cost of a request | uncached input × P\_in + cached input × P\_cached + output × P\_out, floored | All three |
| Provisional ratios | Cached input 0.1× input, output 4× input, until benchmarked | Reports 2 and 3; your choice |
| Reasoning tokens | Already inside output. Never added on top | All three |
| Price basis | Fully loaded cost at a target utilization. Idle capacity is paid centrally | All three |
| Windows | Daily cap in the gateway (QuotaPolicy `1d`) plus 3-hour blocks enforced by the hub. No monthly budgets | Your choice |
| Spent budget | The model's existing setting decides: refuse, or serve as best-effort | Hub today |
| Best-effort | Free, dollar value shown, per-tenant daily cap | Report 1; your choice |
| Prefix cache | Shared across all tenants, no `cache_salt` | Report 3; your choice |
| Rollout | Dollars in dry-run first, enforced per model once its acceptance tests pass | Report 2 |
| vLLM | 0.30.x, `kv_transfer_params` stripped at the gateway | Reports 1 and 3, versions checked |
| Chargeback | Showback only, until Finance asks for internal transfers | Reports 1 and 2 |

## Blockers before money quotas

No money quota can be enforced on a multi-site model today, whatever the pricing. Agent Router up to 1.2.0 attaches no quota to an entry route that has zone weights. None of the three reports caught this; it is the hub's own 0.8.0 known problem.

1. **Multi-site entry routes count nothing.** In the lab, a tenant limited to 30 tokens an hour was always answered and Redis stayed empty.
   - Cause: the gateway only attaches a quota to an upstream named `…/rule/<n>`. With zone weights, Envoy Gateway names it `…/rule/<n>/backend/<m>`.
   - Upgrading to 1.2.0 does not fix it. [PR #2833](https://github.com/theagentrouter/agent-router/pull/2833) was opened on Oct 9 and has no reviews.
   - Ways out: build the controller image with #2833 applied (its author tested on a patched 1.1.0), make the hub set site weights by a patch on the generated cluster instead of zone weights, or wait for upstream.
   - Until then, money quotas and per-tenant showback cover single-site models only.
2. **The hub would reject the cost expression.** `internal/api/cost.go` allows at most 200 characters and only `+ - * / ( )`. The clamped expression is 211 characters and needs `<=`, `?` and `:`. The hub should render it from the rate card (see Cost expression).
3. **Tokens to money is a migration.** Every quota limit, the shared pool and the 90% best-effort threshold change meaning. A changed cost expression does not reset counters, so a switch mid-window mixes tokens and credits in one counter. Switch each model at 00:00 UTC, or reset its counters at the switch.
4. **Tenant identity is not proven unforgeable.** The tenant is `x-aigw-client-id`, written by API-key auth. Nobody has checked that a client-sent `x-aigw-client-id` is overwritten. If it is not, a client can bill another department. This needs a self-test step.
5. **Cached counts are not trustworthy everywhere.**
   - P/D decode nodes report cached ≈ prompt on builds before #54222 (#43370, #47136).
   - vLLM 0.31.x lets a client forge the count through `kv_transfer_params` (#58728). The fix, #58730, is still open.
   - Some builds omit a zero count (#44377) or return null details (#44961).
   - Response: run 0.30.x, strip the field, clamp in the expression, and test each deployment. Where a topology reports wrong counts, price that model's cached tokens at the full input rate and say so on the Models page.
6. **Cost may never reach the counter.** In #2248 (v0.7.0) the counter rose by about 1 per non-streaming request instead of the computed cost. It was closed as not planned, and its status on 1.1 and 1.2 is unknown. The current self-test cannot tell for a model with a cost expression, so it needs an acceptance test.

Two smaller points:

- Quota checks fail open by default (`controller.quotaRateLimitFailureModeDeny: false`). Decide this explicitly.
- The 1.2.0 shared-bucket fix applies to `Distinct` selectors. The hub writes one `RegularExpression` rule per tenant, so it most likely does not affect you. Keep the two-tenant test anyway.

## Pricing model

A request costs the floor of its weighted tokens times the model's prices, in integer credits. Prices are per million tokens and set per model.

```latex
\text{cost} = \left\lfloor \frac{(\text{in} - c)\,P_{in} + c\,P_{cached} + \text{out}\,P_{out}}{10^{6}} \right\rfloor,\qquad c = \min(\text{cached},\ \text{in})
```

**Unit.** One credit is $0.00001.

- Floor rounding loses at most $0.00001 per request.
- The rate limit counter is uint32 (Envoy RLS `hits_addend`). That holds $42,949.67 per tenant, model and window, far above any daily cap.
- Report 3's 1 µ$ caps a window at $4,294.97. That still fits a daily cap, but leaves 10× less room for a model's shared pool.

**Provisional ratios.** P\_cached = 0.1 × P\_in and P\_out = 4 × P\_in. These are starting points, not measurements. Report 1 argued for 0.2× cached, because cached tokens still hold KV memory and are attended at every decode step. The benchmark below settles it.

**Setting P\_in at zero profit.** This uses report 1's formula and report 2's cost categories.

```
C_year = GPU depreciation (capex / 3-4 years) + power x PUE + cooling
         + rack, network and storage share + ops staff share + licences + support
H      = C_year / (GPUs x 8,760)              fully loaded $ per GPU-hour
T_w    = weighted tokens/s per GPU at the latency SLO,
         w = uncached + 0.1 x cached + 4 x output
U      = target utilization, e.g. 0.6
P_in [$/M]  = H / (3,600 x T_w x U) x 1,000,000
credits/M   = $/M x 100,000
```

For illustration only: H = $2.50, T\_w = 10,000 and U = 0.6 give P\_in ≈ $0.116/M (11,574 credits/M), P\_cached ≈ $0.0116/M (1,157) and P\_out ≈ $0.463/M (46,296).

**Rules that keep it zero-profit:**

- **Divide by target utilization, never actual.** Dividing by actual raises prices in a quiet period, which drives tenants away and raises prices again. All three reports agree.
- **Keep three costs apart** (report 2):
  - usage cost, from the rate card;
  - reserved capacity, charged to whoever asked for dedicated capacity;
  - shared idle capacity, paid centrally.
- **Count only goodput as capacity** (report 3). Measure T\_w at throughput that meets the TTFT and TPOT targets.
- **True up once a year.** Under-recovery is absorbed centrally and over-recovery is credited the next year.
- **No minimum charge.** A request worth under one credit is free. Stop tiny-request spam with a separate request-rate limit.
- **No long-context tier yet.** Publish the average context per tenant first. If a few tenants dominate long context, serve it as a separate deployment with its own measured price rather than adding a price cliff to the expression.

**Measuring the ratios.** Run three workloads at target concurrency:

- long input with a 1-token output;
- short input with a long output;
- the first workload again with a warm prefix.

Solve for GPU-seconds per uncached input token, per cached input token and per output token.

- Measure GPU memory hits and LMCache local-disk hits separately (report 2).
- Re-measure when the GPU type, vLLM version, quantization or `max-model-len` changes.

**Price changes.** Change prices only at 00:00 UTC, which is also a 3-hour block boundary, and announce them a day ahead. Version the rate card with an effective date. Because the counters hold money, a change only alters how many tokens the rest of the budget buys.

## Cost expression

The hub renders this expression per model from the rate card, into `perModelQuotas[].quota.costExpression` of the model's `QuotaPolicy`. Admins edit prices, not CEL. Prices below are the illustrative ones.

```yaml
costExpression: >-
  ((cached_input_tokens <= input_tokens ? input_tokens - cached_input_tokens : input_tokens) * 11574u
  + (cached_input_tokens <= input_tokens ? cached_input_tokens : 0u) * 1157u
  + output_tokens * 46296u) / 1000000u
```

- **Integers only.** Literals carry `u`. uint division floors, in the tenant's favour.
- **Clamped.** If cached > input (forged or garbage), the whole prompt is billed as uncached, so lying costs the liar. Subtraction never goes below zero, which in CEL is a runtime error rather than a wrap.
- **Not added:** `reasoning_tokens`, which `output_tokens` already includes. There is no cache-write surcharge either, so `cache_creation_input_tokens` stays out (report 3).
- **No overflow.** Intermediates are uint64, and 10^6 tokens × 10^5 credits = 10^11.
- **Per model without branches.** The `QuotaPolicy` already has one entry per model name, so no `model == …` branches are needed.
- **Never use the upstream example** `input_tokens + cached_input_tokens / 10u + …`, which charges cached tokens at 110%, or `* 0.1` doubles.
- **The gateway has the final word.** The hub's validator guards against typos and does not parse CEL. Check the task log for "Applied, not accepted". The free-form field can stay for admins, with the validator extended to `<=`, `?` and `:` and a higher length limit.

## Windows and enforcement

The gateway enforces each tenant's daily dollar cap. The hub enforces 3-hour blocks by reading that same daily counter.

&#91;embedded content: budget enforcement · daily cap in the gateway, 3-hour blocks in the hub\]

The gateway refuses at the daily cap on its own. Only the 3-hour check depends on the hub's 15-second loop.

**Why the gateway can't do 3 hours.**

- QuotaPolicy windows are a fixed set: `1s`, `1m`, `1h`, `1d`. The CRD rejects anything else ([QuotaPolicy, 1.2](https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/)).
- `Shared` is the only mode. A request passes if any matching bucket has quota, including the model's default bucket. Two rules for one tenant therefore act as OR, not AND.

**How the hub enforces a 3-hour block:**

1. Blocks are fixed: 00–03, 03–06 … 21–24 UTC. They align with the gateway's day, which starts at 00:00 UTC (03:00 in Israel in summer, 02:00 in winter).
2. At each block start, the hub stores every tenant's daily counter in Postgres as a snapshot. At the start of the day it is 0.
3. Every `OVERAGE_INTERVAL` (15 s) the overage loop reads the daily counters, as it does today. Block usage is the counter minus the snapshot.
4. When block usage reaches the threshold of the 3-hour budget (90% today, configurable), the tenant is moved as now:
   - when the model serves a spent budget as best-effort, the tenant is listed on the best-effort route;
   - when it refuses, the hub renders the tenant's rule with a spent limit until the block ends. This path has not been tested.
5. At the block end, the tenant is `standard` again.

**Edge cases:**

- **Lag.** A tenant can overshoot by what it sends in 15 s plus one sync. The daily cap still bounds it.
- **The hub is down.** The daily cap keeps working, because the gateway enforces it. Only the 3-hour block needs the hub.
- **Missed snapshot.** The hub takes it at restart and marks that block's usage as low confidence. Undercounting favours the tenant.
- **Usage reset.** A reset deletes counters, so the snapshot can exceed the counter. Clamp block usage to 0 and take a new snapshot.
- **Changing a rule's limit keeps the usage.** The counter name is built from the rule position, the pattern and the window start, not the limit.
- **Best-effort windows.** The best-effort move today handles only `1h` and `1d` quotas. Daily caps plus hub blocks stay within that.

## Best-effort

Best-effort stays free to the department. Its dollar value is shown next to the budget, and each tenant gets a daily best-effort cap.

- **Why free.** Idle GPUs cost mostly power, which is paid anyway (reports 1 and 3). The ledger records the estimated resource cost separately from the budget debit, which is 0 (report 2).
- **Why capped.** Unlimited free capacity hides the demand signal needed for capacity planning, and lets one team crowd out the others (reports 1 and 2).
- **Priority already works.** The `fleetbe-<model>` route sets `x-llm-d-inference-objective: best-effort`. The hub creates an `InferenceObjective` with priority −1. A full site answers 429 and the request moves to the next site. Report 3's KServe `--router-queue-*` flags could not be found in any documentation, and they aren't needed.
- **The hub enforces the cap, not the gateway.** The best-effort `QuotaPolicy` counts every tenant in shadow mode, and its default bucket never runs out. In `Shared` mode a gateway rule there would never refuse. Instead:
  - The overage loop reads each tenant's best-effort counter. It is in credits, through the same cost expression.
  - At the cap, the hub removes the tenant from the best-effort route.
  - The tenant then gets the main route's 429 until 00:00 UTC.
- **Keep the shared pool at 1 credit on a best-effort model.** A large pool makes the move to best-effort pointless.
- **When to revisit.** If best-effort grows past about 30% of all tokens, consider a discounted flex rate (report 1).
- **Known limits.** At most 200 tenants per model on the best-effort route, and the 4,096-character header-match limit.

Open question: who sets each tenant's best-effort cap, and is the default a share of the model's spare daily capacity?

## Prefix cache

All tenants share one prefix cache. A tenant's discount can therefore come from another tenant's earlier prompt. That saving is real compute avoided, and it stays with whoever gets the hit.

**The risk you are accepting.** With a shared cache, any tenant can tell by timing (TTFT) whether a prefix was recently sent by anyone. This is the CVE-2025-46570 class. You chose a higher hit rate over that isolation.

If one department later needs isolation, set a static `cache_salt` for its tenants. Use `bodyMutation.set` on a route rule that matches those tenants. No Lua is needed. The salt value can only be static, so this works per department, not per user.

**Three different cache cases** (report 2):

| Case | What the GPU does | Charge |
| --- | --- | --- |
| Cache miss | Full prefill, then decode | Input + output at full rate |
| Prefix KV hit | Skips prefill for the cached blocks, still holds them in memory during decode | Cached part at 0.1× |
| Full response hit | Nothing. A stored answer is returned | No inference charge. Not built; a separate feature with its own freshness and security rules |

**Routing already favours hits.** Between sites, a consistent hash on the session header keeps a conversation on one site. Within a site, llm-d scores replicas by prefix. Report 3's claim that hits depend on luck doesn't apply to your stack.

**Trusting the count.** `cached_tokens` includes LMCache hits, because vLLM adds local and external hits together. Test each deployment, because these cases break the count or the answer:

- the P/D and version bugs under Blockers;
- LMCache #5576: on vLLM ≥0.31 with the V2 runner, a full hit is answered from empty KV blocks;
- LMCache #4463: silent KV corruption on vLLM 0.26 fused layouts.

A cache hit is not proof that correct work was done.

## Usage ledger and showback

The 3-hour snapshots double as usage history: 8 rows per tenant, model and day in Postgres. Today the hub keeps only the current window, so this is the first past usage it keeps.

**One ledger row** holds:

- tenant, model, block start, and the rate card version;
- credits used as standard, and credits used as best-effort;
- a confidence flag: `complete`, `late snapshot`, `reset`, or `uncounted` (a multi-site model before #2833).

The counter is shared across sites, so the ledger has no per-site split.

**Three numbers, shown apart** (report 2): the estimated resource cost, what was debited from the budget, and what is paid centrally (best-effort, idle capacity, reservations).

**Department view:**

- today's spend against the daily cap;
- the current 3-hour block, used and remaining;
- the dollar value of best-effort use and its cap;
- spend by model;
- a warning before a cap is reached.

Cache savings per tenant need more than the counter, which holds only total credits. That needs the gateway's token metrics labelled by `x-aigw-client-id`. Check whether the controller can add a request header as a metric attribute; this is phase 4.

**Admin view** (report 2):

- rate cards with effective dates, and benchmark results;
- actual spend against recovered cost, and unallocated capacity;
- cached-count health per deployment, and accounting anomalies;
- audit history and export.

**Reconciliation.** A weekly job compares vLLM's per-model prompt and generation counters in Prometheus with the ledger. The gap is aborted streams, uncounted multi-site traffic and failures. It is absorbed centrally and shown as a percentage.

**Boundaries:**

- **No prompts or responses in the ledger.** Only counts and money.
- **Showback, not chargeback.** FinOps treats showback as always required and chargeback as an accounting-policy choice. Add internal transfers only if Finance asks, settled at the annual true-up.

## vLLM, KServe and gateway configuration

Run vLLM 0.30.x with three flags, strip `kv_transfer_params` on every `AIServiceBackend`, and stage an upgrade to Agent Router 1.2.0 for its stream-usage fixes. One uniform set everywhere: settings that are inert on a given model stay in, so nobody removes them later.

**vLLM on every model:**

```
--enable-prefix-caching --enable-prompt-tokens-details --enable-force-include-usage
```

- The last two default to off. Without them there is no cached count, and streamed requests may carry no usage.
- On an `LLMInferenceService`, pass them through `VLLM_ADDITIONAL_ARGS` on the `main` container, or as `args` that start with `"--"`. Otherwise the template's `args: ["--"]` swallows the first flag. This comes from KServe 0.20 docs; confirm it on 0.21.
- **Version 0.30.x.**
  - Up to and including 0.29.0, [CVE-2026-94622](https://vulert.com/vuln-db/CVE-2026-94622) and [CVE-2026-94626](https://vulert.com/vuln-db/CVE-2026-94626) allow a DoS through `kv_transfer_params`. Both are fixed in 0.30.0.
  - 0.31.0 added the cached-count forgery (#58728). [The fix](https://github.com/vllm-project/vllm/pull/58730) is still open.
  - Report 3's "≥0.29.0" is wrong.
- Check each build for an omitted zero count (#44377) and null details (#44961).

**Gateway:**

- **Strip `kv_transfer_params`** with `bodyMutation: {remove: ["kv_transfer_params"]}` on every `AIServiceBackend` ([header and body mutations](https://theagentrouter.ai/docs/1.0/capabilities/traffic/header-body-mutations/)).
  - The hub renders this on `fleet-` and `fleetbe-` backends. Backends of discovered models belong to the cluster charts.
  - llm-d's P/D sidecar adds the field downstream of the gateway, so P/D keeps working.
- **Tenant from auth only.** `x-aigw-client-id` must come from API-key auth, and a client-sent copy must be overwritten (test H5).
- **Fail-open or fail-closed.** Set `controller.quotaRateLimitFailureModeDeny` deliberately. If you choose fail-open, keep the static fallbacks conservative.

**Staged upgrade, test cluster first** ([1.2 release notes](https://theagentrouter.ai/release-notes/v1.2/)):

1. Envoy Gateway 1.9.1 → 1.9.2, plus the Gateway API 1.6 CRDs.
   - Lua `EnvoyExtensionPolicy` is off by default.
   - Do not enable `mergeBackends`.
   - `ReferenceGrant to.name` is stricter.
2. Agent Router 1.1.0 → 1.2.0 (released Oct 6). Gains:
   - usage is recorded when a client disconnects right after the final SSE frame;
   - a duplicate `stream_options` no longer drops `include_usage`;
   - the `Distinct` bucket fix.
3. Inference Extension CRDs → 1.6.x, then redeploy the llm-d endpoint picker.
4. Run the hub self-test on one cluster. The entry-route patch names the generated route, so re-run it after every gateway upgrade.

The upgrade does not fix multi-site quotas (#2833) or streams aborted midway. Those stay uncharged and show up in reconciliation.

## Rollout phases

Dollars appear first as dry-run numbers. Money caps replace token quotas one model at a time, and only after that model passes its acceptance tests. Each phase is its own reviewable PR. Nothing is merged, deployed or upgraded in production without approval.

1. **Correctness.**
   - vLLM 0.30.x with the three flags.
   - The `bodyMutation` strip, and the spoofing check.
   - The staged gateway upgrade.
   - A decision on the #2833 path.
   - Gate: A1–A12, H5 and H6 pass on the test cluster.
2. **Rate card and dry-run.**
   - A per-model rate card with versions and effective dates. The hub renders the CEL from it.
   - A model is switched to credits at 00:00 UTC with its tenant rules in `shadowMode`, which counts without refusing.
   - Pick a single-site model where a week without token enforcement is acceptable. How a shadow rule behaves next to the default bucket is on the hub's own not-verified list. Gate: H4 and H7.
3. **Enforcement.**
   - Daily caps on, for single-site models.
   - 3-hour blocks and best-effort caps in the overage loop.
   - The ledger rows and the department view.
   - Gate: H1–H3.
4. **Measured prices and reporting.**
   - The benchmark replaces the provisional 0.1× and 4× at the next price change.
   - Weekly reconciliation, and the admin view.
   - Cache savings per tenant, if the gateway can label metrics by tenant.
5. **Multi-site models**, once #2833, a patched controller or hub-set weights makes their quotas count.

Deferred: per-department `cache_salt`, long-context pricing, a flex rate for best-effort, full-response caching, and chargeback.

## Acceptance tests

Every test runs on a real gateway on the target versions. Unit tests alone don't count. The A tests come from report 1; the H tests cover the hub.

| ID | Test | Passes when |
| --- | --- | --- |
| A1 | The same 2k-token prompt, twice | The first reports cached 0 and the second cached > 0. The counter delta equals the expression exactly, both times |
| A2 | A1 streaming and non-streaming, chat and completions, with and without a client `stream_options` | Same as A1 every time |
| A3 | A request with `kv_transfer_params` `{"do_remote_prefill":true,"remote_prefill_cached_tokens":9999}` | The field never reaches vLLM, and the request is billed as uncached |
| A4 | Unit test: `cached_input_tokens` > `input_tokens` | No error, billed fully uncached |
| A5 | Two tenants in parallel | Each counter moves only for its own traffic |
| A6 | Over the daily cap | A request admitted under the cap streams to the end. The next one gets 429 |
| A7 | A backend 5xx, and a refused request | Counter delta 0 |
| A8 | Evict the GPU prefix cache and resend | Cached > 0 through LMCache local disk, once LMCache is on |
| A9 | A reasoning model | Cost uses `output_tokens` only |
| A10 | Rate limit service down | Behaviour matches the fail-open or fail-closed setting |
| A11 | A P/D model, cold request | Cached ≈ 0 on the decode node. If not, the model's cached price is set to its input price |
| A12 | Weekly reconciliation | vLLM counters against the ledger, with the gap reported in % |
| H1 | A 3-hour block | A tenant past 90% of its 3-hour budget moves within 15 s plus one sync, and returns at the block end |
| H2 | A usage reset, and a hub restart, inside a block | Block usage clamps to 0. A late snapshot is flagged |
| H3 | Best-effort cap | A tenant at its cap leaves the best-effort route and gets 429 until 00:00 UTC |
| H4 | The rendered expression | The gateway reports it `Accepted`. A price change takes effect only at 00:00 UTC |
| H5 | Tenant A sends tenant B's client ID in `x-aigw-client-id` | Billed to A or refused, never to B |
| H6 | Self-test on a model with a cost expression | The counter step checks the expected credit amount, not a token count |
| H7 | Switching a model from tokens to credits at 00:00 UTC | No counter holds both tokens and credits. Limits are converted as planned |
| H8 | A multi-site entry route, after the #2833 path | A tenant's request is counted once, on the entry backend |

## Brief for the coding agent

Paste this into the agent working on `roiblum1/aigw-ui`. It follows the decisions above.

```
ROLE: Add zero-profit, cache-aware money quotas to aigw-ui (hub for Agent Router / Envoy AI Gateway).
Stack today: Envoy Gateway 1.9.1, Envoy AI Gateway 1.1.0, KServe 0.21.0 (LLMInferenceService, llm-d),
vLLM, LMCache (local disk). Quotas are QuotaPolicy (Shared mode, one RegularExpression rule per tenant on
x-aigw-client-id), counters in hub Redis, best-effort via fleetbe-<model> + InferenceObjective.
Read docs/architecture.md and docs/optimization.md first. Work in phases, one PR each.
Do not merge, deploy or upgrade production without approval. Keep PR #13 separate.

DECIDED
- Unit: 1 credit = $0.00001. Counters, limits and the cost expression are integer credits; UI shows $.
- Rate card per model in Postgres: P_in, P_cached, P_out (credits per 1M tokens), version, effective date,
  methodology, and an "unpriced" state. A model without a rate card keeps token quotas; never invent prices.
- Provisional ratios: P_cached = 0.1 x P_in, P_out = 4 x P_in, until a benchmark replaces them.
- The hub renders costExpression from the rate card:
  ((cached_input_tokens <= input_tokens ? input_tokens - cached_input_tokens : input_tokens) * P_IN
   + (cached_input_tokens <= input_tokens ? cached_input_tokens : 0u) * P_CACHED
   + output_tokens * P_OUT) / 1000000u
  Never add reasoning_tokens or cache_creation_input_tokens. No doubles.
  Extend internal/api/cost.go for the admin free-form field: allow <=, ?, : and raise the 200-char limit.
- Windows: gateway enforces a daily cap (QuotaPolicy 1d). The hub enforces fixed 3-hour blocks
  (00-03 ... 21-24 UTC): snapshot each tenant's daily counter in Postgres at block start; block usage =
  counter - snapshot (clamp at 0); at the threshold (90%, configurable) move the tenant per the model's
  spent mode (best-effort route, or render the rule with a spent limit until block end); restore at block end.
  No monthly windows. QuotaPolicy accepts only 1s/1m/1h/1d, and Shared mode ORs a tenant's rules.
- Best-effort: free (no budget debit), dollar value shown, per-tenant daily cap enforced by the overage loop
  (drop the tenant from the fleetbe route at the cap). Keep the shared pool at 1 credit on best-effort models.
- Prefix cache shared across tenants: no cache_salt.
- Ledger: one row per tenant, model and 3-hour block (standard credits, best-effort credits, rate card
  version, confidence: complete / late snapshot / reset / uncounted). Show estimated cost, budget debit
  and centrally paid cost separately. Never store prompts or responses.
- Switch a model from tokens to credits only at 00:00 UTC, or reset its counters at the switch.
- Price changes only at 00:00 UTC, announced a day ahead.

PLATFORM CHANGES (prepare manifests and runbook; do not apply to production)
- vLLM 0.30.x on every model with --enable-prefix-caching --enable-prompt-tokens-details
  --enable-force-include-usage (via VLLM_ADDITIONAL_ARGS on the main container, or args starting "--").
  <=0.29.0 has CVE-2026-94622/94626; 0.31.x has cached-count forgery (#58728, fix #58730 unmerged).
- bodyMutation remove ["kv_transfer_params"] on every AIServiceBackend the hub renders (fleet-, fleetbe-);
  document it for the cluster charts' backends too.
- Staged upgrade: EG 1.9.2 + Gateway API 1.6 CRDs -> Agent Router 1.2.0 -> Inference Extension 1.6.x CRDs ->
  redeploy the llm-d endpoint picker -> hub self-test. Do not enable mergeBackends.
- Set controller.quotaRateLimitFailureModeDeny explicitly (ask me which).

BLOCKER: multi-site entry routes count nothing up to 1.2.0 (agent-router PR #2833, open). Money quotas
ship for single-site models first. Prepare the options (patched controller image with #2833, or hub-set
site weights without zone weights) and ask me before building either.

PHASES
1 Correctness: flags, strip, spoofing self-test step, self-test that checks credits (H6), upgrade runbook.
2 Rate card, rendered CEL, migration, dry-run (shadowMode) on one single-site model.
3 Daily caps, 3-hour blocks, best-effort cap, ledger rows, department view.
4 Benchmark script for the ratios (cold, warm prefix, LMCache disk), weekly reconciliation, admin view.
5 Multi-site models after the #2833 decision.

DEFER: per-department cache_salt, long-context pricing, best-effort flex rate, response caching, chargeback.

ACCEPTANCE: tests A1-A12 and H1-H8 in the design doc, on a real gateway.
```

## Where the reports disagreed

Factual conflicts were settled by checking the source. Policy conflicts were settled by your answers, or by a default you didn't object to.

| Topic | Report 1 | Report 2 | Report 3 | Resolution |
| --- | --- | --- | --- | --- |
| Unit | 10 µ$ credits | Dollars shown, precise ledger | 1 µ$ | 10 µ$, by default |
| Cached input price | 0.2× | 0.1× provisional | 0.1× | 0.1×, your choice. The benchmark decides |
| Output price | 5× | 4× | 4× | 4×, your choice |
| Windows | Month + day via BackendTrafficPolicy | Monthly in a hub ledger | Daily only | Daily in the gateway plus 3-hour blocks in the hub, your choice. QuotaPolicy has no 3-hour window |
| Best-effort | Free, metered, daily cap | Free; cost recorded apart from debit | Free or near-zero | Free, shown, daily cap, your choice |
| Cache isolation | Salt per trust domain, later | Salt per tenant | Share unless compliance needs it | Share everything, your choice |
| Rollout | Enforce credit quotas | Dry-run first | Enforce now | Dry-run first, by default |
| vLLM version | ≤0.30.x | Validate per deployment | ≥0.29.0 | 0.30.x. The CVEs reach 0.29.0 and are fixed in 0.30.0 |
| Stripping `kv_transfer_params` | At the gateway | Not covered | Lua filter or webhook | `bodyMutation.remove`, no Lua |
| Agent Router 1.2.0 date | Oct 6 | Oct 7 | Not stated | Oct 6, per the release notes |
| Disconnect fix | Only right after the final SSE frame | Same | vLLM flag stops mid-stream evasion | Only after the final frame. Streams aborted midway stay uncharged |
| Shared-bucket bug | Critical | Critical | Not covered | `Distinct` selectors only. The hub uses `RegularExpression` |
| Counter ceiling | uint32 | Not covered | Overflow resets the counter and grants free use | uint32 confirmed. The reset behaviour is unsourced |
| Priority mechanism | Not detailed | Hub's best-effort | KServe `--router-queue-*` flags | The hub's `InferenceObjective`. The flags could not be found |
| Prefix-aware routing | llm-d does it | Not covered | Future work; hits are luck | Already in place |
| Multi-site quotas | Missed | Missed | Missed | Blocker: #2833 |

## Caveats

The design rests on four things nobody has run yet: the hub's 3-hour enforcement, the refuse path for a spent block, the best-effort cap, and costs reaching the counter on 1.1 or 1.2.

- **Prices are illustrative.** The $2.50/GPU-hour and 10,000 weighted tokens/s only show the formula. The 0.1× and 4× ratios are starting points, not measurements.
- **Docs versions.** The CEL variable list and `u`-suffix rule come from the *Next* QuotaPolicy docs. Confirm them against the 1.1 and 1.2 `llmcostcel` source.
- **KServe.** The argument guidance comes from 0.20 docs; no 0.21 page was found.
- **Not checked by me.** The vLLM and LMCache issue numbers come from report 1 and were not re-opened. The exceptions are #58730 and the two CVEs.
- **Shared prefix cache.** A timing side channel between tenants is accepted by choice.
- **Unrecoverable loss.** Streams aborted midway are uncharged on every version. The weekly reconciliation measures that gap; it cannot recover it.

## Sources

Opened and checked for this merge:

- [roiblum1/aigw-ui](https://github.com/roiblum1/aigw-ui): `docs/architecture.md`, `docs/optimization.md`, `docs/release-notes/v0.8.0.md`, `internal/api/cost.go`, `internal/render/quota.go`, `internal/render/counters.go`
- [Agent Router v1.2.x release notes](https://theagentrouter.ai/release-notes/v1.2/)
- [Agent Router QuotaPolicy, 1.2](https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/)
- [Agent Router header and body mutations](https://theagentrouter.ai/docs/1.0/capabilities/traffic/header-body-mutations/)
- [agent-router PR #2833](https://github.com/theagentrouter/agent-router/pull/2833)
- [vLLM PR #58730](https://github.com/vllm-project/vllm/pull/58730)
- [CVE-2026-94622](https://vulert.com/vuln-db/CVE-2026-94622)
- [CVE-2026-94626](https://vulert.com/vuln-db/CVE-2026-94626)

Cited by the reports and not re-opened:

- [vLLM #58728](https://github.com/vllm-project/vllm/issues/58728), [#47136](https://github.com/vllm-project/vllm/issues/47136), [#43370](https://github.com/vllm-project/vllm/issues/43370), [#44961](https://github.com/vllm-project/vllm/issues/44961), [#44377](https://github.com/vllm-project/vllm/issues/44377)
- [agent-router #2248](https://github.com/theagentrouter/agent-router/issues/2248)
- [Envoy RLS proto](https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ratelimit/v3/rls.proto)
- [Usage-based rate limiting](https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/)
- [LMCache #5576](https://github.com/LMCache/LMCache/issues/5576), [#4463](https://github.com/LMCache/LMCache/issues/4463)
- [KServe LLMInferenceService](https://kserve.github.io/website/docs/concepts/architecture/control-plane-llmisvc)
- [vLLM automatic prefix caching](https://docs.vllm.ai/en/stable/design/prefix_caching/)
