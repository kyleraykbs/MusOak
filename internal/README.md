
## Notes on the database

WAL and a five-second `busy_timeout` are set on every connection, and writes are
retried briefly when SQLite says the database is locked. A search that finds a
provider playlist it already knows writes nothing: writing on every search is
what made a busy database fail one.
