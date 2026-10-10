# Architecting Zero-Profit, Cost-Recovery Enterprise AI Infrastructure: A FinOps and Systems Engineering Report

The transition of generative artificial intelligence from experimental silos into centralized, multi-tenant enterprise infrastructure demands a fundamental reconfiguration of operational economics. When an organization provisions large language models (LLMs) to internal departments or partner entities without a profit motive, traditional software-as-a-service billing paradigms become actively detrimental. The objective shifts from margin maximization to precise, mathematically sound cost recovery. This necessitates abandoning abstract metrics such as usage tokens or arbitrary credits in favor of deterministic financial metrics—specifically, microdollars—mapped directly to the underlying hardware utilization.

This comprehensive analysis architects a zero-profit, cost-recovery model utilizing Envoy AI Gateway 1.1.0, KServe 0.21.0, and the vLLM inference engine. The architecture resolves the complex interplay between prefix caching discounts, asymmetric input and output compute costs, streaming telemetry gaps, and best-effort queueing priorities. Furthermore, it establishes the precise declarative configurations required to guide deployment agents in constructing an infrastructure that is secure, financially equitable, and operationally transparent.

## The Economic Philosophy of Zero-Profit AI Provisioning

In a zero-profit model, the infrastructure operates as a shared utility. The primary directive of financial operations (FinOps) is to present consumption to the end-user or internal department as a "share of cost" rather than a commercial invoice. Displaying costs in abstract credits obscures the financial reality of the hardware lease, preventing departments from optimizing their specific workloads. Conversely, displaying costs in standard dollars can invoke unnecessary vendor-client friction or trigger excessive financial scrutiny for micro-transactions. Because the overarching operational windows are short—typically daily quotas—and the explicit goal is exact cost recovery, microdollars emerge as the optimal unit of account for the underlying policy engine.

### The Fallacy of Uniform Token Pricing

Historically, commercial AI platforms have charged a flat rate per token, treating all tokens as computationally equal. This paradigm is fundamentally misaligned with the physics of graphics processing unit (GPU) computation and creates severe cross-tenant inequities in a shared cluster.

In autoregressive models, the compute profile is highly asymmetric. The prefill phase, which processes the input prompt, operates in a compute-bound regime. It leverages massive parallel matrix multiplications across the GPU's streaming multiprocessors, processing hundreds or thousands of tokens simultaneously. Conversely, the decode phase, which generates the output tokens, is inherently memory-bandwidth bound. The system must load the entire Key-Value (KV) cache for the context window into the GPU registers for every single generated token, a sequential bottleneck that dictates the maximum generation speed.

Empirical hardware profiling demonstrates that generating an output token consumes roughly three to eight times the GPU time of processing an input token, depending on the specific model architecture, quantization levels (e.g., INT4 versus FP16), and the underlying hardware tier. A flat pricing model creates a cross-subsidization failure. A tenant submitting extensive documents and receiving brief analytical answers, utilizing low-cost prefill, financially subsidizes a tenant submitting brief prompts and generating extensive code or prose, which heavily taxes the decode memory bandwidth.

To achieve zero-profit equity, the pricing model must mathematically unbundle input and output costs. A recommended baseline ratio is one-to-four, where output tokens are priced at four times the rate of input tokens. Furthermore, reasoning tokens—surfaced in models utilizing chain-of-thought paradigms—are computationally indistinguishable from standard output tokens. They require the exact same autoregressive generation cycles and KV cache memory updates. Consequently, reasoning tokens must be priced identically to standard output tokens, avoiding arbitrary surcharges that commercial providers often leverage for margin inflation.

### The Denominator Problem and Idle Capacity Allocation

A core challenge in establishing a zero-profit infrastructure is the "denominator problem." The true cost of a generated token is the total monthly lease cost of the GPU cluster divided by the total number of tokens generated in that month. If the infrastructure experiences a quiet month with low tenant utilization, the denominator shrinks, and the true cost per token mathematically spikes.

Passing this spiked cost dynamically to the active tenants creates a negative feedback loop. Higher prices drive lower usage, which shrinks the denominator further, driving prices even higher. This destabilizes departmental budgeting and discourages platform adoption.

