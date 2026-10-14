# Capacity

The **Capacity** page answers three questions for each model:

- How much does one instance serve at the latency you promise?
- How much can the model therefore be given a day, in dollars or tokens?
- How much of that do the tenants' budgets already take, and how far are
  they overcommitted?

It also works out the prices that pay for the instances, from what an
instance costs an hour.

The answer comes from a measurement with
[AIPerf](https://github.com/ai-dynamo/aiperf), NVIDIA's benchmark for model
servers. It runs in a Job on the cluster. The prompts never reach the hub.

## How a measurement works

1. **One pod.** The hub picks a ready pod of the model's deployment, or the
   one you name, and starts a Job next to it in the same namespace. The Job
   sends requests straight to the pod's address, past the gateway and the
   scheduler, so the result is what one instance serves. The hub multiplies
   it by the ready instances on every site.
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
   prompt is about 100,000 tokens on average. Between turns it waits like
   an agent running a tool (2.5 s on average) or, one turn in three, like a
   person (40 s). A deployment that takes shorter prompts gets every part
   scaled down by the same factor. The measurement shows the average and the
   longest prompt it made up.
3. **Steps.** The Job replays the sessions with a fixed number of requests
   in flight: 1, 2, 4, 8, 16, 32, 64 by default, each for 5 minutes. The
   same sessions are replayed at every step, so after the first one the
   shared parts are in the cache, as on a busy day.
4. **Targets.** A step meets them when at most 1% of its requests failed,
   the 95th percentile of the time to the first token is within its target
   (5 s by default), and the 95th percentile of the time between tokens is
   within its target (50 ms, 20 tokens a second for each user, by default).
   The Job stops at the first step that misses: a higher load only gets
   slower.
5. **Result.** The highest step that met the targets, with every step
   below it meeting them too.

For each step the Job prints one line of numbers to its log, which the hub
reads every 20 seconds:

| Figure | From |
|---|---|
| Requests answered and failed | AIPerf |
| Prompt and output tokens | The model server's `usage`, as AIPerf records it |
| Time to first token, time between tokens: p50 and p95 | AIPerf |
| Share of prompt tokens from the cache | The pod's own counters, `vllm:prompt_tokens_cached_total` over `vllm:prompt_tokens_total`, before and after the step |

**Nothing of a prompt reaches the hub**: the Job makes its prompts up and
the hub reads only these lines. **No tenant is charged**: the requests do
not pass the gateway.

## Before you measure

- **The image.** Set `capacity.image` in the chart to an AIPerf image the
  LLM clusters can pull. The offline bundle builds one from
  `deploy/aiperf/Containerfile` and `load-images.sh` prints the value to
  set. It has the vocabulary of AIPerf's built-in tokenizer inside, so a
  measurement downloads nothing. Without the setting the page says so and
  nothing can be measured.
- **The pod keeps its traffic.** While it is measured, its users get slower
  answers, and their requests make the result lower. Measure a pod that
  takes no traffic, such as one extra replica that you then name in the
  form, or a quiet hour.
- **One at a time.** A model is measured on one pod at a time.
- **Not measured yet.** A deployment with separate prefill pods, where one
  instance is two pods, and a pod that answers only over HTTPS.
- **Network.** The Job must reach the pod's port. A NetworkPolicy in the
  namespace that only lets the gateway in also keeps the Job out.

The Job is called `aigw-capacity-<id>`, asks for 2 CPUs and 4 GiB, never
restarts, and is stopped by Kubernetes after the longest the steps can take
(each step plus 6 minutes, plus 15). It is deleted 7 days after it ends.
**Stop** deletes it at once; the steps it finished are kept.

## Reading the result

**One instance.** The requests in flight it serves within the targets, the
tokens a second at that load, and the share of prompt tokens from the cache.

**What the model can be given a day.**

```
per instance a day = what one instance serves a second, in the model's unit x 86,400
can be given       = per instance a day x ready instances x expected busy share
```

What an instance serves a second is valued at the model's prices:

```
(prompt - cached) x input price + cached x cached price + output x output price
```

For a model without prices it is prompt plus output tokens, which is what a
quota counts unless the model has its own cost expression. The busy share
is set in the form, 60% by default.

**Given to tenants.** Every enabled tenant's enforced limit on the model,
as an amount a day: a limit per hour counts 24 times. Dry-run quotas are
left out.

**Overcommit** is given divided by can be given. Above 1 is usual: not
every tenant uses its whole budget, and not on the same day. How far is
safe shows in the next figure.

**Busiest day in 30 days** is the most the model was used in one day,
best-effort included, against what every ready instance serves busy all day.
A model given 3× its capacity whose busiest day used 25% of it has room;
one whose busiest day used 90% has none.

**Cost and price.** With the cost of one instance an hour, the page works
out the prices that pay for the instances at the expected busy share, the
same arithmetic as [setting a price without profit](pricing.md#setting-a-price-without-profit):

```
input price per 1M = cost an hour / (3,600 x weighted tokens a second x busy share) x 1,000,000
weighted           = (prompt - cached) + cached ratio x cached + output ratio x output
```

The ratios are the model's own prices (cached over input, output over
input), or a tenth and four for a model without prices. For a priced model
the page also shows what one busy instance is charged an hour, and that
divided by its cost.

**Load steps.** A chart of the output tokens a second and the time to first
token at each step, against its target, and a table with every figure.
**Measurements** lists the earlier ones of the model.

## What the figures leave out

- One pod and one kind of traffic. A model with other prompts, another
  quantization or other GPUs needs its own measurement.
- The cache share depends on the replay: every session's shared part is
  cached after the first step. Real traffic with many repositories caches
  less.
- The pod's counters include any other traffic it served during a step.
- The scheduler's choice of replica by cache is not part of it. Multiplying
  by the ready instances assumes it spreads the load evenly.

## API

| Method | Path | Body | Result |
|---|---|---|---|
| GET | `/capacity` | | `{"image", "models": [...]}`: for each model its sites, the defaults for a new measurement, the latest result, the plan and the one running |
| GET | `/models/{id}/capacity-runs` | | The model's measurements, newest first |
| POST | `/models/{id}/capacity-runs` | `{"cluster_id", "pod", "settings"}` | 201 and the measurement. 409 with the reason when it cannot start |
| GET | `/capacity-runs/{id}` | | One measurement |
| POST | `/capacity-runs/{id}/cancel` | | The measurement, stopped |
| DELETE | `/capacity-runs/{id}` | | 204. 409 while it runs |

`settings` holds `concurrency` (a list), `step_seconds`, `sessions`,
`max_prompt_tokens`, `max_output_tokens`, `ttft_p95_ms`, `itl_p95_ms`,
`instance_cost_per_hour_usd` and `utilization` (0 to 1). What is left out
takes its default for the site's deployment.
