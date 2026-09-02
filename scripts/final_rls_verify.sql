SET SESSION AUTHORIZATION workspace_a_user;
SET app.current_workspace = '11111111-1111-1111-1111-111111111111';
SELECT 'A_departments' as t, count(*) FROM departments;
SELECT 'A_embeddings' as t, count(*) FROM memory_embeddings;
SELECT 'A_see_B_dept' as t, count(*) FROM departments WHERE name = 'Dept B';
SELECT 'A_similarity' as t, memory_id, round((1 - (embedding <=> '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10)))::numeric,4) as sim FROM memory_embeddings ORDER BY embedding <=> '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10) LIMIT 3;
RESET SESSION AUTHORIZATION;
