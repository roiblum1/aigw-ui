# Capacity

The **Capacity** page answers, for each model:

- How much does one instance of each of its LLMInferenceServices serve at
  the latency you promise?
- How much can the model therefore be given a day, in dollars or tokens?
- How much of that do the tenants' budgets take, and how far could they
  go?
- Optionally, with GPU costs: what do its GPUs cost, and which prices
  would pay for them and no more?

The figures come from measurements with
[AIPerf](https://github.com/ai-dynamo/aiperf), NVIDIA's benchmark for model
servers. A measurement runs in a Job on the cluster. The prompts never reach
the hub.

## What is measured: each LLMInferenceService

One model can run in many ways at once: prefill and decode over several
nodes at one site, B200 on one node at FP4 with LMCache at another, H200
without LMCache at a third. Each serves a different amount, and the GPU
alone does not tell them apart. So every **LLMInferenceService (LLMI)** is
measured on its own, keyed by its cluster, namespace and name; two sites
with the same spec are measured apart, since their network, tuning and
versions differ.

**Its spec hash** says what it runs: SHA-256 of its `spec` with every
`replicas` field removed. Scaling it keeps its result; changing anything
else, such as one argument, makes it not measured until it is measured
again. A prefill/decode LLMI adds the ratio of prefill to decode replicas,
since that changes what one decode replica serves.

**What one instance is:** one pod on one node; one LeaderWorkerSet group
over several nodes, as KServe counts ready instances; for prefill and
decode there is no fixed instance, so the whole LLMI is measured through
its scheduler and the result is divided by its decode replicas during the
run.

The page also shows each LLMI's shape (GPU, GPUs per instance, topology,
revision), its vLLM `--quantization` and whether it uses LMCache, read from
its containers' arguments and environment (KServe passes extra arguments in
`VLLM_ADDITIONAL_ARGS`):

| Part of a shape | Read from |
|---|---|
| GPU | The node label `nvidia.com/gpu.product` of the deployment's ready pods, which the NVIDIA GPU Operator sets |
| GPUs per instance | What the ready pods ask for (`nvidia.com/gpu`), divided by the ready instances |
| Topology | One node; several nodes (`spec.worker`); prefill and decode (`spec.prefill`) |
| Revision | The annotation `aigw-ui.io/model-revision` |

The hub must be allowed to list pods and nodes on the cluster to read the
GPU. When it is not, the GPU is not known and the cluster's discovery
message on the Clusters page says so; see [security](security.md).

Measurements made before 0.16, by shape, are kept in **Measurements** as
history and count for nothing.

## GPU costs (optional)

**GPU costs** on the page holds what one GPU costs an hour, all in: the
hardware over its life (three or four years), power, cooling, rack, network
and the people who run it. An instance costs its GPUs times this. The list
shows every GPU the fleet's deployments run on.

