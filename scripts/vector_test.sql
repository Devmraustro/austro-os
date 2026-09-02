CREATE TABLE IF NOT EXISTS memory_embeddings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    memory_id TEXT NOT NULL,
    content TEXT NOT NULL,
    embedding vector(10)
);

ALTER TABLE memory_embeddings ENABLE ROW LEVEL SECURITY;

CREATE POLICY workspace_isolation_policy ON memory_embeddings
    USING (workspace_id = current_setting('app.current_workspace', true)::UUID);

CREATE INDEX IF NOT EXISTS idx_memory_embeddings_workspace ON memory_embeddings(workspace_id);

INSERT INTO memory_embeddings (workspace_id, memory_id, content, embedding) VALUES
('11111111-1111-1111-1111-111111111111', 'mem-a1', 'AI agent architecture patterns', '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10)),
('11111111-1111-1111-1111-111111111111', 'mem-a2', 'Database design best practices', '[0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0, 0.1]'::vector(10)),
('22222222-2222-2222-2222-222222222222', 'mem-b1', 'Machine learning pipeline optimization', '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10)),
('22222222-2222-2222-2222-222222222222', 'mem-b2', 'Cloud infrastructure deployment', '[0.5, 0.6, 0.7, 0.8, 0.9, 1.0, 0.1, 0.2, 0.3, 0.4]'::vector(10));
