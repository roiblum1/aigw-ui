-- Measurements keyed by LLMInferenceService instead of by shape
-- (docs/design/capacity-prices-wallets.md, step 2).

-- The one benchmark every LLMInferenceService is measured with. Editing it
-- adds a version; exactly one is active, and a result counts only with the
-- version it was measured with.
CREATE TABLE benchmark_profiles (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    version    integer NOT NULL UNIQUE CHECK (version >= 1),
    workload   jsonb NOT NULL,
    slo        jsonb NOT NULL,
    sweep      jsonb NOT NULL,
    note       text NOT NULL DEFAULT '',
    active     boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX benchmark_profiles_one_active ON benchmark_profiles (active) WHERE active;
INSERT INTO benchmark_profiles (name, version, workload, slo, sweep, active) VALUES ('agentic-code', 1,
    '{"max_prompt_tokens": 167000, "max_output_tokens": 1500, "sessions": 200, "shared_prefix_tokens": 32000, "pause_ms": 0}',
    '{"ttft_p95_ms": 5000, "itl_p95_ms": 50, "max_error_share": 0.01}',
    '{"first_concurrency": 1, "bisect_steps": 2, "step_seconds": 300, "min_requests": 60, "max_concurrency": 1024}',
    true);

-- What each deployment runs, whatever its scale: SHA-256 of its spec
-- without any replicas field (kube.SpecHash). A result counts only for the
-- spec it was measured on. Quantization and LMCache are shown on the page;
-- decode_replicas is what a prefill/decode measurement is divided by.
ALTER TABLE model_deployments ADD COLUMN spec_hash text NOT NULL DEFAULT '';
ALTER TABLE model_deployments ADD COLUMN quantization text NOT NULL DEFAULT '';
ALTER TABLE model_deployments ADD COLUMN lmcache boolean NOT NULL DEFAULT false;
ALTER TABLE model_deployments ADD COLUMN decode_replicas bigint NOT NULL DEFAULT 1;

-- A run's spec hash and profile. Runs from before have spec_hash '' and no
-- profile: they stay as history and count for nothing.
ALTER TABLE capacity_runs ADD COLUMN spec_hash text NOT NULL DEFAULT '';
ALTER TABLE capacity_runs ADD COLUMN profile_id uuid REFERENCES benchmark_profiles (id);
ALTER TABLE capacity_runs ADD COLUMN profile_version integer;
-- What one instance is: the result divided by this, the decode replicas of
-- a prefill/decode deployment during the run, 1 otherwise.
ALTER TABLE capacity_runs ADD COLUMN instances_during double precision NOT NULL DEFAULT 1 CHECK (instances_during > 0);
-- Where a running drained measurement is: draining the site, measuring, and
-- whether it drained the site itself, so that it restores only its own drain.
ALTER TABLE capacity_runs ADD COLUMN phase text NOT NULL DEFAULT '' CHECK (phase IN ('', 'draining', 'measuring'));
ALTER TABLE capacity_runs ADD COLUMN drained_site boolean NOT NULL DEFAULT false;
ALTER TABLE capacity_runs ADD COLUMN started_by text NOT NULL DEFAULT 'operator' CHECK (started_by IN ('operator', 'campaign'));
CREATE INDEX capacity_runs_llmi ON capacity_runs (cluster_id, namespace, deployment, created_at DESC);
