-- Migration 005: add owner_email to projects for self-serve signup.
--
-- The /signup endpoint creates a project per signing-up email. Each
-- project has exactly one owner_email; an email may own multiple
-- projects (the Cloud Hobby 1-project cap is enforced at the
-- application layer based on subscription state, not at the schema
-- layer, so Pro users can have many projects under one email).
--
-- The partial index keeps the index size proportional to the number
-- of signed-up projects, not the total project count (some projects
-- may not have an owner_email if they were created via direct API
-- key issuance for testing).
ALTER TABLE projects ADD COLUMN owner_email TEXT;
CREATE INDEX idx_projects_owner_email
    ON projects(owner_email)
    WHERE owner_email IS NOT NULL;
