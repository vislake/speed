-- The migration the concurrency scenario applies, on the first start of two
-- replicas against an empty database.
--
-- The sleep is first and inside the migration's transaction, so the replica that
-- applies this file stays inside the migration run for three seconds while
-- holding the migration mutex. That is the window the other replica has to reach
-- its own acquisition in; without it the two would divide into an applier and a
-- no-op whether or not anything serialises them.
--
-- The last two statements are what makes the run readable afterwards: the
-- session that executed the migration records its own application_name, which is
-- the only thing about the finished database that tells the two replicas apart.
-- Both of them go on to see the same widgets table and the same single row in
-- the record table, so without this row a run with no mutex at all would produce
-- the same evidence as one that serialised its replicas.
SELECT pg_sleep(3);
CREATE TABLE widgets (id INTEGER PRIMARY KEY);
CREATE TABLE dbhost_appliers (role TEXT NOT NULL);
INSERT INTO dbhost_appliers (role) VALUES (current_setting('application_name'));