The FinOps resolution to the denominator problem is to price the compute at a fixed target utilization rate. Standard enterprise infrastructure typically targets a 60% to 80% utilization band. If a GPU cluster costs a fixed amount per month and is capable of generating a specific volume of tokens at 70% utilization, the internal rate is locked based on that volume. If actual utilization drops to 40%, the unrecovered idle cost is absorbed centrally as an infrastructure availability overhead, rather than unjustly penalizing the tenants who utilized the system during that period.

### Goodput Economics and Quality Gates

When evaluating the financial efficiency of the system, platform engineering teams must shift their telemetry from measuring raw throughput, defined as total tokens generated per second, to Goodput, defined as accepted results per second. Goodput represents the strict subset of requests that meet the required Time-To-First-Token (TTFT) and Time-Per-Output-Token (TPOT) Service Level Objectives (SLOs) without failing quality gates or requiring client-side retries.

A cluster that routes traffic inefficiently or operates under severe memory pressure may show high raw throughput but abysmal Goodput, resulting in a higher loaded cost per accepted result (LCPR). Quality failures at the model level burn GPU budgets invisibly. An output that passes the latency SLO but fails the quality gate consumes its entire inference budget for nothing. By leveraging KServe's priority queues and Envoy's rate-limiting, the system shields the Goodput of standard-tier tenants from the chaotic bursts of competing workloads, ensuring the zero-profit price point remains viable.

## Architecting the Microdollar Cost-Recovery Mechanism

Implementing this economic philosophy requires mapping physical hardware constraints to the logical constructs of the Envoy AI Gateway 1.1.0 cost expression engine. The gateway relies on the Common Expression Language (CEL) to evaluate the payload of upstream AI model responses and dynamically calculate the exact cost of the transaction before deducting it from a distributed state store.

### Microdollar Quota Limitations and Windowing Strategy

The decision to utilize microdollars directly interfaces with the mathematical limits of the Envoy AI Gateway. In version 1.1.0, the gateway relies on a distributed Redis-backed rate limit counter to track tenant consumption. A critical limitation of this architecture is the 32-bit unsigned integer ceiling of its internal counters, which caps at precisely 4,294,967,295.

If the system tracks budgets in microdollars, this integer ceiling creates a maximum quota of approximately $4,294.96 per time window. For enterprise applications, a monthly window of $4,294 is easily exhausted by a moderately active department or a suite of automated agentic workflows. Attempting to assign a larger budget results in a silent integer overflow, resetting the counter and granting the tenant unlimited free access until the next window.

The architectural solution is to configure the quota windows to reset daily rather than monthly. A daily limit of $4,294 provides an ample operational ceiling, equivalent to over $128,000 in monthly compute allocation, while safely remaining below the overflow threshold. Daily windows also provide superior blast-radius containment for internal security and budget management. If a tenant's internal agent enters a runaway loop due to a logic error, the financial damage is strictly capped to a single day's budget, rather than exhausting an entire quarter's allocation in hours.

### Prefix Caching and the Mathematics of Memory Holding

Prefix caching, frequently referred to as context caching, allows the vLLM backend to retain the KV cache states of previously processed prompt segments. These segments often include extensive system instructions, few-shot examples, large codebase contexts, or massive document embeddings. When a subsequent request shares a prefix with a cached entry, the prefill phase bypasses the redundant matrix multiplications, loading the state directly from memory.

However, a cache hit is not entirely free, and offering it at zero cost violates the zero-profit recovery mandate. Retaining the KV cache consumes substantial High Bandwidth Memory (HBM) on the GPU. Modern LLM inference engines manage this memory at the granularity of blocks or pages, which are typically sized for 16 tokens. Maintaining gigabytes of cached context displaces other potential tenants and reduces the maximum batch size the scheduler can maintain for concurrent decode operations. Therefore, while the compute cost drops to near-zero, the memory lease cost remains constant.

The architectural recommendation is to charge cached input tokens at a 10% to 20% fraction of the standard input token rate. This accurately reflects the HBM holding cost while passing the vast majority of the compute savings directly back to the tenant. It is critical to note that writing to the cache should never incur a surcharge. The computational cost of generating the KV cache during the initial prefill is naturally absorbed into the standard input processing fee, and penalizing the tenant for the initial write discourages the use of efficient, cacheable prompt structures.

