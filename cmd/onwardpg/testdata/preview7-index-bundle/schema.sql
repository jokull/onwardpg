CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint PRIMARY KEY,
  email text NOT NULL,
  org bigint,
  name text
);
CREATE INDEX accounts_email_org_idx ON app.accounts (email, org);
