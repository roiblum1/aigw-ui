-- Every deployment (LLMInferenceService) of a model on a cluster, with its
-- shape: the GPU it runs on, how many GPUs one instance holds, and whether
-- one instance is one pod, several nodes, or prefill and decode pods. One
-- model can run in several shapes, and each serves a different amount.
CREATE TABLE model_deployments (
    cluster_id        uuid NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
    namespace         text NOT NULL,
    name              text NOT NULL,
    model_id          uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    topology          text NOT NULL CHECK (topology IN ('single', 'multi-node', 'prefill-decode')),
    -- The node label nvidia.com/gpu.product of its pods, or '' without one.
    gpu               text NOT NULL DEFAULT '',
    gpus_per_instance double precision NOT NULL DEFAULT 0,
    revision          text NOT NULL DEFAULT '',
    -- Ready instances, NULL while the deployment has not reported them.
    instances         double precision,
    observed_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_id, namespace, name)
);

-- What a GPU costs an hour, all in: the hardware over its life, power,
-- rack, network and people. An instance costs its GPUs times this.
CREATE TABLE gpu_prices (
    gpu          text PRIMARY KEY,
    usd_per_hour double precision NOT NULL CHECK (usd_per_hour > 0),
    note         text NOT NULL DEFAULT '',
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- A measurement of what one instance of one deployment serves at the
-- latency an operator promises. AIPerf runs it in a Job on the cluster
-- against one pod. The Job makes up its own prompts; only the numbers reach
-- the server. Deployments of the same shape share it.
CREATE TABLE capacity_runs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id          uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    cluster_id        uuid NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
    -- The deployment and the pod measured, and its shape at the start.
    namespace         text NOT NULL,
    deployment        text NOT NULL,
    instance          text NOT NULL,
    topology          text NOT NULL,
    gpu               text NOT NULL,
    gpus_per_instance double precision NOT NULL,
    revision          text NOT NULL,
    job_name          text NOT NULL,
    -- The load steps and the latency targets.
    settings          jsonb NOT NULL,
    status            text NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    detail            text NOT NULL DEFAULT '',
    -- What the made-up sessions hold, and one entry per load step, added
    -- while the Job runs.
    dataset           jsonb,
    steps             jsonb NOT NULL DEFAULT '[]',
    created_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz
);
CREATE INDEX capacity_runs_model ON capacity_runs (model_id, created_at DESC);
-- At most one measurement runs per model at a time: two would share the
-- GPUs of one site and both be wrong.
CREATE UNIQUE INDEX capacity_runs_one_running ON capacity_runs (model_id) WHERE status = 'running';

-- The share of the day a model's instances are expected to be busy. The
-- prices that pay for the instances are worked out at it.
ALTER TABLE models ADD COLUMN target_utilization double precision NOT NULL DEFAULT 0.6
    CHECK (target_utilization > 0 AND target_utilization <= 1);