### Correcting the Cost Expression Vulnerability

A critical vulnerability exists in the naive implementation of token-based cost reduction within Envoy Gateway. Upstream model servers, including vLLM, report token usage in a structured JSON format, detailing `prompt_tokens` and nesting `cached_tokens` within a `prompt_tokens_details` object.

Crucially, the `prompt_tokens` integer represents the total length of the prompt, inclusive of the cached portion. If an Envoy Gateway CEL cost expression simply adds the fields, as demonstrated in early and flawed documentation examples, the mathematics fail. An expression configured as `input_tokens + (cached_input_tokens / 10)` effectively charges the user 100% of the input cost plus a 10% penalty for the cache hit, resulting in a 110% charge rather than a discount.

The CEL cost expression implemented in Envoy AI Gateway 1.1.0 must explicitly subtract the cached tokens from the total prompt tokens before applying the respective price multipliers. Because CEL executes strict type checking, and the Envoy Gateway rejects decimal arithmetic in its rate limit counters, the entire expression must be mapped using whole-number microdollar multipliers, ensuring fractional divisions do not crash the evaluation engine.

To illustrate the necessary complexity, the evaluation must calculate the effective uncached input by subtracting the `usage.prompt_tokens_details.cached_tokens` from the `usage.prompt_tokens`. It must then multiply the uncached input by the standard prefill microdollar rate, multiply the cached input by the discounted HBM holding rate, and finally multiply the `usage.completion_tokens` by the premium decode rate. This ensures a mathematically flawless zero-profit recovery.

| Token Classification | Compute Phase | Hardware Constraint | Recommended Pricing Multiplier |
|---|---|---|---|
| Uncached Input | Prefill | GPU Compute (Matrix Math) | Base Rate (1x) |
| Cached Input | Bypass | GPU HBM (Memory Holding) | Discount Rate (0.1x) |
| Standard Output | Decode | GPU Memory Bandwidth | Premium Rate (4x) |
| Reasoning Output | Decode | GPU Memory Bandwidth | Premium Rate (4x) |

## Deep-Dive: Inference Engine Telemetry and vLLM Configuration

The Envoy AI Gateway operates purely as a policy enforcement and routing layer; it possesses no inherent knowledge of the GPU's internal state. It relies entirely on the telemetry emitted by the downstream inference engine. In this architecture, vLLM serves as the high-throughput backend, utilizing its PagedAttention mechanism to manage the KV cache dynamically. Configuring vLLM correctly is the linchpin of the FinOps architecture; if vLLM fails to report the precise physical execution metrics, Envoy cannot calculate the accurate cost.

### Telemetry Capture for Streaming Responses

One of the most pervasive data-loss vulnerabilities in modern LLM infrastructure involves Server-Sent Events (SSE) streaming. When a client requests a streamed response to achieve low latency, standard model servers stream the generated text chunks continuously. Historically, these streams omit the final usage telemetry block to save bandwidth and reduce parsing complexity. Consequently, the Envoy gateway logs zero tokens, executes a zero-cost expression, and deducts nothing from the tenant's budget.

Furthermore, in Envoy AI Gateway 1.1.0, a specific edge case exists regarding client termination. If a client prematurely disconnects before the stream completes, the gateway defaults to a zero charge because the final telemetry chunk was never received or parsed. This allows tenants to systematically evade cost recovery by deliberately severing the connection milliseconds before the final stop token is generated.

To enforce exact cost recovery, the gateway must be supplied with guaranteed usage data regardless of the streaming protocol. This necessitates strict backend enforcement at the vLLM layer, compelling the engine to append the usage object to the final SSE chunk, even if the connection is interrupted.

### Required vLLM Startup Parameters

The vLLM entrypoint must be initialized with two critical flags that are disabled by default. These flags bridge the gap between the engine's internal memory management and the gateway's financial telemetry requirements.

The first required parameter is `--enable-prompt-tokens-details`. When this flag is omitted, vLLM silently absorbs cache hits, reporting only the total prompt length. By forcing this flag to true, the engine decomposes the input tokens into their cached and uncached physical components, exposing the `usage.prompt_tokens_details.cached_tokens` integer strictly required for the 10% HBM memory discount.

