-- Test 1: Workspace A similarity search
SET SESSION AUTHORIZATION workspace_a_user;
SET app.current_workspace = '11111111-1111-1111-1111-111111111111';
SELECT 'WS_A_SIMILARITY' as test, memory_id, content, round((1 - (embedding <=> '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10)))::numeric, 4) as cosine_similarity
FROM memory_embeddings
ORDER BY embedding <=> '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10)
LIMIT 5;

-- Test 2: Cross-workspace isolation from A
SELECT 'WS_A_CROSS_ISOLATION' as test, count(*) as ws_b_visible FROM memory_embeddings WHERE workspace_id = '22222222-2222-2222-2222-222222222222';
RESET SESSION AUTHORIZATION;

-- Test 3: Workspace B similarity search
SET SESSION AUTHORIZATION workspace_b_user;
SET app.current_workspace = '22222222-2222-2222-2222-222222222222';
SELECT 'WS_B_SIMILARITY' as test, memory_id, content, round((1 - (embedding <=> '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10)))::numeric, 4) as cosine_similarity
FROM memory_embeddings
ORDER BY embedding <=> '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10)
LIMIT 5;

-- Test 4: Cross-workspace isolation from B
SELECT 'WS_B_CROSS_ISOLATION' as test, count(*) as ws_a_visible FROM memory_embeddings WHERE workspace_id = '11111111-1111-1111-1111-111111111111';
RESET SESSION AUTHORIZATION;
