INSERT INTO memory_embeddings (workspace_id, memory_id, content, embedding) VALUES
('11111111-1111-1111-1111-111111111111', 'mem-a1', 'AI agent architecture patterns', '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10)),
('11111111-1111-1111-1111-111111111111', 'mem-a2', 'Database design best practices', '[0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0, 0.1]'::vector(10)),
('22222222-2222-2222-2222-222222222222', 'mem-b1', 'Machine learning pipeline optimization', '[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]'::vector(10)),
('22222222-2222-2222-2222-222222222222', 'mem-b2', 'Cloud infrastructure deployment', '[0.5, 0.6, 0.7, 0.8, 0.9, 1.0, 0.1, 0.2, 0.3, 0.4]'::vector(10));
