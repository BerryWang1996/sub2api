-- The separate mode bit distinguishes an unpublished or damaged policy from
-- an ordinary static Composite group. Group creation sets it atomically.
ALTER TABLE groups ADD COLUMN IF NOT EXISTS fork_ai_routing BOOLEAN NOT NULL DEFAULT FALSE;
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'groups'::regclass AND conname = 'fork_ai_routing_requires_composite') THEN
  ALTER TABLE groups ADD CONSTRAINT fork_ai_routing_requires_composite CHECK (NOT fork_ai_routing OR platform = 'composite');
 END IF;
END $$;
CREATE TABLE IF NOT EXISTS fork_ai_routing_policies (
 group_id bigint PRIMARY KEY REFERENCES groups(id) ON DELETE CASCADE,
 revision bigint NOT NULL CHECK (revision > 0),
 draft jsonb NOT NULL CHECK (jsonb_typeof(draft) = 'object'),
 published jsonb CHECK (published IS NULL OR jsonb_typeof(published) = 'object'),
 updated_at timestamptz NOT NULL DEFAULT NOW()
);
