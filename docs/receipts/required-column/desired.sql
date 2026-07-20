CREATE SCHEMA app;

CREATE TABLE app.bookings (
  id bigint PRIMARY KEY,
  status text NOT NULL
);
