-- PARADOX runs as an ordinary role that owns only its own database:
-- no superuser, no extensions. This proves migrations need nothing more.
CREATE ROLE paradox LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD 'paradox';
CREATE DATABASE paradox OWNER paradox;
\connect paradox
ALTER SCHEMA public OWNER TO paradox;