The second required parameter is `--enable-force-include-usage`. This flag must be globally enforced across all worker nodes. It guarantees that every response, whether a discrete JSON object or a chunked SSE stream, concludes with a comprehensive usage report. Without this flag, streamed requests bypass the Envoy Gateway's cost expression entirely, rendering the cost-recovery model mathematically invalid and allowing massive budgetary leaks.

### Multi-Tenant Cache Isolation Versus Efficiency

A critical architectural decision when utilizing vLLM behind KServe is the degree of multi-tenant isolation applied to the prefix cache. When multiple tenants share the same vLLM process, the engine natively attempts to deduplicate identical prompts across the entire active memory pool to maximize cache efficiency and reduce compute waste.

However, this creates a potential cryptographic side-channel vulnerability. If a tenant submits a prompt containing a highly specific, proprietary system instruction, and a second tenant submits the identical instruction, the second tenant will experience an unusually fast TTFT due to the first tenant's prior cache population. By measuring TTFT variances, malicious actors can infer the presence of specific prompts in the shared memory space.

While this level of risk is generally acceptable in internal, zero-profit corporate environments where all tenants belong to the same organizational entity, it must be carefully considered. If strict departmental separation is required for compliance or data sovereignty, the platform must utilize vLLM's ability to salt the prefix cache hash with the tenant ID. This ensures one department's cache cannot be hit by another, trading a slight reduction in global memory efficiency for mathematically guaranteed isolation.

## Multi-Tenant Queueing and Best-Effort Tiering via KServe

KServe 0.21.0 provides the Kubernetes-native orchestration layer, managing the lifecycle of the vLLM pods and routing traffic via the Envoy proxy mesh. In this architecture, KServe's InferenceService Custom Resource Definition (CRD) is utilized to map underlying physical GPU resources to logical model endpoints, abstracting the hardware complexity away from the gateway.

### The Economics of the Best-Effort Tier

In a multi-tenant environment, traffic patterns are inherently bursty. Provisioning for peak load guarantees that GPUs sit idle during troughs, exacerbating the denominator problem discussed earlier. To maximize the return on hardware investments and lower the baseline token cost for all users, the infrastructure must support a best-effort service tier.

Best-effort traffic utilizes spare GPU cycles that would otherwise go entirely to waste. These workloads typically consist of asynchronous batch processing, large-scale document summarization, offline embeddings generation, or synthetic data creation. When the cluster is idle, best-effort requests are processed immediately. When the cluster is under heavy interactive load, best-effort requests are aggressively queued, yielding to high-priority, latency-sensitive traffic.

Because best-effort traffic strictly consumes stranded capacity, it fundamentally costs the central infrastructure nothing extra to execute; the lease on the GPU is already a sunk cost. Consequently, best-effort requests should be routed at a near-zero microdollar rate, or entirely free, recorded strictly for telemetry and abuse prevention rather than budget deduction. This incentivizes departments to shift non-critical workloads to off-peak hours, flattening the utilization curve.

### Implementing Priority-Based Routing

To implement this tiered strategy, KServe and the Envoy front-end must be configured to respect request priority headers. Standard interactive requests, such as human-in-the-loop chat interfaces, contain a standard priority HTTP header. Best-effort workloads are tagged by the client or the gateway with a lower priority marker.

When the cluster is operating below the queue threshold—for example, less than 70% of maximum prefill capacity—both standard and best-effort requests are forwarded to vLLM via standard First-Come-First-Served (FCFS) logic. However, when the system experiences heavy load, the KServe router queue engages. The priority hints dictate that standard requests physically bypass best-effort requests in the pending queue. This ensures that the TTFT SLOs of interactive applications remain uncompromised, while the best-effort requests quietly soak up the idle capacity during subsequent lulls.

This is configured within the KServe frontend using the `--router-queue-threshold` parameter to define the trigger point, and the `--router-queue-policy` set to strict priority rather than standard FCFS.

| Routing Tier | Workload Profile | Header Configuration | Queue Priority | Recommended Pricing |
|---|---|---|---|---|
| Standard | Interactive Chat, Real-time Agents | Default | High (Bypass enabled) | Full Cost Recovery |
| Best-Effort | Batch Summarization, Offline Eval | `x-priority: low` | Low (Yields to High) | Zero or Near-Zero |

