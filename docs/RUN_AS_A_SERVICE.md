# Run NanoTDB As A Service

This is a minimal Linux/systemd example for running NanoTDB continuously.

It is based on the service templates already in the repo:

- `cmd/drip/nanotdb.service`
- `cmd/drip/drip.service`

Adjust paths and usernames for your machine.

## 1. Put the binary and config somewhere stable

Example layout:

```text
/home/pi/nanotdb/
  nanotdb
  engine.toml
  drip
  drip.toml
```

Initialize the config once if needed:

```bash
mkdir -p /home/pi/nanotdb
/home/pi/nanotdb/nanotdb --init --config /home/pi/nanotdb/engine.toml
```

## 2. Install the NanoTDB service

Create `/etc/systemd/system/nanotdb.service`:

```ini
[Unit]
Description=NanoTDB Time Series Database
After=network.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=pi
WorkingDirectory=/home/pi
ExecStart=/home/pi/nanotdb/nanotdb -config /home/pi/nanotdb/engine.toml
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

Then reload and start it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable nanotdb.service
sudo systemctl start nanotdb.service
sudo systemctl status nanotdb.service
```

Inspect logs:

```bash
journalctl -u nanotdb.service -f
```

## 3. Optional: install `drip` beside it

If you want host telemetry on a Raspberry Pi or similar Linux box, create
`/etc/systemd/system/drip.service`:

```ini
[Unit]
Description=NanoTDB Metrics Collector (drip)
After=network.target nanotdb.service
Wants=nanotdb.service

[Service]
Type=simple
User=pi
WorkingDirectory=/home/pi
ExecStart=/home/pi/nanotdb/drip -config /home/pi/nanotdb/drip.toml
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

Start it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable drip.service
sudo systemctl start drip.service
sudo systemctl status drip.service
```

Inspect logs:

```bash
journalctl -u drip.service -f
```

## 4. Quick smoke check

After the service is up:

```bash
curl "http://localhost:8428/health"
curl "http://localhost:8428/api/v1/databases"
```

If you get a healthy HTTP response, the service wiring is working.

## 5. Raspberry Pi tuning notes

Keep these in one place rather than copying them into every guide.

If you are running on a Pi or other SD-backed system, the main tradeoff is
between write overhead and recovery conservatism:

- `wal.fsync_policy = "segment"` reduces fsync overhead and is a practical default for many local telemetry setups.
- `wal.fsync_policy = "always"` is the more conservative choice when you want stronger per-append durability.
- `durability.profile = "strict"` is the conservative page/catalog setting.
- `durability.profile = "balanced"` can be a reasonable compromise when you want lower write overhead.

NanoTDB is already fairly flash-friendly because the durable `.dat` files are
small, append-only, and partitioned, but these settings let you choose how hard
you want to lean toward throughput versus recovery conservatism.