They are optional, and only this page uses them: for what the measured
instances cost, what they are charged at the model's prices as a multiple of
that, and prices that would pay for them. Nothing a tenant pays depends on
them. A model has one price, set by hand on the Models page and the same on
every site and GPU; see [charging by cost](pricing.md#setting-prices-by-hand).
A model whose GPUs are costed in part gets a note, since its cost would
leave some out; one with none costed shows the rest of the page as usual.

## The benchmark profile

Every LLMI is measured with the same **benchmark profile**, so their
results compare. **Benchmark profile** on the page shows and edits it:

| Part | Holds | Default |
|---|---|---|
| Workload | AIPerf's coding-agent sessions: the longest prompt and answer, sessions, the shared system prompt, the pause between turns | prompts up to 167,000 tokens (about 100,000 on average), 32,000 shared, answers up to 1,500, 200 sessions, no pause |
| Targets | p95 time to first token, p95 time between tokens, failed requests | 5 s, 50 ms, 1% |
| Sweep | first step, halvings of the gap, longest step, requests per step, most in flight | 1, 2, 5 minutes, 60, 1,024 |

Saving makes a new **version**; the earlier ones stay. Every result keeps
the version it was measured with. A model is counted with one version
only, never two added together: the version whose results cover the most
of its ready instances, the active one on a tie. After a change the old
results therefore keep counting, marked stale, until enough LLMIs are
measured again, and capacity does not fall to zero at once.

A deployment that takes shorter requests than the profile's longest prompt
(`aigw-ui.io/max-model-len`, or what the pod lists under `/v1/models`) gets
every part of a session scaled down by the same factor: one profile cannot
hand a 100,000-token prompt to a model that takes 8,192.

## How a measurement works

1. **Coding-agent sessions.** AIPerf makes up sessions the way a coding
   agent such as Claude Code or Cursor sends them (`aiperf synthesize
   agentic-code`):

   | Part of a prompt | Size | Shared by |
   |---|---|---|
   | System prompt and tools | 32,000 tokens | Every session |
   | Files opened at the start | 15,000 tokens on average | One session |
   | Each new turn | about 6,000 tokens, plus the previous answer | One session |
   | Answer | about 1,000 tokens, at most 1,500 | |

   The same sessions are replayed at every step, so after the first one
   the shared parts are in the cache, as on a busy day.
2. **Up until it breaks, then halve the gap.** The Job runs 1, 2, 4, 8 …
   requests in flight until a step misses a target, then the step halfway
   between the last that met them and the first that did not, twice by
   default: 8 met them and 16 did not, so 12, then 10 or 14. A step sends
   at least 60 requests, and four per request in flight above that, and
   stops after five minutes even so; it is then marked as having few
   samples.
3. **Targets.** A step meets them when at most 1% of its requests failed
   and both 95th percentiles are within their targets. A drained step must
   also have served nothing else (below).
4. **Result.** The highest step that met the targets below the lowest step
   that missed them, with the tokens a second it served: uncached prompt,
   cached prompt, output.

For each step the Job prints one line of numbers to its log, which the hub
reads every 20 seconds:

| Figure | From |
|---|---|
| Requests answered and failed | AIPerf |
| Prompt and output tokens | The model server's `usage`, as AIPerf records it |
| Time to first token, time between tokens: p50 and p95 | AIPerf |
| Share of prompt tokens from the cache | The pod's own counters before and after the step: `vllm:prompt_tokens_cached_total` over `vllm:prompt_tokens_total`, or `vllm:prefix_cache_hits_total` over `vllm:prefix_cache_queries_total` |
| Prompt and generated tokens the pod counted, and the time between the two readings | `vllm:prompt_tokens_total`, `vllm:generation_tokens_total` |

**Nothing of a prompt reaches the hub**: the Job makes its prompts up and
the hub reads only these lines. **No tenant is charged**: the requests do
not pass the gateway's quotas.

## Drained or live

**Drained** is the default, and the only way the hub measures by itself.
It needs another site that serves the model:

1. The hub drains the model on the LLMI's site, as **Drain** on the Models
   page does: the entry route's weight steps down to 0, one instance a
   discovery round. A site an operator drained already is used as it is.
2. Once the weight is 0 the Job starts and waits until the measured pods
   have no request in flight (`vllm:num_requests_running`), for at most 10
   minutes; otherwise the measurement fails with that reason. The drain
   itself may take up to 30 minutes.
3. It measures, and the hub ends the drain: the weight comes back with the
   usual steps. A drain an operator made is left as it was.

**Live** measures an LLMI while it serves its users, for a model served on
one site only, or when you choose it. The form says so first:

> This LLMI serves users now. The benchmark adds load until it misses the
> SLO. Users of *model* on *site* will get slower answers or errors for up
> to *duration*, and their traffic makes the result lower than what the
> LLMI could serve alone.

It starts only once the model's name is typed. The result is marked
**live**, and its throughput is the pod's own counters, users' traffic
included; the latency is AIPerf's own requests'.

**Other traffic.** Each step compares the prompt tokens AIPerf sent with
what the pod's prompt counter grew by: other traffic is 1 − sent ÷
counted. Drained, a step with more than 5% fails as contaminated; it
usually means clients call the site's own route directly. Live, it is
expected, and shown with the step.

## Prefill and decode

A prefill/decode LLMI has no instance of its own, so it is measured whole,
through its scheduler on the gateway's **peer listener**, which asks for
the site's client certificate. The Job runs in the gateway's namespace and
mounts the client certificate (`fleet.peerClientSecret`, `llm-peer-client`
by default) and the CA (`fleet.peerCAConfigMap`) read-only; AIPerf cannot
present a certificate, so the Job's script passes its requests on with it,
answers streamed as they come. It needs the cluster's peer host and the
hub's `fleet.domain` or `fleet.peerSNI`. The result is divided by the
decode replicas during the run, whose pods' counters it reads.

The peer listener counts nothing against a quota (lab check B): a model's
peer route goes to its InferencePool, and a QuotaPolicy attaches only to an
AIServiceBackend.

## When an LLMI is measured again

| What happened | Its result |
|---|---|
| Its spec hash changed | Dropped: not measured until measured again |
| The profile has a new version | Kept, marked stale, until measured again |
| It is older than 30 days | Kept, marked stale |

## Measuring by itself

With `capacity.window` set in the chart, such as `"Fri 22:00 - Sat 06:00
Asia/Jerusalem"` once a week or `"01:00 - 05:00 UTC"` every day, the hub
works through a queue inside that window: every LLMI with a ready instance
that is not measured, then every one whose result is stale. It measures
drained only, one per site and one per model at a time. An LLMI whose model
no other site serves can only be measured live and waits for an operator.
**Measuring by itself** on the page shows the window, the queue and what
each item waits for. Empty, the default, never.

## Before you measure

- **The image.** Set `capacity.image` in the chart to an AIPerf image the
  LLM clusters can pull. The offline bundle builds one from
  `deploy/aiperf/Containerfile` and `load-images.sh` prints the value to
  set. It has the vocabulary of AIPerf's built-in tokenizer inside, so a
  measurement downloads nothing. CI runs the Job's script in that image
  against a stand-in for vLLM on every change.
- **The model's length.** Before anything else the Job asks the instance
  (`/v1/models`) how long a request it takes, and stops at once when the
  profile, scaled to the deployment, still does not fit.
- **One at a time.** A model is measured once at a time.
- **Network.** The Job must reach the pod's port, and for prefill/decode
  the gateway and the decode pods from the gateway's namespace. A
  NetworkPolicy that only lets the gateway in also keeps the Job out.
- **HTTPS.** A pod that answers only over HTTPS is not measured yet.

The Job is called `aigw-capacity-<id>`, asks for 2 CPUs and 4 GiB, never
restarts, and is stopped by Kubernetes after the longest the steps can take.
It is deleted 7 days after it ends. **Stop** deletes it at once and ends
the drain; the steps it finished are kept. See [security](security.md) for
what this needs on the cluster.

## Reading the result

**The LLMI table.** For each LLMI: its site, shape, quantization and spec,
when it was measured and in which mode, whether its result is stale or why
it has none, its ready instances, the requests in flight one instance
serves within the targets, what one instance serves in a day busy all day,
what it costs an hour, and what a million input-equivalent tokens cost on
it busy all day. The last column shows which hardware serves the model for
the least.

What an instance serves a second is valued in the model's unit:

```
priced model:   (prompt - cached) x input price + cached x cached price + output x output price
otherwise:      prompt + output tokens
```

The second is what a quota counts unless the model has its own cost
expression.

**What the model can be given a day.**

```
busy all day   = sum over LLMIs with a result: one instance a day x ready instances
can be given   = busy all day x expected busy share
```

Instances on a drained site are not counted, so the figure follows the
fleet: a site that loses pods or is drained lowers it at the next poll.

**What is left to give.**

```
ceiling        = can be given x overcommit limit
given          = sum over the enabled tenants' enforced budgets of: the most it lets be used in a day
left to give   = ceiling - given
```

- **Overcommit limit.** Each model has one, 1.5 until you change it next
  to the busy share. Tenants rarely use all of their budgets on the same
  day, so budgets that add up to more than the GPUs serve keep them busier.
  1 means no overcommit.
- **Budgets per hour or minute** count at the most they allow in a day: $1
  an hour is up to $24 a day. A budget in dry run is not counted.
- **Overcommit** is given divided by can be given.
- **The shared pool** is shown apart. Every request is charged to the
  tenant's budget and to the pool, and passes while either has room, so a
  pool above 1 lets tenants past their budget use what others leave. It is
  not added to what is given.

Nothing is refused: a budget that takes the model past its ceiling is
saved once you confirm it.

**How far the budgets could go.** If tenants keep using their budgets as on
the busiest day of the last 30, the budgets could add up to

```
can be given x given / busiest day
```

before such a day fills what the model can be given. It is a suggestion for
the overcommit limit once there is history.

## Budgets in dollars

**How many dollars a model can be given a day.** For a priced model, *can
be given* is what its measured instances serve a day at the expected busy
share, charged at the model's prices. It is in the same dollars as the
budgets whatever the prices are, set by hand or not, so the comparison holds
without GPU costs. The overcommit limit says how far the budgets may go past
it. At the break-even prices it is also exactly what the GPUs cost a day,
without profit.

The figures appear where budgets are decided:

- **Capacity page**: each model's panel, and above them the total over the
  measured models in dollars.
- **A tenant's quotas**: choosing a model in **Add a quota**, or editing a
  budget, shows what the model can be given, its ceiling, what the budgets
  take and what is left, and the same with the budget being typed. Saving
  past the ceiling asks to confirm. The card says what the tenant can use a
  day over the models in dollars.
- **Overview**: the **Budgets** card, one row per model and a total.
- **Task log**: a budget change says where the model stands after it, for
  example "glm budgets now $1,840 of $1,700 a day (1.08×, limit 1.5×)".

**A model in tokens** has no dollars. Give it prices under **Prices** on
the Models page, by hand; or, when its GPUs have a cost, **Use these
prices** next to the break-even prices on the Capacity page opens the
model's prices with them filled in. The prices then follow the rules in
[pricing](pricing.md): they start at the next 00:00 UTC, the first prices
convert the model's limits once, and the model starts in dry-run.

**Busiest day in 30 days** is the most the model was used in one day,
best-effort included, against what the measured instances serve busy all
day. **An average day** is the same for the mean of the 30 days.

**Expected busy share.** Each model has one, 60% until you change it. The
prices are worked out at it. The page shows next to it the share an average
day really used. Raising it lowers the price; it should follow what the
platform sustains, not be set high to make the price low, because a share
the platform does not reach leaves the GPUs' cost unpaid. Dividing by
actual use instead would raise the price in a quiet month, which makes
tenants use less, which raises it again. Set it once, then review it every
month or so against the average day shown beside it.

**Prices and cost.** With a cost for every measured shape's GPU:

```
cost an hour        = sum over measured shapes of: GPUs per instance x GPU cost x ready instances
input price per 1M  = cost an hour / (3,600 x weighted tokens a second x expected busy share) x 1,000,000
weighted            = sum over measured shapes of: ready instances x
                      ((prompt - cached) + cached ratio x cached + output ratio x output)
```

The ratios are the model's own prices (cached over input, output over
input), or a tenth and four for a model without prices. The result is one
set of prices for the model: tenants pay the same whichever shape serves
them, and at the expected busy share the prices pay for the GPUs exactly,
without profit. For a priced model the page also shows what the measured
instances are charged busy for an hour, and that divided by their cost.