## Security Imperatives and Vulnerability Management

When operating a multi-tenant vLLM cluster behind a unified gateway, platform stability is paramount. A zero-profit model relies entirely on high, uninterrupted Goodput to maintain low token costs. If the cluster crashes, active requests fail, GPU cycles are wasted, and the loaded cost per accepted result artificially inflates.

The research material highlights critical Common Vulnerabilities and Exposures (CVEs) affecting vLLM versions prior to 0.29.0, which pose an existential threat to multi-tenant stability. Specifically, vulnerabilities involving the `kv_transfer_params` dictionary—tracked under CVE-2026-94622 and CVE-2026-94626—expose the inference engine to severe Denial of Service (DoS) attacks.

An unauthenticated or authenticated tenant can submit malformed requests with incomplete, out-of-bounds, or deliberately manipulated `kv_transfer_params`. These parameters are intended for advanced KV cache movement between nodes in disaggregated architectures. Because the vLLM EngineCore scheduler lacks robust validation for these dictionary entries in older versions, the malformed payload triggers an uncaught `KeyError` or an unbounded memory allocation routine.

This error immediately crashes the decode worker process, inducing a kernel Out-Of-Memory (OOM) kill. The catastrophic failure halts inference for all tenants sharing the KServe InferenceService, regardless of whether they initiated the malformed request.

To secure the zero-profit infrastructure, the platform team must ensure that the vLLM container image is strictly pinned to version 0.29.0 or higher. If organizational constraints prevent upgrading the container image, the platform engineering team must explicitly implement admission control webhooks in KServe, or utilize an Envoy Lua filter, to aggressively strip the `kv_transfer_params` object from all inbound JSON payloads before they traverse the mesh to reach the vLLM pod.

## Actionable Directives for Autonomous Configuration Agents

Based on the exhaustive analysis of FinOps logic, gateway constraints, serving engine telemetry, and security imperatives, the following technical directives must be transmitted to the autonomous deployment agents. These instructions dictate the exact parameters required to construct the infrastructure, ensuring no requirements are missed.

### Pricing and Unit Directives

The agent must configure the infrastructure to abandon token counting in favor of microdollars. The base unit of account must be defined such that 1,000,000 microdollars equals $1.00 USD. The agent must enforce daily quota windows across all internal tenants, setting a hard ceiling of 4.2 billion microdollars per tenant, per model, per day. This strictly avoids the Envoy AI Gateway 1.1.0 32-bit integer overflow bug while providing ample operational runway.

The agent must implement a multi-dimensional pricing matrix rather than a flat token rate. It must define separate integer variables within the configuration state for input price, output price, and cached price. The output price must be mathematically configured to be approximately four times the input price, reflecting memory bandwidth constraints. The cached price must be set at exactly 10% of the input price, reflecting the HBM holding costs.

### Gateway and Cost Expression Directives

The agent must discard the flawed documentation example for cost calculation. It must deploy a corrected logical CEL structure into the Envoy AI Gateway configuration. The expression must execute whole-number microdollar arithmetic and include strict guards against negative results.

The agent must construct the CEL logic as follows:

1. Extract the total input from `usage.prompt_tokens`.
2. Extract the cached input from `usage.prompt_tokens_details.cached_tokens`, defaulting to zero if the field is null.
3. Extract the output from `usage.completion_tokens`.

The final mathematical execution must be: `((total_input - cached_input) * price_input) + (cached_input * price_cached) + (output * price_output)`. The agent must apply a floor function to round down any fractional division, ensuring the central infrastructure never accidentally profits from rounding errors.

### Engine and Telemetry Directives

The agent must inject mandatory arguments into the KServe ClusterServingRuntime or InferenceService deployment manifests for all vLLM containers. It must append `--enable-prompt-tokens-details` to expose the cached memory metrics to the gateway. It must also append `--enable-force-include-usage` to guarantee that SSE streaming telemetry is appended to the final chunk, preventing streaming cost evasion.

The agent must execute a version check on the target vLLM container image. It must verify that the image is pinned to version 0.29.0 or higher. If the version is lower, the agent must automatically generate and deploy an Envoy Lua filter to forcefully strip `kv_transfer_params` from all incoming JSON payloads, neutralizing the known CVE-2026-94622 and CVE-2026-94626 vulnerabilities.

