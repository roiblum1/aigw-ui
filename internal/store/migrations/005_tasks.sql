-- The task log: one row per change an admin or the portal made, and for each
-- cluster what happened when the change was applied there.
CREATE TABLE tasks (
    id         bigserial PRIMARY KEY,
    action     text NOT NULL,
    summary    text NOT NULL,
    -- Set for a task that is finished without any cluster work, such as a
    -- usage reset. NULL means the status follows from task_results.
    status     text CHECK (status IN ('succeeded', 'failed')),
    message    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task_results (
    task_id      bigint NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    cluster_id   uuid REFERENCES clusters (id) ON DELETE SET NULL,
    cluster_name text NOT NULL,
    -- failed results are tried again by the next sync.
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    message      text NOT NULL DEFAULT '',
    changes      jsonb NOT NULL DEFAULT '[]',
    rejected     jsonb NOT NULL DEFAULT '[]',
    finished_at  timestamptz,
    PRIMARY KEY (task_id, cluster_name)
);

CREATE INDEX task_results_open ON task_results (cluster_id) WHERE status <> 'succeeded';
