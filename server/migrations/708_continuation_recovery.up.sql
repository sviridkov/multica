CREATE TABLE issue_continuation_recovery (
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    chain_id UUID NOT NULL,
    remainder_id UUID NOT NULL,
    original_task_id UUID NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    receipt JSONB NOT NULL,
    admission JSONB NOT NULL
);
CREATE TABLE issue_continuation_operation (
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    operation_key TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    record JSONB NOT NULL
);
