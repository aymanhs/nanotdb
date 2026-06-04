# NanoTDB Hello World

This is the fastest copy/paste path to see what NanoTDB is good at:

- start one local server
- write a few metrics
- query them back
- inspect the stored files offline

## 1. Create a data directory

```bash
mkdir -p ~/nanotdb-data
```

## 2. Initialize NanoTDB

```bash
./nanotdb --init --config ~/nanotdb-data/engine.toml
```

That writes a default `engine.toml` and creates the root directory layout.

## 3. Start the server

```bash
./nanotdb --config ~/nanotdb-data/engine.toml
```

Leave that terminal running.

## 4. Write a few metrics

In another terminal:

```bash
curl -X POST "http://localhost:8428/api/v1/import" \
  -d $'demo/room.temp 21.5\ndemo/room.humidity 48\ndemo/room.temp 21.8'
```

Or include explicit timestamps:

```bash
curl -X POST "http://localhost:8428/api/v1/import" \
  -d $'demo/room.temp 21.5 1715000000000000000\ndemo/room.temp 21.8 1715000060000000000'
```

## 5. Query the data back

Latest point style query:

```bash
curl "http://localhost:8428/api/v1/query?query=demo/room.temp"
```

Range query:

```bash
curl "http://localhost:8428/api/v1/query_range?query=demo/room.temp"
```

## 6. Inspect the files offline

This is one of NanoTDB's main advantages: you can stop thinking of the database
as a black box and inspect its local files directly.

While the server is still running, the newest data may still be in the WAL and
the in-memory open page rather than a flushed `.dat` file. So inspect the WAL
first:

```bash
./nanocli inspect wal --root ~/nanotdb-data --db demo --verbose
```

Then stop `nanotdb` cleanly with `Ctrl+C` in the server terminal and inspect the
database again:

```bash
./nanocli inspect db --root ~/nanotdb-data --db demo --verbose
./nanocli inspect dat --root ~/nanotdb-data --db demo --verbose
```

You should now have a directory that looks roughly like this:

```text
~/nanotdb-data/
  engine.toml
  demo/
    catalog.json
    manifest.toml
    demo.wal
    data-YYYY-MM-DD.dat
```

## Why this matters

If your use case is small, local, and operationally simple, this flow is often
more useful than either:

- dumping numeric history into plain logs and trying to query it later
- running a larger TSDB stack that solves problems you do not have

Next steps:

- See [GETTING_STARTED.md](GETTING_STARTED.md) for installation and longer examples.
- See [RUN_AS_A_SERVICE.md](RUN_AS_A_SERVICE.md) if you want to keep it running under systemd.