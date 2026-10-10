# My recommendation: build a cost-aware, non-profit AI platform — not a token billing system

After reviewing your agents’ findings and the architecture of [aigw-ui](https://github.com/roiblum1/aigw-ui), I would make a different decision from the one your agents suggested.

I recommend internal dollar-based cost accounting, combined with capacity-aware quotas, caching discounts, and priority-based access to unused GPU capacity.

Your company owns and operates the infrastructure. You're not an API provider trying to maximize revenue. Your objectives should be:

1. Fairness: Teams should pay, or be allocated, only their reasonable share of infrastructure costs.
2. Efficiency: Unused GPU capacity should be available rather than wasted.
3. Transparency: Every department should understand how its estimated cost was calculated.
4. Predictability: Departments need understandable budgets, without unpredictable charges caused by other tenants.
5. Performance: Cost controls must not undermine latency, throughput, or production workloads.
6. Accountability: Every dollar allocated should be explainable against actual infrastructure expenditure, with no profit margin.

I would not start with an arbitrary price per million tokens and call that the true cost.

That is the biggest weakness in the agents' proposal.

## 1. Dollars, credits, or tokens?

| Option           | Benefits                                                 | Weakness                                    | Verdict                              |
| ---------------- | -------------------------------------------------------- | ------------------------------------------- | ------------------------------------ |
| Raw tokens       | Simple to enforce                                        | Ignores different GPU workloads             | Keep as technical metrics            |
| Weighted credits | Flexible and efficient for quota enforcement             | Users don't know their financial meaning    | Useful internally                    |
| Dollars          | Intuitive for departments, budgeting and reporting       | Requires a defensible cost model            | Primary user-facing unit             |
| GPU-seconds      | Useful for measuring actual infrastructure consumption   | Hard to attribute under concurrent batching | Use for benchmarking and calibration |
| Hybrid           | Combines financial transparency with capacity protection | More engineering work                       | Best architecture                    |

I would expose USD as the default accounting currency, with an option to display your company's preferred financial currency.

But one important distinction: a department's displayed estimated cost does not automatically need to equal the amount internally charged back to that department.

You can show actual consumption, allocate shared costs separately, and decide which costs remain centrally funded.

This is how I'd build a platform intended to serve the company's interests rather than profit from its internal customers.

## 2. The biggest issue your agents missed: a cache hit is not always trustworthy

Your agents correctly identified that vLLM can report cached input tokens, and Envoy AI Gateway can use that number in a cost expression. The v1.1.0 API explicitly supports `cached_input_tokens`, `input_tokens`, `output_tokens`, and `reasoning_tokens`.&#x20;

[image](https://www.google.com/s2/favicons?domain=https://pkg.go.dev\&sz=32)

Go Packages



But I found an important additional problem.

### Prefix-cache reporting can be incorrect with P/D disaggregation

There is an open vLLM issue, [#47136](https://github.com/vllm-project/vllm/issues/47136), describing a disaggregated prefill/decode deployment where the decode node reports every prompt token as cached, even when prefill actually performed the full computation.&#x20;

[image](https://www.google.com/s2/favicons?domain=https://github.com\&sz=32)

GitHub



That matters if you're using, or plan to use, llm-d with disaggregated serving.

If you base financial accounting on that field without testing it, you could seriously undercount actual infrastructure consumption.

I would require the agent to validate the exact vLLM version, KV connector, LMCache integration, and serving topology before using the field for accounting.

### There are actually three different cache situations

| Situation               | What the infrastructure does                                       | Charging approach                                         |
| ----------------------- | ------------------------------------------------------------------ | --------------------------------------------------------- |
| Cache miss              | Computes the prompt prefill and generates the response             | Normal input + output cost                                |
| Prefix KV-cache hit     | Reuses already computed KV blocks, then generates the response     | Discount cached input only                                |
| Full response-cache hit | Returns a previously stored answer without running model inference | No inference charge; account for cache overhead centrally |

The third case is an additional optimization worth investigating, particularly for repetitive internal requests. It is not the same thing as vLLM automatic prefix caching, and it requires explicit response caching.

A KV-cache hit still consumes memory, potentially transfers KV data, and performs decode. Consequently, a cache discount is appropriate, but a zero cost for all cached prompt tokens is not automatically accurate.

### What discount should you use?

I would not hardcode 10% as the permanent policy.

Start with 10% provisionally, but have the agent benchmark cold-cache versus warm-cache requests on representative models and workloads.

Benchmark CPU/GPU memory-cache hits separately from LMCache RAM, disk, or remote-store hits if you use those tiers.

The measured results should determine the policy, with admins able to override it.

Illustrative internal price per 1M tokens — not actual company rates

Uncached input

# $1.00

100% of input rate

Cached input

# $0.10

10% provisional rate

Output

# $4.00

4× illustrative rate

I would separately record cache-hit savings so departments can see when their applications are using the infrastructure efficiently.

Reasoning tokens should normally be included in output cost, not charged again as an additional category. Verify that the model's response usage actually includes them to avoid double counting.

## 3. A critical finding about your exact gateway version

I checked the official release notes for Agent Router 1.2.0. Version 1.2.0 was released on October 7, 2026, three days ago.

It fixes several accounting problems in your installed 1.1.0:

- A `QuotaPolicy` using a `Distinct` header selector could incorrectly put end-of-stream token cost into a shared bucket instead of the individual tenant's bucket.
- Some streaming requests could bypass proper usage accounting.
- Usage metadata could be lost when the client disconnected immediately after the final SSE frame.

These are not just observability issues. They directly affect whether tenant quotas and money-based limits are trustworthy.&#x20;

[image](https://www.google.com/s2/favicons?domain=https://github.com\&sz=32)

GitHub

+1



I would make a staging evaluation of Agent Router 1.2.0 a prerequisite to enforcing financial quotas.

However, do not perform a blind upgrade. Version 1.2.0 also requires compatibility work around Envoy Gateway 1.9.2, Gateway API and Inference Extension versions. Your KServe 0.21.0 and llm-d components need end-to-end compatibility testing first.

There is another important design issue in your own hub: your [optimization documentation](https://github.com/roiblum1/aigw-ui/blob/main/docs/optimization.md) confirms that shared-pool overflow runs at normal priority, whereas best-effort is lower priority. It also shows the best-effort transition relies on periodic Redis polling rather than a synchronous budget decision.&#x20;

[image](https://www.google.com/s2/favicons?domain=https://github.com\&sz=32)

GitHub



For production, I would prefer this policy:

Guaranteed standard budget → opportunistic best-effort overflow → rejection only when capacity or safety limits require it.

Unused capacity should remain accessible, but borrowers should not degrade the service that other departments were promised.

## 4. How I would calculate your company's real AI costs

There are two different costs, and your platform should account for both.

### The internal GPU cost model

Fixed infrastructure cost

Hardware depreciation, maintenance, GPU servers, networking, storage, baseline power and cooling, software licenses, and agreed operational overhead.

Variable operating cost

Additional electricity, storage and network activity, and other measurable incremental expenses.

Cost allocation

Allocate reserved capacity to its owner, attribute shared consumption using benchmark-calibrated rates, and explicitly retain or distribute otherwise unallocated costs according to company policy.

For self-hosted inference, the actual monthly expenditure of the infrastructure does not decrease to zero when nobody sends requests.

Therefore, distinguish:

- Usage cost: A standardized estimate of the resources attributable to a tenant's requests.
- Reserved capacity cost: The cost of capacity held for a team, whether or not it uses it.
- Shared idle capacity cost: Infrastructure expenditure that is not directly attributable to consumption.

I would keep idle-capacity costs centrally funded by default, except where a department explicitly requested dedicated or guaranteed reserved capacity.

This is consistent with FinOps guidance distinguishing transparent showback from actual financial chargeback, and treating shared costs as an explicit allocation decision.&#x20;

[image](https://www.google.com/s2/favicons?domain=https://www.finops.org\&sz=32)

finops.org

+1



### How to establish per-model prices

Have your agent create a benchmarking and cost-calibration module that combines:

1. Actual infrastructure cost per GPU-hour, including amortized shared costs.
2. Model-specific throughput and efficiency, measured under representative concurrency.
3. Uncached prefill versus cached-prefill performance.
4. Decode performance and context length.
5. Memory/storage overhead where significant.

Then use those measurements to establish a fixed rate card per model for an accounting period.

The guiding principle:

Rates should follow measured efficiency, not fluctuate with the number of requests made that day.

If the infrastructure has a quiet month, don't automatically charge the remaining users more. Report the unused capacity as a separate, visible cost.

To ensure there is genuinely no markup, reconcile internal allocations against the actual cost pool at period end, and document any residual cost or adjustment. Do not silently use financial budgets to disguise discrepancies.

## 5. How I would treat each type of customer usage

| Usage scenario                | Internal accounting policy                                                                   |
| ----------------------------- | -------------------------------------------------------------------------------------------- |
| Standard, uncached requests   | Normal model-specific measured rate                                                          |
| Prefix-cache hits             | Discount the cached input portion                                                            |
| Reasoning                     | Charge through output tokens, once                                                           |
| Long context                  | Normal rate initially; benchmark whether complexity justifies future adjustments             |
| Best-effort overflow          | Record estimated consumption; make centrally subsidized or free to the department by default |
| Shared-pool borrowing         | Prefer lower-priority best-effort overflow rather than equal-priority borrowing              |
| Dedicated capacity            | Allocate reservation cost explicitly to its owner                                            |
| Failed / disconnected request | Distinguish infrastructure work actually performed from any department-facing waiver         |
| Full response-cache hit       | Avoid inference charge; separately account for cache infrastructure                          |

Two specific things I would change from the agents' advice:

First: No end-to-end response-time charging. A request that spends 30 seconds waiting for a busy scheduler shouldn't cost more than an equivalent request that starts immediately. But actual active GPU compute time is useful for calibrating the cost model.

Second: Best-effort should have two separate values: estimated resource cost and amount deducted from the department's budget. These can be different. Free usage has an infrastructure cost, even if the central organization chooses to subsidize it.

### An interactive example of cache-aware cost

The following is a policy calculator, using illustrative prices rather than measured infrastructure rates.

Cost calculator

Illustrative rates

Uncached input tokens

Cached input tokens

Output tokens

Input $ / 1M tokens

Output $ / 1M tokens

Cached input cost (% of input)

Estimated usage cost

# $0.008600

Savings from caching

# $0.005400

Cost reduction from caching

38.6%

This calculates metered usage value only. It does not include separately allocated reservations or fixed-capacity costs.

## 6. Exactly what I would tell your coding agent

I would give the agent the following instruction. It is intentionally broader than simply adding dollar fields to the existing UI.

\# Mission: Build a Fair, Cost-Aware Internal AI Platform  We are building an enterprise AI inference platform for our own organization. \*\*Our objective is fair allocation of actual infrastructure costs, not revenue generation or profit.\*\*  Architecture: - Repository: https\://github.com/roiblum1/aigw-ui - Current: Envoy Gateway 1.9.1, Agent Router / Envoy AI Gateway 1.1.0, KServe 0.21.0. - Multi-cluster OpenShift architecture. - Existing per-tenant quotas, shared pool, and best-effort serving. - vLLM, LMCache, and llm-d capabilities must be verified against our actual deployed versions and topology.  I want a production-quality implementation, not a superficial UI change.  ## 1. Begin with a parallel technical investigation  Launch independent research agents covering:  1. \*\*Gateway accounting:\*\* Inspect Agent Router 1.1.0 source for QuotaPolicy, CEL calculations, stream usage, Distinct selectors, tenant isolation, and Redis counters. Investigate the 1.2.0 fixes, migration requirements, and compatibility with our stack. 2. \*\*vLLM and caching:\*\* Inspect real cached-token reporting for GPU KV cache, LMCache, distributed cache, and prefill/decode disaggregation. Identify inaccurate metrics and client-spoofing risks. Research response caching separately. 3. \*\*Hub architecture:\*\* Trace every model price, quota, counter, tenant API, UI, self-test, synchronization, and shared-pool/best-effort code path. Identify migration risks and double counting. 4. \*\*Infrastructure economics:\*\* Design a cost model based on actual hardware, deployment, operational expenditure, model throughput, utilization, and benchmarked performance. 5. \*\*Scheduling and fairness:\*\* Verify best-effort priority enforcement, shared-pool behavior, borrowing, tenant starvation, load shedding, and whether excess traffic can degrade guaranteed workloads. 6. \*\*FinOps and security:\*\* Review transparent showback, no-markup reconciliation, cache isolation, financial auditability, and cost-accounting observability.  Run the investigations in parallel where possible. Do not rely solely on documentation. Inspect upstream source and produce evidence for important technical claims.  ## 2. Design the cost model  Implement an admin-configurable, versioned price catalog per model.  Each model must support: - Uncached input cost per million tokens. - Cached input cost per million tokens. - Output cost per million tokens. - Optional infrastructure-cost and deployment profile. - Currency, pricing version, effective date, and measurement methodology. - A mode for models without trustworthy price or usage data.  Never silently assign arbitrary prices to unconfigured models.  Implement:  Usage cost = (uncached input × input rate + cached input × cached rate + output × output rate) / 1,000,000.  Ensure reasoning tokens are not double-counted.  Guard against cached tokens exceeding input tokens.  Cache creation, offloaded-cache retrieval, long-context overhead, and multimodal inference must be considered explicitly when supported and measurable.  Use benchmark-calibrated rates. Treat cached input at 10% and output at 4× the normal input rate only as configurable illustrative starting points, never as claims about actual infrastructure cost.  ## 3. Separate cost from charging policy  Implement three independently visible concepts:  - Estimated resource consumption cost. - Department budget consumption. - Actual centrally funded or allocated infrastructure cost.  A user may consume resources without having their department's budget debited, such as when using subsidized best-effort capacity.  Account for reserved-capacity ownership separately.  Allow company-wide reconciliation against actual operating expenditure. Never introduce an implicit profit margin.  Default to showback rather than financial chargeback until Finance approves an allocation policy.  ## 4. Build reliable usage accounting  Use the gateway for lightweight, synchronous quota enforcement whenever the supported gateway version permits it.  Implement a separate historical accounting/reporting pipeline outside the LLM request path.  Include: - Tenant, model, serving site, request class and price version. - Input, cached input, uncached input, output and reasoning tokens where valid. - Monetary value, department budget deduction and cache savings. - Request outcome, streaming completion state and accounting confidence. - Cross-cluster deduplication and retries. - Daily and monthly history and aggregation.  Preserve exact or high-precision financial values in the accounting ledger. Do not rely exclusively on rounded gateway counters as the financial source of truth.  Check all integer ranges and quota limits in source. Test fixed-point arithmetic, fractional rounding, very small requests, overflow, and changing rates.  When usage is missing or untrustworthy, expose the accounting uncertainty. Do not fabricate a cache discount or silently count a request as free.  Do not log raw prompts, responses or sensitive content as part of the billing system.  ## 5. Implement cache-aware accounting securely  Verify cached-token metrics end-to-end for each actual vLLM deployment.  Specifically investigate vLLM issue #47136, the disaggregated prefill reporting problem.  Validate streaming and non-streaming usage.  Implement tenant-scoped cache isolation as appropriate. Use gateway-controlled cache identifiers or salts; do not trust arbitrary client-provided identifiers for cross-tenant cache access.  Do not assume that prefix caching means a cached response.  Measure and report how much prefill computation is avoided and what overhead remains.  If cached-token accounting is inaccurate for a serving topology, disable financial enforcement of the inaccurate discount and report the limitation rather than presenting misleading amounts.  Investigate full-response caching as a separate, optional future feature with security, correctness and freshness requirements.  ## 6. Preserve and improve fairness  Keep standard service, best-effort overflow, and shared-pool functionality.  Review whether equal-priority shared-pool borrowing can interfere with other tenants.  Our preferred policy is: - Standard allocation for departments within their budgets. - Opportunistic best-effort overflow when budget is exhausted. - Immediate protection of normal-priority traffic under congestion. - Separate observability of free/subsidized usage. - Fair sharing among best-effort tenants, including concurrency and throughput controls.  Investigate the current 90% quota transition and polling delay. Avoid moving legitimate normal-priority traffic to best-effort prematurely.  Do not call a quota a guaranteed service level unless scheduler capacity or admission controls really enforce that guarantee.  ## 7. Improve the UI  Make dollars the default unit presented to users. Retain tokens as secondary technical metrics.  Build:  \*\*Department view\*\* - Current estimated cost. - Budget used and remaining. - Normal versus subsidized consumption. - Cache savings and efficiency. - Usage by model. - Historical trends. - Alerts before budget exhaustion.  \*\*Administrator view\*\* - Model rate cards. - Effective-date pricing management. - Cost calibration and benchmark results. - Actual infrastructure expenditure. - Cost reconciliation and unallocated capacity. - Cached-token reporting health. - Tenant and cluster accounting anomalies. - Audit history and export.  All changes must be backward compatible with existing token quotas or provide an explicit, safe migration.  ## 8. Correctness and compatibility are release gates  Investigate the accounting defects fixed by Agent Router 1.2.0.  Prepare a compatibility and staged-upgrade plan for Envoy Gateway, Agent Router, KServe, llm-d, InferencePool, and relevant CRDs.  Do not upgrade production automatically.  Test: - Cache hit/miss accounting. - Reasoning tokens. - Missing usage details. - Streaming, aborted streams and retries. - Client spoofing attempts. - Multiple tenants, clusters and gateway hops. - Shared-pool and best-effort interactions. - Price changes during an active quota window. - Quota enforcement, reset and rollover. - Correct tenant attribution. - Load-related latency regression and starvation. - Financial reconciliation.  Unit tests alone are insufficient. Include real gateway integration tests and a staging validation plan.  ## 9. Execution and deliverables  Implement this in separately reviewable phases:  1. Accounting and compatibility correctness. 2. Historical usage ledger and read-only dollar showback. 3. Verified per-model pricing and monetary budget enforcement. 4. Cache savings, fairness improvements, and FinOps reporting. 5. Optional advanced features such as full-response caching and automated cost calibration.  First provide the architecture, the important findings, the identified risks, and the proposed rollout.  Then implement the changes as reviewable PRs with tests, documentation, database migrations, and deployment guidance.  \*\*Do not merge, deploy, or upgrade production without explicit approval.\*\*  Keep the existing quota editor bugfix PR #13 separate.  Success means: - Correct, reproducible accounting. - No implicit markup. - No tenant paying for another tenant's congestion or centrally chosen idle capacity. - Real savings when computation is avoided. - Better GPU utilization without sacrificing service fairness. - No added synchronous database dependency or unnecessary gateway request latency. - Administrators can explain every department's allocated cost.  Build a platform that optimizes total company value, not billable token volume.

## 7. What I would prioritize

First: trustworthy accounting

Fix or establish safe handling of the 1.1.0 gateway accounting issues. Verify per-tenant attribution and cached-token accuracy. Until this works, I wouldn't enforce monetary budgets.

Second: dollar showback and cost calibration

Build pricing, historical usage and transparent reporting. Run it in shadow mode and compare estimates against measured infrastructure costs.

Third: financial quotas with fair overflow

Implement monetary budgets while retaining separate operational rate limits. Use best-effort to make idle capacity available safely.

Fourth: optimization

Measure cache savings, improve prefix locality, investigate response caching, and continuously calibrate model cost and GPU efficiency.

One final point: I would not make monthly budgets another real-time gateway counter. Agent Router 1.1.0's available quota windows end at one day, and longer windows remain roadmap work in 1.2.0.&#x20;

[image](https://www.google.com/s2/favicons?domain=https://pkg.go.dev\&sz=32)

Go Packages

+1

&#x20;Use a historical ledger for monthly budgeting, and keep gateway counters responsible for the shorter windows they can enforce reliably.



### My overall conclusion

You're building a shared internal AI infrastructure service. The most important economic measure isn't how much money each department consumes. It's how much useful AI work your company gets from each dollar spent on GPU infrastructure, while providing departments fair access.

That makes three things especially valuable: accurate cache discounts, subsidized use of genuinely spare capacity, and transparent allocation of the costs that cannot fairly be attributed to individual requests.

I would rather build those correctly than release dollar-denominated quotas quickly on top of accounting we haven't yet proven.

If you want, I can:

- Summarize this conversation
- Advise the agent on best pricing strategy for a no-profit internal company AI platform
- Explain the tradeoffs between tokens, credits, and dollar-based pricing for internal AI cost allocation
