# drip — the optional host metrics collector

`drip` is a small, opt-in companion binary that collects host metrics on a
Linux box and POSTs them into NanoTDB using line protocol. It is intended for
edge boxes — Raspberry Pi, NAS appliances, small Linux servers — where you
want one place to look at CPU, memory, disk, network, load, and temperature
without setting up a separate metrics pipeline.

NanoTDB's default `dashboard.json` is pre-wired to the metric names `drip`
emits, so a Pi + `nanotdb` + `drip` deployment is a usable dashboard from the
first start.

You do not need `drip` to use NanoTDB. Any source that can POST line protocol
to `/api/v1/import` works.

---

## What it collects

| Collector        | Sample metrics                                                                |
|------------------|-------------------------------------------------------------------------------|
| `cpu`            | `cpu.user`, `cpu.system`, `cpu.idle`, `cpu.iowait`, `cpu.busy_pct`, `cpu.temp_mdeg`, `sys.uptime_sec` |
| `memory`         | `mem.total`, `mem.free`, `mem.available`, `mem.cached`, `mem.swaptotal`, `mem.swapfree` |
| `process`        | `proc.{exe}.count`, `proc.{exe}.rss_bytes`, `proc.{exe}.cpu_pct` per configured executable |
| `disk`           | per block device: `disk.{dev}.reads`, `disk.{dev}.busy_pct`, `disk.{dev}.iops`, `disk.{dev}.read_kbps`, `disk.{dev}.write_kbps`; per mount: `diskfs.{mount}.bytes_used`, `diskfs.{mount}.used_pct` |
| `io`             | `io.pgpgin`, `io.pgpgout`, `io.pswpin`, `io.pswpout`                          |
| `sd_write_probe` | `disk.sd_write_probe_ms` — measured write+fsync latency on the configured directory |
| `network`        | per interface: `net.{iface}.rx_bytes`, `net.{iface}.tx_bytes`, plus packet and error counters |
| `loadavg`        | `sys.load1`, `sys.load5`, `sys.load15`, `sys.procs_running`, `sys.procs_total` |
| `onewire`        | `temp.{name}.mdeg` for DS18B20 sensors, with friendly-name mapping            |

Each collector can be enabled or disabled independently in `drip.toml`.

---

## Configuration

`drip.toml` controls the server endpoint, the destination database, the
collection cadence, and per-collector options.

Minimum:

```toml
[drip]
server_url = "http://localhost:8428"
database = "metrics"
collection_interval_ms = 10000
timeout_ms = 5500
```

Per-collector enable flags follow:

```toml
[collectors.cpu]
enabled = true

[collectors.memory]
enabled = true

[collectors.process]
enabled = true
exe_names = ["drip", "nanotdb"]

[collectors.disk]
enabled = true

[collectors.sd_write_probe]
enabled = true
directory = "/tmp"
bytes = 32000
every_n_cycles = 6
metric = "disk.sd_write_probe_ms"

[collectors.network]
enabled = true
skip = ["lo", "br-*", "docker*"]

[collectors.loadavg]
enabled = true

[collectors.onewire]
enabled = true
auto_discover = true
base_path = "/sys/bus/w1/devices"
max_valid_mdeg = 85000

[collectors.onewire.devices]
"28-0120481ddd35" = "office_wet"
"28-01214506d428" = "out_dry"
"28-0121450f31ef" = "office_dry"
```

See the in-repo [cmd/drip/drip.toml](../cmd/drip/drip.toml) for the full
annotated example, which is what the default `dashboard.json` is tuned to.

---

## Running

Build it from source:

```bash
go build -o drip ./cmd/drip
```

Or download a prebuilt binary from
[Releases](https://github.com/aymanhs/nanotdb/releases/latest). `drip` is
published for Linux ARM/x86_64 targets only.

Run it pointing at a running NanoTDB:

```bash
./drip --config drip.toml
```

It will start posting samples on every `collection_interval_ms` tick.

To verify, check the destination DB once data starts arriving:

```bash
curl "http://localhost:8428/api/v1/metrics?db=metrics"
```

---

## Running as a service

`drip` has a systemd template alongside the NanoTDB one. See
[RUN_AS_A_SERVICE.md](RUN_AS_A_SERVICE.md) for the recommended `pi`-user
layout and unit files.

---

## Notes on edge use

- The `sd_write_probe` collector measures write+fsync latency on a directory
  you choose. Point it at an SD-card-backed path on a Pi and you'll see the
  card's actual responsiveness over time — extremely useful when SD cards
  start degrading.
- Process CPU is emitted as a per-cycle delta. The first cycle starts at 0.
- One-wire devices that aren't explicitly named fall back to using their
  device ID as the metric suffix.