**Steps.** A shape's row opens the chart of its latest measurement (output
tokens a second and time to first token at each step, against its target)
and a table of every step. **Measurements** lists the earlier ones.

## What the figures leave out

- One pod and one kind of traffic. A model whose prompts are short, such as
  a small model used for chat, is better measured with a shorter longest
  prompt.
- The cache share depends on the replay: every session's shared part is
  cached after the first step. Real traffic with many repositories caches
  less. A session's turns also follow each other with no pause, so its
  prompt is never evicted between them, as it can be while a person reads
  an answer.
- The pod's counters include any other traffic it served during a step.
- The scheduler's choice of replica by cache is not part of it. Multiplying
  by the ready instances assumes it spreads the load evenly.
- The site weights of the entry route use the measurements only when every
  deployment of the model with a ready instance has a measured shape;
  until then they follow `aigw-ui.io/capacity-per-instance`. See
  [architecture](architecture.md).

## API

| Method | Path | Body | Result |
|---|---|---|---|
| GET | `/capacity` | | `{"image", "gpus", "models", "profile", "campaign"}`. Each model has `llmis`, its LLMInferenceServices with their shape, `spec_hash`, `quantization`, `lmcache`, the `run` that counts (or null and `why`) and `stale`; `profile_version`, the version it is counted with; `sites`; `plan`, whose `shapes` are the LLMIs in the same order; and `running`. `campaign` has `window`, `open`, `next` and `queue` |
| GET | `/benchmark-profiles` | | Every version of the profile, newest first; one is `active` |
| PUT | `/benchmark-profile` | `{"name", "workload", "slo", "sweep", "note"}` | The new version, now active |
| PUT | `/models/{id}/target-utilization` | `{"utilization"}` | 204. 0.05 to 1 |
| PUT | `/models/{id}/overcommit-limit` | `{"limit"}` | 204. 1 to 10 |
| GET | `/budgets` | | `{"models", "credits"}`: for each model `measured`, `capacity_day`, `ceiling_day`, `given_day`, `left_day`, `overcommit`, `overcommit_limit`, `short_windows`, `pool_day`, in its unit; `credits` adds them up over the measured models in dollars |
| PUT | `/gpu-prices/{gpu}` | `{"usd_per_hour", "note"}` | 204. A GPU cost, shown as **GPU costs** on the page |
| DELETE | `/gpu-prices/{gpu}` | | 204 |
| GET | `/clusters/{id}/deployments/{namespace}/{name}/pods` | | The deployment's ready pods, read from the cluster; a prefill/decode deployment lists its decode pods |
| GET | `/models/{id}/capacity-runs` | | The model's measurements, newest first |
| POST | `/models/{id}/capacity-runs` | `{"cluster_id", "namespace", "deployment", "pod", "mode", "confirm"}` | 201 and the measurement. `mode` is `drained` or `live`; live needs `confirm` equal to the model's name. `pod` is empty for a prefill/decode deployment. 409 with the reason when it cannot start, such as a drained measurement of a model no other site serves |
| GET | `/capacity-runs/{id}` | | One measurement, with `result.steps` saying of each step whether it met the targets and its `foreign_share` |
| POST | `/capacity-runs/{id}/cancel` | | The measurement, stopped; a drain it made ends |
| DELETE | `/capacity-runs/{id}` | | 204. 409 while it runs |

A measurement carries `spec_hash` (empty for one of a shape from before
0.16), `profile_id`, `profile_version`, `mode`, `instances_during` (what
the result is divided by for one instance), `phase` (`draining` or
`measuring` while it runs), `drained_site` and `started_by` (`operator` or
`campaign`). Its `settings` are the profile as it applied to the
deployment.
