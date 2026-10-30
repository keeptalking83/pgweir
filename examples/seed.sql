-- Demo data for pgweir. Runs once, as the superuser, when the container is
-- created. pgweir itself runs as "app": not a superuser, but the owner of the
-- database and its tables, with REPLICATION and CREATEDB, like the
-- application role on a managed Postgres.
CREATE ROLE app LOGIN PASSWORD 'app' REPLICATION CREATEDB;
ALTER DATABASE shop OWNER TO app;

-- The agent's own role. It gets no privileges on main; grant_agent.sql gives
-- it privileges on a fork after "pgweir branch create".
CREATE ROLE agent LOGIN PASSWORD 'agent';

SET ROLE app;

CREATE TABLE customers (
  id    bigint PRIMARY KEY,
  email text NOT NULL UNIQUE,
  tier  text NOT NULL DEFAULT 'basic'
);

CREATE TABLE orders (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  customer_id bigint NOT NULL REFERENCES customers (id),
  status      text NOT NULL DEFAULT 'PENDING',
  amount      numeric(12, 2) NOT NULL,
  note        text
);

INSERT INTO customers
SELECT g, 'customer' || g || '@example.com', CASE WHEN g % 10 = 0 THEN 'gold' ELSE 'basic' END
FROM generate_series(1, 100) AS g;

INSERT INTO orders (customer_id, status, amount)
SELECT 1 + (g * 7) % 100, 'PENDING', 10 + (g * 37) % 490
FROM generate_series(1, 1000) AS g;