### Routing and KServe Directives

The agent must create a routing rule in the Envoy proxy that maps specific workloads containing an `x-best-effort` header to a near-zero pricing tier. It must enable the `--router-queue-threshold` and `--router-queue-policy` within the KServe frontend environment variables. The policy must be set to strict priority, ensuring requests tagged with the best-effort header yield to standard interactive requests during queue states, allowing the infrastructure to safely monetize idle GPU capacity without degrading Goodput for core workloads.

## Strategic Recommendations for the Enterprise

For an enterprise acting as an internal, zero-profit service provider, the strategy extends beyond code and into organizational policy. To answer the imperative of "what is best for the company," the architecture must be paired with operational transparency.

1. **Mandate shared pools rather than dedicated hardware.** Provisioning a dedicated GPU for each department guarantees privacy but devastates utilization rates, driving the baseline cost per token unacceptably high. By utilizing in-process tagging, where multiple tenants share the same vLLM process and GPU, the denominator problem is mitigated. The gateway simply injects a tenant ID into the HTTP headers, which is propagated through KServe to vLLM, ensuring telemetry is correctly attributed without partitioning the hardware.

2. **Decouple raw billing from telemetry reporting.** Because the Envoy gateway only updates a single total microdollar counter per tenant in the Redis backend, the gateway itself cannot show a department how much money they saved through efficient caching. The platform engineering team must build a secondary observability dashboard, combining the `gen_ai_client_token_usage_token_sum` metric from Envoy with the `cached_tokens` histogram from vLLM. This allows the FinOps team to present a comprehensive breakdown report to department heads, proving the value of the zero-profit model and illustrating exactly how prefix caching reduced their internal share of the costs.

3. **Socialize the concept of target utilization.** Departments must understand that their microdollar rate is based on an assumption of a busy cluster. If a department anticipates a massive spike in usage, or a prolonged quiet period, they must communicate this to the platform team so the target utilization baseline can be adjusted, preventing unexpected budgetary shocks at the end of the fiscal quarter.

## Future Outlook and Scaling Considerations

As the enterprise AI infrastructure matures, the convergence of Envoy, KServe, and vLLM will support increasingly sophisticated traffic management. Currently, prefix caching relies entirely on luck and standard load balancing; a cache hit only occurs if the KServe router happens to send the request to the specific pod that previously processed the prompt.

Future iterations of the architecture should explore prefix-aware routing. By analyzing the prompt hash at the Envoy layer before the request reaches KServe, the gateway could deterministically route the request to the exact vLLM replica holding the corresponding KV cache in its HBM. This would dramatically increase the cache hit rate, maximizing the impact of the 10% discount policy and driving the effective cost of contextual inference closer to zero.

Furthermore, as the Envoy AI Gateway ecosystem evolves beyond version 1.1.0, native support for 64-bit integer counters will likely be introduced, removing the $4,295 ceiling constraint. This will allow the FinOps team to transition from daily operational windows back to traditional monthly budgeting cycles, reducing administrative overhead while maintaining the mathematical precision of the microdollar framework.

## Conclusion

A zero-profit, cost-recovery AI infrastructure requires a paradigm shift from simplistic token counting to rigorous, hardware-aware financial engineering. By transitioning the unit of account to microdollars, the enterprise platform achieves the granular precision necessary to charge departments for their actual physical compute consumption.

Implementing mathematically distinct pricing for input prefill, output decode, and cached memory holding ensures that cross-tenant subsidization is eliminated, creating an equitable utility model. To execute this policy, the Envoy AI Gateway must utilize a corrected CEL cost expression that isolates and discounts cached memory operations without mistakenly overcharging the base prompt. Simultaneously, the vLLM serving backend must be forcefully configured to emit complete telemetry during streaming operations, and must be secured against critical dictionary injection vulnerabilities that threaten multi-tenant cluster stability.

Finally, integrating a best-effort queueing policy via KServe allows the platform to harvest idle GPU cycles, driving up overall cluster Goodput and driving down the baseline denominator cost for all tenants. When these comprehensive directives are passed to the autonomous deployment agents, the resulting infrastructure operates not as an opaque commercial vendor, but as a transparent, mathematically equitable, and highly efficient corporate utility.
