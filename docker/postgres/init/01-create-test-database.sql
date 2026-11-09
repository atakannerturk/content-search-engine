-- Runs once when the PostgreSQL volume is first initialised.
-- The integration tests use a dedicated database so they can truncate freely.
CREATE DATABASE content_test;
GRANT ALL PRIVILEGES ON DATABASE content_test TO app;
