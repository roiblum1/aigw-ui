# Cache-Aware Cost-Recovery Pricing for an Internal LLM Hub: Stress-Test of the Agent's Design

**Bottom line: the agent's core idea is sound and should go ahead. Bill each request as (uncached input + discounted cached input + weighted output), integer-only, in a dollar-pegged credit unit. But three of its details are wrong or dangerous: the upstream "cached / 10" example overcharges cached tokens; 1/10,000-dollar units are too coarse; and per-tenant money quotas on Envoy AI Gateway 1.1.0 are not safe until you upgrade to Agent Router 1.2.0 and close the forgeable-cache-count and spoofable-tenant-header holes.**

## TL;DR

- **Pricing model:** charge `floor((uncached_in × P_in + cached_in × P_cached + out × P_out) / 1,000,000)`. Prices are integer credits per million tokens, and 1 credit = $0.00001 (10 µ$). Defaults: cached = 0.2× input, output = 5× input, until you measure your own ratios. Reasoning tokens are already inside output, so never add them separately. Set prices from fully loaded cost divided by capacity at a *target* utilization, not actual use. Run showback in dollars first; move to chargeback only if Finance needs it, and true-up yearly so the hub nets to zero.
- **Agent claims:** most of the mechanics are right. The CEL variables exist, they are uint, input includes cached, Envoy's rate-limit counters are uint32, and the vLLM flags exist. The forgery bug is real (vLLM #58728, added in v0.31.0 by #54222; fix PR #58730 still unmerged).\[1\] Several claims are partly wrong:
  - The 1-day window limit applies only to QuotaPolicy. BackendTrafficPolicy supports Month and Year.
  - 1.2.0 fixes only disconnects *right after the final frame*, not mid-stream disconnects.
  - cache_salt is a vLLM feature. It does not give per-tenant hit counters.
- **Before go-live:**
  1. Upgrade to Agent Router 1.2.0, which fixes a QuotaPolicy bug that put all tenants' stream costs into one shared bucket.
  2. Pin vLLM ≤0.30.x or patch it.
  3. Strip `kv_transfer_params` and client-supplied tenant headers at the gateway.
  4. Clamp cached ≤ input in CEL.
  5. Prove with Redis counters that the cost actually lands in each tenant's bucket for both streaming and non-streaming requests.

## Key Findings

### What "Envoy 1.9.1 agent router" actually is

- **There is no "agent-router 1.9.1".** Envoy AI Gateway was renamed **Agent Router** when it joined the Agentic AI Foundation on 10 September 2026.\[2\] Its first release under the new name is **v1.2.0 (6 October 2026)**. CRDs, the `aigateway.envoyproxy.io` API group, Helm charts, images and the `aigw` CLI are all unchanged.\[3\]
- **What 1.1.0 is built on.** Envoy AI Gateway **v1.1.0 (21 August 2026)** is tested against **Envoy Gateway v1.8.1+ (Envoy Proxy v1.38.x)** and Gateway API v1.5.x.\[4\]
  - The sources conflict on the Gateway API Inference Extension version for 1.1.x. The compatibility matrix says v1.6.0. The 1.1 release notes and the 1.2.0 notes ("updated from v1.0.2") say v1.0.2.\[3\]\[4\]
- **What 1.2.0 is built on.** Agent Router **v1.2.0** ships on **Envoy Gateway v1.9.2, Envoy Proxy v1.39.1, Gateway API v1.6.1 and Inference Extension v1.6.2**.\[3\]
- **Your likely setup.** "1.9.1" is most likely your **Envoy Gateway** version. AI Gateway 1.1.0 on EG 1.9.1 is not a combination the project's matrix lists as tested. Agent Router 1.2.0 requires EG **≥1.9.2**, so you need a small EG bump anyway.\[4\]\[5\]

### Verdict table: the agent's 12 claims

