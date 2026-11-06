CREATE TABLE public.events (id bigint PRIMARY KEY, x integer NOT NULL, y integer NOT NULL, CONSTRAINT events_check CHECK (x BETWEEN 1 AND 5 AND y > 0));
