# Capacity

The **Capacity** page answers, for each model:

- How much does one instance of each of its shapes serve at the latency
  you promise?
- How much can the model therefore be given a day, in dollars or tokens?
- How much of that do the tenants' budgets take, and how far could they
  go?
- Which one set of prices pays for its GPUs, and no more?

The figures come from measurements with
[AIPerf](https://github.com/ai-dynamo/aiperf), NVIDIA's benchmark for model
servers. A measurement runs in a Job on the cluster. The prompts never reach
the hub.

## Shapes

One model can run in several ways at once: on one node of 8× B300 at one
site, on 8× H200 at another, on prefill and decode pods spread over several
nodes at a third. Each serves a different amount. The hub calls what one
instance is a **shape**, and reads it from each deployment
(LLMInferenceService) when it polls the cluster:

| Part of a shape | Read from |
|---|---|
| GPU | The node label `nvidia.com/gpu.product` of the deployment's ready pods, which the NVIDIA GPU Operator sets |
| GPUs per instance | What the ready pods ask for (`nvidia.com/gpu`), divided by the ready instances |
| Topology | One node; several nodes (`spec.worker`); prefill and decode (`spec.prefill`) |
| Revision | The annotation `aigw-ui.io/model-revision` |

The hub must be allowed to list pods and nodes on the cluster to read the
GPU. When it is not, the GPU is not known and the cluster's discovery
message on the Clusters page says so; see [security](security.md).

A measurement counts for every deployment of the same model and shape,
wherever it runs. A new site with a shape that is already measured needs no
new measurement. A shape that is not measured is listed and not counted.

**Prefill and decode is not measured yet.** One of its instances is several
pods, so the Job would have to go through the deployment's scheduler, which
in this platform answers only on the gateway's peer listener with its client
certificate. Until then such a shape is listed as not measured.

## GPU prices

**GPU prices** on the page holds what one GPU costs an hour, all in: the
hardware over its life (three or four years), power, cooling, rack, network
and the people who run it. An instance costs its GPUs times this. The list
shows every GPU the fleet's deployments run on; one without a price leaves
the cost of its models out, and the page says so.

## How a measurement works

1. **One pod, chosen on purpose.** **Measure** on a shape's row opens a
   form: a deployment of that shape, then one of its ready pods. The Job
   starts in the deployment's namespace and sends requests straight to the
   pod's address, past the gateway and the scheduler.
2. **Coding-agent sessions.** AIPerf makes up sessions the way a coding
   agent such as Claude Code or Cursor sends them (`aiperf synthesize
   agentic-code`):

   | Part of a prompt | Size | Shared by |
   |---|---|---|
   | System prompt and tools | 32,000 tokens | Every session |
   | Files opened at the start | 15,000 tokens on average | One session |
   | Each new turn | about 6,000 tokens, plus the previous answer | One session |
   | Answer | about 1,000 tokens, at most 1,500 | |

   A session grows until it reaches 167,000 tokens or ends early, so a
   prompt is about 100,000 tokens on average. There is no pause between
   turns: a step keeps its requests in flight. A deployment that takes
   shorter prompts gets every part scaled down by the same factor. The
   measurement shows the average and the longest prompt it made up.
3. **Steps.** The Job replays the sessions with a fixed number of requests
   in flight: 1, 2, 4, 8, 16, 32, 64 by default. A step sends at least 60
   requests, and four per request in flight above that, so its 95th
   percentile rests on enough of them. It stops after 20 minutes even so,
   and is then marked as having few samples. The same sessions are replayed
   at every step, so after the first one the shared parts are in the cache,
   as on a busy day.
4. **Targets.** A step meets them when at most 1% of its requests failed,
   the 95th percentile of the time to the first token is within its target
   (5 s by default), and the 95th percentile of the time between tokens is
   within its target (50 ms, 20 tokens a second for each user, by default).
   The Job stops at the first step that misses.
5. **Result.** The highest step that met the targets, with every step
   below it meeting them too.

For each step the Job prints one line of numbers to its log, which the hub
reads every 20 seconds:

| Figure | From |
|---|---|
| Requests answered and failed | AIPerf |
| Prompt and output tokens | The model server's `usage`, as AIPerf records it |
| Time to first token, time between tokens: p50 and p95 | AIPerf |
| Share of prompt tokens from the cache | The pod's own counters before and after the step: `vllm:prompt_tokens_cached_total` over `vllm:prompt_tokens_total`, or `vllm:prefix_cache_hits_total` over `vllm:prefix_cache_queries_total` |

**Nothing of a prompt reaches the hub**: the Job makes its prompts up and
the hub reads only these lines. **No tenant is charged**: the requests do
not pass the gateway.

## Before you measure

- **The image.** Set `capacity.image` in the chart to an AIPerf image the
  LLM clusters can pull. The offline bundle builds one from
  `deploy/aiperf/Containerfile` and `load-images.sh` prints the value to
  set. It has the vocabulary of AIPerf's built-in tokenizer inside, so a
  measurement downloads nothing. CI runs the Job's script in that image
  against a stand-in for vLLM on every change.
- **The pod keeps its traffic.** While it is measured its users get slower
  answers, and their requests make the result lower. The form asks you to
  confirm. A pod that takes no traffic gives the cleanest figure, such as
  one extra replica that you then name.
- **The model's length.** The form fits the longest prompt to the
  annotation `aigw-ui.io/max-model-len` on the deployment. Without it the
  form offers AIPerf's 167,000 tokens. Before anything else the Job asks the
  pod (`/v1/models`) how long a request it takes, and stops at once with
  the longest prompt to set when the settings do not fit.
- **One at a time.** A model is measured on one pod at a time.
- **Network.** The Job must reach the pod's port. A NetworkPolicy in the
  namespace that only lets the gateway in also keeps the Job out.
- **HTTPS.** A pod that answers only over HTTPS is not measured yet.

The Job is called `aigw-capacity-<id>`, asks for 2 CPUs and 4 GiB, never
restarts, and is stopped by Kubernetes after the longest the steps can take.
It is deleted 7 days after it ends. **Stop** deletes it at once; the steps it
finished are kept. See [security](security.md) for what this needs on the
cluster.

## Reading the result

**The shapes table.** For each shape: its sites, its ready instances, the
requests in flight one instance serves within the targets, what one
instance serves in a day busy all day, what it costs an hour, and what a
million input-equivalent tokens cost on it busy all day. The last column
shows which hardware serves the model for the least.

What an instance serves a second is valued in the model's unit:

```
priced model:   (prompt - cached) x input price + cached x cached price + output x output price
otherwise:      prompt + output tokens
```

The second is what a quota counts unless the model has its own cost
expression.

**What the model can be given a day.**

```
busy all day   = sum over measured shapes of: one instance a day x ready instances
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

**How many dollars a model can be given a day.** At the break-even prices,
what a model's ready GPUs serve a day at the expected busy share is worth
exactly what those GPUs cost a day. So for a priced model, *can be given* is
the GPUs' cost a day, without profit, and the overcommit limit says how far
the budgets may go past it.

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

**A model in tokens** has no dollars. **Use these prices**, next to the
break-even prices on the Capacity page, opens the model's prices with them
filled in. The prices then follow the rules in [pricing](pricing.md): they
start at the next 00:00 UTC, the first prices convert the model's limits once,
and the model starts in dry-run.

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

**Cost and price.** With every measured shape's GPU priced:

```
cost an hour        = sum over measured shapes of: GPUs per instance x GPU price x ready instances
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
| GET | `/capacity` | | `{"image", "gpus", "models"}`: for each model its deployments with their shapes, the expected busy share, the defaults for a new measurement, the latest result of each shape, the plan and the measurement running |
| PUT | `/models/{id}/target-utilization` | `{"utilization"}` | 204. 0.05 to 1 |
| PUT | `/models/{id}/overcommit-limit` | `{"limit"}` | 204. 1 to 10 |
| GET | `/budgets` | | `{"models", "credits"}`: for each model `measured`, `capacity_day`, `ceiling_day`, `given_day`, `left_day`, `overcommit`, `overcommit_limit`, `short_windows`, `pool_day`, in its unit; `credits` adds them up over the measured models in dollars |
| PUT | `/gpu-prices/{gpu}` | `{"usd_per_hour", "note"}` | 204 |
| DELETE | `/gpu-prices/{gpu}` | | 204 |
| GET | `/clusters/{id}/deployments/{namespace}/{name}/pods` | | The deployment's ready pods, read from the cluster |
| GET | `/models/{id}/capacity-runs` | | The model's measurements, newest first |
| POST | `/models/{id}/capacity-runs` | `{"cluster_id", "namespace", "deployment", "pod", "settings"}` | 201 and the measurement. 409 with the reason when it cannot start |
| GET | `/capacity-runs/{id}` | | One measurement, with `result.steps` saying of each step whether it met the targets |
| POST | `/capacity-runs/{id}/cancel` | | The measurement, stopped |
| DELETE | `/capacity-runs/{id}` | | 204. 409 while it runs |

`settings` holds `concurrency` (a list), `min_requests`, `step_seconds`
(the longest a step may take), `sessions`, `max_prompt_tokens`,
`max_output_tokens`, `ttft_p95_ms` and `itl_p95_ms`. What is left out takes
its default for the deployment.