| # | Claim | Verdict | Evidence |
|---|---|---|---|
| 1 | AIGW reads `prompt_tokens_details.cached_tokens` and exposes `cached_input_tokens` to CEL; input includes cached, so subtract | **Correct** | `CachedInputToken` type and the `cached_input_tokens` CEL variable are documented. The upstream example `(input_tokens - cached_input_tokens) + …` assumes inclusion.\[6\] OpenAI semantics put cached tokens inside the input total, and vLLM output confirms it (prompt_tokens 21, cached_tokens 16).\[7\]\[8\] |
| 2 | `input_tokens + cached_input_tokens / 10u + …` charges cached at 110% | **Correct, and the bug is upstream too** | The QuotaPolicy docs (Next) use exactly `input_tokens + cached_input_tokens / 10u + output_tokens * 6u` with the comment "Cached input tokens count as 1/10 of a regular input token".\[9\] Because input already includes cached, each cached token actually costs 1.1×. A second upstream example mixes `uint` with `* 0.1` and `* 1.5`.\[6\] CEL has no implicit numeric conversion, so expect that one to fail; test before copying it. |
| 3 | Integer-only CEL; underflow guard needed; variable list | **Partly** | Variables are uint and literals need the `u` suffix. The result must be a non-negative integer. A `double` intermediate *is* allowed if cast back, e.g. `uint(double(x) * 0.1)`, so "decimals rejected" is too strong.\[9\] You still shouldn't use doubles, for reproducible rounding. A guard is needed because CEL uint subtraction below zero is a runtime error, not a wrap. Documented variables: `input_tokens, output_tokens, total_tokens, cached_input_tokens, cache_creation_input_tokens, reasoning_tokens` (uint) and `model, backend, route_name` (string).\[9\] This is from the *Next* QuotaPolicy docs; check `model` in 1.1.0's `llmRequestCosts` before relying on it. |
| 4 | Counter ceiling ≈2^32, so ≈$4,295 per tenant+model+window in µ$; a coarser unit is better | **Ceiling correct; the unit advice is wrong** | Envoy RLS v3 defines `hits_addend` and `requests_per_unit` as **uint32**.\[10\] In µ$ that caps a window at $4,294.97. But 1/10,000 $ (100 µ$) is too coarse: a ~1k-token request costs only ~2 units, so floor rounding can wipe out up to half the charge. Use **10 µ$**, which gives a $42,949 cap and at most $0.00001 lost per request. |
| 5 | Max window is one day in 1.1.0/1.2.0; weekly/monthly only on main | **Partly wrong** | True for **QuotaPolicy** (`1s/1m/1h/1d` only). But **BackendTrafficPolicy** usage-based rate limiting supports **Month and Year**, per the docs' "Choosing a policy" table.\[9\] Longer QuotaPolicy windows (weekly/monthly/yearly) are only listed under 1.2.0's "What's Next"; they haven't shipped on main.\[3\] |
| 6a | Client disconnect mid-stream charged 0; fixed in 1.2.0 | **Partly** | 1.2.0 says usage "is now also recorded when a client disconnects **right after the final SSE frame**". It also fixed duplicate `stream_options` stripping `include_usage`.\[3\] A true mid-stream abort still never receives a usage chunk from vLLM, so it is still uncharged. |
| 6b | Streamed `/v1/completions` without `include_usage` charged 0, still on main | **Unverified** | No issue or PR found. The chat-completions processor injects `stream_options.include_usage=true` when cost metrics are configured (seen in issue #1017).\[11\] Completions-processor behaviour wasn't confirmed. Either way, vLLM `--enable-force-include-usage` neutralizes it on the server side.\[12\] |
| 7 | Need `--enable-prompt-tokens-details`; `--enable-force-include-usage` forces streamed usage; cached includes LMCache; pass via KServe | **Correct, with caveats** | Both flags exist and default to False.\[12\] vLLM computes `num_cached = local + external`, so LMCache/connector hits are included.\[13\]\[14\] Known bugs: zero cached is emitted as null/omitted (#44377); `prompt_tokens_details` always null on some V1 builds (#44961); in P/D mode the decode node reports cached = prompt − 1 (#43370) or = prompt (#47136).\[14\]\[15\]\[16\]\[17\] KServe: set `VLLM_ADDITIONAL_ARGS` on the `main` container, or `args` starting with `"--"` (the default template's `args: ["--"]` swallows the first flag otherwise).\[18\]\[19\] KServe docs found are 0.20, not 0.21. |
| 8 | vLLM 0.31.0 lets a client forge cached count via `kv_transfer_params` (open bug) | **Correct** | Issue #58728: `kv_transfer_params` with `do_remote_prefill` + `remote_prefill_cached_tokens` overwrites `num_cached_tokens` "with no bounds check". The repro gave a 17-token prompt with cached 9999 and also cached −5, and "no P/D setup is required".\[1\] The path came from PR #54222, listed in the **v0.31.0** release notes.\[20\] Fix PR #58730 (clamp to [0, prompt_len]) was **open, unmerged**.\[21\] A secondary source says the v0.30.0 output processor lacks the field.\[22\] |
| 9 | LMCache `cache_salt` per tenant gives per-tenant hit counters and isolation | **Partly wrong** | `cache_salt` is a **vLLM** request parameter, added in PR #17045 as the fix for CVE-2025-46570. That advisory (GHSA-4qjh-9fv9-r85r, published 28 May 2025, CVSS 2.6 Low) affected vLLM <0.9.0, was patched in 0.9.0, and describes an attacker sharing the backend who "could attempt to guess the victim's input. By measuring the TTFT based on prefix matches." It is injected into the first block's hash, so only requests with the same salt share KV; LMCache keys also carry it. It does **not** create per-tenant hit counters: vLLM metrics aren't labelled by salt. Per-tenant hit data comes from the gateway's `cached_input_tokens` per tenant. |
| 10 | Reasoning tokens are inside output; vLLM may or may not fill `reasoning_tokens` | **Correct** | OpenAI: reasoning "is included in output_tokens, itemized … billed at the model's output-token rate".\[7\] vLLM's chat output includes reasoning in `completion_tokens` (e.g. 6,450 completion, of which 4,757 reasoning).\[23\]\[24\] `/v1/responses` reports 0 for text-based parsers (#33512), and one proxy saw reasoning > completion in streams (#24526).\[23\]\[25\] So never add `reasoning_tokens` to the cost. |
| 11 | Output ≈3–8× input; start ~4× | **Plausible range; 5× is the better start** | Public list prices: OpenAI GPT-5.6 Terra $2 in / $12 out (6×); Anthropic Mythos 5.1 $10 / $50 (5×).\[26\]\[27\] Provider prices include margin and their own batching, so measure the ratio on your hardware (method below). |
| 12 | Cached at 10% of input | **Copies providers, overstates the saving** | Anthropic charges 0.1× read plus a 1.25× (5-min) or 2× (1-h) cache *write* premium. At a $3/M base, Flexera (secondary source) puts that at "$3.75 per million to write a 5-minute cache, $6 per million for the 1-hour tier and $0.30 per million to read". Some newer Claude models read even cheaper: 0.05× on Opus 5.5 and 0.025× on Fable 5.1. Newer OpenAI models charge 0.1× cached with no write fee; legacy gpt-4o/4.1 charge 0.5×. OpenRouter lists Gemini at 0.25× and DeepSeek at 0.1× (secondary sources). On your own GPUs a hit skips prefill compute only; cached tokens still occupy KV memory and get attended to on every decode step. **0.2×** is a fairer cost-recovery default until measured. |

### Defects the agent missed (ranked by severity)

1. **On 1.1.0, per-tenant QuotaPolicy buckets are broken.** The 1.2.0 notes say: "With QuotaPolicy Distinct header selectors, the end-of-stream token cost went into one shared bucket instead of the caller's own."\[3\] On 1.1.0, a per-tenant money quota through QuotaPolicy `Distinct` charges everyone's streams to a shared pot. 1.2.0 lists this as a breaking change because tenants "can start receiving 429 at the limits you configured."\[3\]
2. **Cost may never reach the counter at all.** Issue #2248 (v0.7.0) shows the BackendTrafficPolicy counter rising by ≈1 per request instead of the CEL cost on non-streaming requests. The cause is that the response `hits_addend` was read before ext_proc set the metadata.\[28\] That issue was closed "not planned", and the related streaming race (#1786) was intermittent.\[28\] Its status on 1.1/1.2 is unknown, so this must be an acceptance test, not an assumption. Separately, PR #2816 fixed QuotaPolicy adding a request-time hit of 1 (a 28-token response was charged 29).\[29\]
3. **Tenant identity is spoofable if it comes from a client header.** Every example keys buckets on `x-tenant-id`. If a client can set that header, it can bill another department, which is far worse than forging cached tokens. Derive the tenant from authentication (JWT/ext_authz) and overwrite or strip any client-supplied value. The docs note that per-tenant limits read from dynamic metadata "can only be written by filters in the chain, never by the downstream client".\[6\]
4. **Quota enforcement fails open by default.** For QuotaPolicy, `controller.quotaRateLimitFailureModeDeny` defaults to `false`, and a missing `limit.fromMetadata` value silently falls back to the static limit.\[6\]\[9\] Decide fail-open vs fail-closed explicitly.
5. **P/D disaggregation breaks cached counts.** If llm-d runs prefill/decode split, decode nodes before #54222 report near-100% cached tokens (#43370, #47136), so tenants would get a ~80% discount on everything.\[14\]\[15\]\[30\] #54222 fixed that but introduced the forgery.
6. **LMCache correctness bugs can make you charge for wrong answers.** LMCache #5576 (vLLM ≥0.31, V2 model runner) answers full-hit requests from empty KV blocks.\[31\] LMCache #4463 reports silent KV corruption on vLLM 0.26 fused layouts.\[31\]\[32\] A discount on a corrupted answer is the least of the problems, but it shows that "cache hit" ≠ "correct work done".

## Details

### Recommended pricing model

**Unit: dollar-pegged credits, 1 credit = $0.00001 (10 µ$). Display in dollars.**
- **Why credits.** A counter in credits stays meaningful across price changes, because each credit is money already spent. Weighted-token counters silently change meaning when ratios change.
- **Why 10 µ$.** It keeps per-request floor loss at ≤$0.00001 and lifts the uint32 ceiling to **$42,949.67 per tenant+model+window**. If one department could exceed that in a month on one model, use 100 µ$ for that model only, or add daily windows.
- **What tenants see.** Show "$" in the portal and reports. Call the counter unit "credits" only in config, so nobody thinks they are paying a vendor.

**Per-request cost (integer, floor = tenant's favour):**

```
cached   = min(cached_input_tokens, input_tokens)      // clamp forged/garbage counts
uncached = input_tokens - cached
cost_credits = floor( (uncached × P_in + cached × P_cached + output_tokens × P_out) / 1,000,000 )
```

Where `P_*` are integer **credits per million tokens**, set per model.

**Default ratios until measured:** P_cached = 0.2 × P_in; P_out = 5 × P_in; reasoning is counted inside output at the output rate, never on top.
- **Why 0.2×, not the providers' 0.1×.** A hit removes prefill compute, but the cached tokens still hold KV memory for the whole decode and are attended to at every output step. Pricing them at 10% shifts that occupancy cost onto tenants whose prompts weren't cached. On a zero-profit platform, every discount is paid for by someone else.
- **Why 5× output.** It sits mid-range of current list-price ratios (5–6×) and errs toward charging the decode-heavy resource.

**How to set P_in (zero-profit formula):**

```
C_year   = GPU depreciation (capex / 3–4 yrs) + power × PUE + cooling + rack/network/storage share
           + ops staff share + licences (OpenShift / Red Hat AI) + support contracts
H        = C_year / (GPUs × 8,760)                         // fully loaded $/GPU-hour
T_w      = measured "input-equivalent" tokens/sec per GPU at your latency SLO, where
           w = uncached + 0.2·cached + 5·output (use your measured ratios)
U_target = planned utilization (e.g. 0.6)
P_in [$/M]     = H / (3,600 × T_w × U_target) × 1,000,000
P_cached [$/M] = c × P_in ;   P_out [$/M] = r × P_in ;   credits/M = $/M × 100,000
```

Illustrative only: with H = $2.50, T_w = 10,000 w/s and U_target = 0.6, P_in ≈ $0.116/M (11,574 credits/M), P_cached ≈ $0.023/M (2,315), P_out ≈ $0.579/M (57,870).

- **Use target utilization, never actual.** Dividing by actual usage causes the death spiral: low usage raises the price, tenants leave, and the price rises again.
- **Who absorbs idle capacity.** Capacity above target is a central platform cost. The best-effort tier exists to soak it up.
- **Annual true-up.** Under-recovery is absorbed centrally. Over-recovery is credited back next year. That is what makes the hub genuinely non-profit.

**How to measure ratios on your own hardware:** run the vLLM benchmark at your target concurrency with three workloads: long-input/1-output, short-input/long-output, and the first workload repeated with a warm prefix (≈100% hit). Solve for GPU-seconds per uncached input token, per cached input token and per output token. The ratios of those three numbers are c and r. Re-measure whenever you change GPU type, vLLM version, quantization or `max-model-len`.

**Windows:** use BackendTrafficPolicy with **two rules per tenant+model**: a monthly budget, plus a daily rule at roughly ⅓ of the monthly budget. A request is denied when *any* matched limit is exceeded, so a runaway job can't burn the whole month on day 3. QuotaPolicy only supports up to a 1-day window, so don't use it for budgets.\[9\]

**Over-limit and failure semantics (keep the gateway's behaviour, and document it):**
- Usage is checked *before* a request using already-charged usage, and charged after it completes. A request admitted under the limit is served in full even if it overshoots ("1,000-token hourly limit … can stream 1,200 tokens successfully").\[6\]\[33\]
- Failed or refused requests produce no usage, so they charge 0. This is fair.
- **No minimum charge.** A sub-credit request costing <$0.00001 is free. Control spam with a separate request-count rate limit, not a minimum fee that overcharges small calls.

**Cache fairness: charge the actual per-request discount, and make "luck" mostly a tenant's own.**
- The hub only has aggregate counters, so smoothing is impossible anyway, and the per-window sum already averages out luck.
- Prefix-aware routing in llm-d makes hits depend on prefix rather than on which replica a request happens to land on.\[34\]
- **Per-tenant `cache_salt` set by the gateway** means a tenant's discount comes only from its own reuse. That is both fairer and closes the prefix-cache timing side channel (vLLM: "only requests with the same salt can reuse cached KV blocks").\[35\] The channel is real: arXiv 2608.09225 confirms it on Qwen2.5-7B-Instruct / vLLM 0.26.0 / A100 (cold/cached ratio 0.22). But salting is not airtight. CVE-2026-105752 (GHSA-935w-9g4m-p28p, Oct 2026) reports that "Harmony tool continuations drop `cache_salt`", restoring a cross-tenant membership oracle on vLLM ≤0.25.1, and a gateway-set salt does not cover that path.
  - **Cost:** lost cross-tenant sharing. In a departmental setup that is mostly shared apps' system prompts, so salt per *trust domain* (e.g. one salt for the company-wide assistant, one per sensitive department), not per user.
  - **Defer it if needed.** Injecting a per-tenant body field needs a dynamic filter (ext_proc/Lua), so this is phase 2 if it blocks go-live.

**Best-effort tier:** keep it money-free but **metered, capped per tenant per day, and preemptible**. Show its would-be dollar value in showback.
- **Why free.** The marginal cost of idle GPUs is mostly power, which you pay anyway.
- **Why capped.** Unlimited free capacity drowns out the demand signal you need for capacity planning, and lets one team crowd out the others.
- **When to revisit.** If best-effort grows past ~30% of total tokens, add a flex-style rate. Providers' batch/flex tiers are typically 50% off. For Anthropic, a secondary source (chudi.dev, 28 Sep 2026) reports that the pricing page says batch and prompt-caching discounts "can be combined. Batch gives 50% off input and output."

**Long-context tiers:** defer them. The superlinear cost is real, because KV occupancy grows with context × decode time. But a CEL tier like `input_tokens > 128000u ? …` adds a cliff that tenants will game. First publish per-tenant average context length. If a few tenants dominate long context, serve long context as a **separate model deployment with its own measured P_in**. That is simpler and more honest than a price cliff.

**Showback vs chargeback:** start with **showback in dollars** plus enforced credit quotas. The FinOps Foundation's Invoicing & Chargeback capability states: "Showback is always required in any FinOps practice, but chargeback is dependent on organizational accounting policies". Its Chargeback & Finance Integration guidance adds that "neither way should be considered more mature than the other", and chargeback can be unwarranted where costs land on few cost centres. Add chargeback only if Finance wants internal transfers, and settle it annually with the true-up.

**Price changes:** change prices only at a window boundary, announce them ≥1 window ahead, and version the price table in Git with an effective date. Because the counters are money, a mid-window change only alters how many *tokens* the remaining budget buys; it doesn't corrupt the counter. Confirm the window alignment (UTC vs Israel local time) in your rate-limit service before choosing a cut-over hour.

### CEL cost expression (Envoy AI Gateway 1.1.0, illustrative prices)

```yaml
llmRequestCosts:
  - metadataKey: llm_cost_credits          # 1 credit = $0.00001
    type: CEL
    cel: >-
      (
        (cached_input_tokens <= input_tokens ? input_tokens - cached_input_tokens : input_tokens) * 11574u
        + (cached_input_tokens <= input_tokens ? cached_input_tokens : 0u) * 2315u
        + output_tokens * 57870u
      ) / 1000000u
```

- **Integer-only and floor-rounded:** CEL uint division truncates, which is the tenant's favour.
- **Underflow-safe:** subtraction only happens when `cached ≤ input`.
- **Forgery-safe:** if `cached > input`, the request is billed as fully uncached, so lying costs the liar.
- **No overflow:** uint64 intermediates (10^6 tokens × 10^5 credits = 10^11) are far from overflow.
- **Separate prices:** input, cached and output each have their own rate, and reasoning is not added.
- **Per-model prices:** `llmRequestCosts` is route-scoped.\[6\] Either put each model on its own `AIGatewayRoute` with its own expression, or, if `model` is confirmed available in 1.1.0, use `model == "llama-70b" ? (…) : (…)`.
- **Untested:** run the acceptance tests below before trusting any of it.

### Prerequisites checklist

1. **Gateway:**
   - Upgrade Envoy Gateway to v1.9.2 plus the Gateway API v1.6 CRDs, then Agent Router v1.2.0 (upgrade from 1.1.x, not 1.0.x).\[3\]
   - Before upgrading, read EG 1.9's breaking changes (Lua `EnvoyExtensionPolicy` is disabled by default; don't enable `mergeBackends`), the MCP session-seed change, and the stricter `ReferenceGrant to.name` rule.\[3\]
   - Upgrade the Inference Extension CRDs to v1.6.x and **redeploy the llm-d endpoint picker**.\[3\]
2. **vLLM flags:** `--enable-prefix-caching --enable-prompt-tokens-details --enable-force-include-usage`. On KServe, set them via `VLLM_ADDITIONAL_ARGS` on the `main` container, or `args: ["--", …]`.\[18\]\[19\]
3. **vLLM version:** pin **≤0.30.x**, or run 0.31.x only with the #58730 clamp applied. Also confirm the zero-cached and null-details bugs (#44377, #44961) on your exact build.
4. **Strip at the gateway:** remove the request-body `kv_transfer_params` from all client requests. llm-d's own P/D sidecar adds it *downstream* of the gateway, so legitimate use is unaffected.\[36\] Remove or overwrite any client `x-tenant-id`, and set the tenant from auth.
5. **Self-test cached reporting:**
   - Send the same >1k-token prompt twice. Expect cached 0, then cached ≈ prompt − (prompt mod block size).
   - Repeat after evicting the GPU cache, to confirm LMCache hits also appear.
   - With P/D, check that a cold request reports ~0 cached.
6. **Counter test:** for streaming and non-streaming, chat and completions, confirm the Redis counter increments by exactly the CEL value for the right tenant (catches #2248, #1786 and the Distinct bug).
7. **Failure mode:** set fail-open vs fail-closed explicitly, and keep the static fallback limits conservative.

### Risks and failure modes (by severity)

1. **Critical:** tenant header spoofing, which bills another department. Fix it at the auth layer.
2. **Critical:** per-tenant QuotaPolicy costs going into a shared bucket on 1.1.0. Upgrade, or don't use QuotaPolicy Distinct.
3. **Critical:** cost metadata not reaching `hits_addend`, leaving quotas effectively unenforced. Catch it with the counter tests.
4. **High:** forged `cached_tokens` on vLLM 0.31.x. Mitigate with strip + clamp + version pin.
5. **High:** P/D cached-count inflation (pre-#54222), which gives tenants a near-total discount.
6. **Medium:** uncharged mid-stream aborts. These are inherent. Reconcile monthly by comparing vLLM's per-model prompt/generation token counters against the gateway's totals, and absorb the gap centrally.
7. **Medium:** wrong c/r ratios that redistribute cost between tenants. Fix by measuring and re-pricing each period.
8. **Medium:** the death spiral, if prices are ever recomputed from actual usage.
9. **Low:** uint32 ceiling breaches at 10 µ$ (>$42.9k per window). Add monitoring.
10. **Low:** floor rounding leaking <$0.00001 per request, which is accepted by design.

### Copy-paste brief for the coding agent

```
ROLE: Implement cache-aware, zero-profit token cost metering for our internal LLM hub.
Stack: Envoy Gateway + Envoy AI Gateway (target: Agent Router v1.2.0 on EG v1.9.2), KServe, vLLM, LMCache, llm-d.

BUILD NOW
1. Unit: 1 credit = $0.00001. All counters, limits and CEL output are integer credits. UI/reports show dollars (credits / 100000).
2. CEL per model (llmRequestCosts, metadataKey llm_cost_credits), integer-only, floor rounding:
   ((cached_input_tokens <= input_tokens ? input_tokens - cached_input_tokens : input_tokens) * P_IN
    + (cached_input_tokens <= input_tokens ? cached_input_tokens : 0u) * P_CACHED
    + output_tokens * P_OUT) / 1000000u
   P_* = credits per 1M tokens, read from a versioned price table (Git, with effective date).
   Defaults until measured: P_CACHED = 0.2*P_IN, P_OUT = 5*P_IN. If `model` is not a CEL variable
   in our version, generate one AIGatewayRoute per model instead.
3. Enforcement: BackendTrafficPolicy global rate limit, cost.request = Number 0, cost.response = metadata
   io.envoy.ai_gateway/llm_cost_credits. Two rules per tenant+model: unit Month (budget) and unit Day
   (~1/3 of monthly). Separate request-count limit per tenant to stop tiny-request spam.
4. Tenant identity: derive from authenticated identity (JWT/ext_authz); overwrite/strip any client-supplied
   x-tenant-id. Strip request-body field kv_transfer_params from all client requests.
5. vLLM (via KServe main container env VLLM_ADDITIONAL_ARGS, or args starting with "--"):
   --enable-prefix-caching --enable-prompt-tokens-details --enable-force-include-usage.
   Pin vLLM <= 0.30.x unless the #58730 clamp is applied.
6. Best-effort tier: metered in its own counter, cost shown in dollars but not charged, per-tenant daily cap, preemptible.
7. Upgrade path script/runbook: EG v1.9.2 + Gateway API v1.6 CRDs -> Agent Router v1.2.0 (from 1.1.x) ->
   Inference Extension CRDs v1.6.x -> redeploy llm-d endpoint picker. Do NOT enable EG mergeBackends.

DEFER
- Per-tenant cache_salt injection (phase 2; per trust domain, gateway-set only).
- Long-context pricing tiers, priority/flex tiers, weekly windows, chargeback/finance integration.
- Measured c/r ratios (deliver a benchmark script now; apply results next pricing period).

DO NOT
- Do not use `input_tokens + cached_input_tokens / 10u` (charges cached at 110%).
- Do not use doubles in CEL; do not add reasoning_tokens to cost (already inside output_tokens).
- Do not use QuotaPolicy Distinct buckets for tenant money budgets on v1.1.0.
- Do not use units coarser than $0.00001; do not add a minimum per-request charge.
- Do not trust any client-supplied tenant header, cache_salt-as-identity, or kv_transfer_params.
- Do not change prices mid-window.

ACCEPTANCE TESTS (all must pass on the target versions)
A1 Same 2k-token prompt twice: 1st cached=0, 2nd cached>0; Redis counter delta equals the CEL formula exactly, both requests.
A2 Repeat A1 streaming and non-streaming, /v1/chat/completions and /v1/completions, with and without client stream_options.
A3 Request with kv_transfer_params {"do_remote_prefill":true,"remote_prefill_cached_tokens":9999}: field is stripped
   before vLLM; charged as uncached.
A4 Synthetic cached_input_tokens > input_tokens (unit test of CEL): no error, billed fully uncached.
A5 Tenant A sends x-tenant-id: B: request is billed to A (or rejected), never to B.
A6 Two tenants in parallel: each counter only moves for its own traffic (catches shared-bucket bug).
A7 Over-limit: request admitted under limit streams to completion; next request gets 429.
A8 Backend error (5xx) and refused request: counter delta = 0.
A9 Evict GPU prefix cache, re-send: cached>0 via LMCache (if enabled).
A10 Reasoning model: cost uses output_tokens only; reasoning_tokens never added.
A11 Rate-limit service down: behaviour matches the configured fail-open/fail-closed setting.
A12 Monthly reconciliation job: vLLM per-model token counters vs gateway totals; report gap %.
```

## Caveats

- **Version-specific docs.** The CEL variable list and the uint/`u`-suffix rules are quoted from the *Next* (unreleased) QuotaPolicy docs. Confirm them against the 1.1.0/1.2.0 `llmcostcel` source.
- **Unconfirmed items.** The legacy `/v1/completions` streaming gap (claim 6b) is unconfirmed, and so is the status of #1786 on current releases.
- **Version and release status.** KServe guidance comes from 0.20 docs; no 0.21-specific page was found. The vLLM forgery fix (#58730) was unmerged at the time of research; check before pinning. "v0.30.x unaffected" rests on a secondary source.
- **Prices are illustrative.** The $2.50/GPU-hour and 10,000 w/s figures only show the formula. The 0.2× cached and 5× output defaults are reasoned starting points, not measurements, and cited provider price points come partly from third-party summaries of list prices.

## Sources

1. [\[Bug\]: client-supplied kv\_transfer\_params.remote\_prefill\_cached\_tokens can forge prompt\_tokens\_details.cached\_tokens · Issue #58728 · vllm-project/vllm](https://github.com/vllm-project/vllm/issues/58728)
2. [Envoy AI Gateway is becoming Agent Router, an Agentic AI Foundation project](https://theagentrouter.ai/blog/envoy-ai-gateway-is-now-agent-router/)
3. [Agent Router v1.2.x Release Series | Agent Router](https://theagentrouter.ai/release-notes/v1.2/)
4. [Compatibility Matrix | Agent Router](https://theagentrouter.ai/docs/next/compatibility/)
5. [Prerequisites](https://theagentrouter.ai/docs/next/getting-started/prerequisites/)
6. [Usage-based Rate Limiting](https://aigateway.envoyproxy.io/docs/capabilities/traffic/usage-based-ratelimiting/)
7. [How are reasoning tokens, cached tokens, input tokens, and output tokens counted for billing? - API - OpenAI Developer Community](https://community.openai.com/t/how-are-reasoning-tokens-cached-tokens-input-tokens-and-output-tokens-counted-for-billing/1386849)
8. [\[Bugfix\] Add Responses cache\_write\_tokens for API compat and CC parity by yzong-rh · Pull Request #57222 · vllm-project/vllm](https://github.com/vllm-project/vllm/pull/57222)
9. [Quota Policy | Agent Router](https://theagentrouter.ai/docs/next/capabilities/traffic/quota-policy/)
10. [Rate limit service (RLS) (proto) — envoy 1.40.0-dev-079c16 documentation](https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ratelimit/v3/rls.proto)
11. [\[BUG\] modelname override + stream mutation is invalid · Issue #1017 · theagentrouter/agent-router](https://github.com/theagentrouter/agent-router/issues/1017)
12. [Chapter 3. vLLM server usage](https://docs.redhat.com/en/documentation/red_hat_ai_inference_server/3.2/html/vllm_server_arguments/vllm-server-usage_server-arguments)
13. [fix: ask vLLM and SGLang to report cached prompt tokens by kartikey-vyas · Pull Request #27 · ArtificialAnalysis/aa-agentperf-local](https://github.com/ArtificialAnalysis/aa-agentperf-local/pull/27)
14. [\[Bug\]: cached\_tokens always equals prompt\_tokens in disaggregated prefill (P/D) on the decode node · Issue #47136 · vllm-project/vllm](https://github.com/vllm-project/vllm/issues/47136)
15. [prompt\_tokens\_details.cached\_tokens always reports prompt\_tokens - 1 in disaggregated prefill/decode mode · Issue #43370 · vllm-project/vllm](https://github.com/vllm-project/vllm/issues/43370)
16. [\[Bug\]: \[V1\] \`prompt\_tokens\_details\` always \`null\` in API response despite \`--enable-prompt-tokens-details\` (14+ months, incomplete fix in PR #18149) · Issue #44961 · vllm-project/vllm](https://github.com/vllm-project/vllm/issues/44961)
17. [\[Bug\]: --enable-prompt-tokens-details omits zero cached tokens · Issue #44377 · vllm-project/vllm](https://github.com/vllm-project/vllm/issues/44377)
18. [LLMInferenceService Architecture Deep Dive](https://kserve.github.io/website/docs/concepts/architecture/control-plane-llmisvc)
19. [kserve: an LLMInferenceService composed from a preset drops the preset's first argument (the runtime template's args \["--"\] is replaced, so \$0 eats it) · Issue #112 · giantswarm/model-manager](https://github.com/giantswarm/model-manager/issues/112)
20. [Release v0.31.0 · vllm-project/vllm](https://github.com/vllm-project/vllm/releases/tag/v0.31.0)
21. [\[Bugfix\] Clamp remote\_prefill\_cached\_tokens override to \[0, prompt\_len\] by ptimizeroracle · Pull Request #58730 · vllm-project/vllm](https://github.com/vllm-project/vllm/pull/58730)
22. [Clarify vLLM P/D cached-token usage accounting by sernote · Pull Request #41 · sernote/audit-prompt-caching](https://github.com/sernote/audit-prompt-caching/pull/41)
23. [reasoning\_tokens exceeds completion\_tokens in streaming responses from vLLM · Issue #24526 · BerriAI/litellm](https://github.com/BerriAI/litellm/issues/24526)
24. [\`output\_tokens\` in \`llm\_requests\` excludes reasoning tokens on vLLM but includes them on llama.cpp · Issue #4641 · vectorize-io/hindsight](https://github.com/vectorize-io/hindsight/issues/4641)
25. [Responses API reasoning\_tokens always zero for text-based reasoning parsers · Issue #33512 · vllm-project/vllm](https://github.com/vllm-project/vllm/issues/33512)
26. [Prompt Caching: How It Works, Provider Pricing, Cache-Aware Routing (2026)](https://www.morphllm.com/prompt-caching)
27. [Pricing - Claude Platform Docs](https://platform.claude.com/docs/en/about-claude/pricing)
28. [Cost/usage-based rate limiting deterministically under-charges non-streaming responses on v0.7.0 (hits\_addend never populated from cost metadata) · Issue #2248 · theagentrouter/agent-router](https://github.com/theagentrouter/agent-router/issues/2248)
29. [fix: avoid charging request-time quota hit by PatilHrushikesh · Pull Request #2816 · theagentrouter/agent-router](https://github.com/theagentrouter/agent-router/pull/2816)
30. [\[CI\]\[Misc\] mian2main vllm 0928 by zhangxinyuehfad · Pull Request #17870 · vllm-project/vllm-ascend](https://github.com/vllm-project/vllm-ascend/pull/17870)
31. [\[Bug\] vLLM \>= 0.31 (V2 model runner): KV not loaded when the cache hit covers the whole prompt; request answered from empty blocks · Issue #5576 · LMCache/LMCache](https://github.com/LMCache/LMCache/issues/5576)
32. [\[Bug\]\[GPU Connector\] Silent KV cache corruption on vLLM 0.26 fused/packed KV layout · Issue #4463 · LMCache/LMCache](https://github.com/LMCache/LMCache/issues/4463)
33. [Usage-based Rate Limiting](https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/)
34. [Production-Grade LLM Inference at Scale with KServe, llm-d, and vLLM](https://llm-d.ai/blog/production-grade-llm-inference-at-scale-kserve-llm-d-vllm)
35. [Automatic Prefix Caching - vLLM](https://docs.vllm.ai/en/stable/design/prefix_caching/)
36. [Taking vLLM Apart: A Practical Guide to Disaggregated Serving](https://vllm.ai/blog/2026-09-29-disaggregated-serving-guide)
