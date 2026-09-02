DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'memory_embeddings'::regclass) THEN
        EXECUTE 'CREATE POLICY workspace_isolation_policy ON memory_embeddings
            USING (workspace_id = current_setting(''app.current_workspace'', true)::UUID)';
    END IF;
END$$;
