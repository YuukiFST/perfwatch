# perfwatch

When a Windows PC turns slow, the process to blame has often exited or calmed down by the time you open Task Manager; perfwatch records CPU and RAM usage continuously, detects peak episodes, and shows which processes consumed the most, overall and during each peak.

It is a single Go binary for Windows 10 and 11.
The sampler sits behind an interface, so a Linux backend can be added later without touching storage or UI.

## Install

```sh
perfwatch install      # start the collector now and at every logon; no admin needed
perfwatch uninstall    # stop it and remove the autostart; the data stays
```

`install` adds `perfwatch` to `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, pointing at the `perfwatch.exe` it was run from, so keep the binary where it is.
Running it again, or the registered command while the collector runs, still leaves one collector.
At logon Windows opens a console window for the collector; it closes as soon as the collector starts and lets go of it.
`install --service` installs a Windows service that starts at boot instead, from an elevated prompt, and removes the logon entry.
The service runs as LocalSystem, so it runs a copy in `%ProgramFiles%\perfwatch` and keeps its data in `%ProgramData%\perfwatch`, both open to administrators only; read that data from an elevated prompt with `PERFWATCH_DATA_DIR` set to it.
`uninstall` removes the service and its copy too, from an elevated prompt, and keeps the data.

## Commands

| Command | What it does |
|---------|--------------|
| `perfwatch install [--service]`, `perfwatch uninstall` | Start the collector now and at every logon, or stop it and its autostart; see Install. |
| `perfwatch run [--print] [--for 10m] [--record file.jsonl] [--replay file.jsonl] [--data-dir dir]` | Sample CPU and RAM every second until interrupted; what autostart runs. `--print` shows one line per sample; `--record` saves the raw samples and `--replay` feeds them back instead of the live system, into the directory `PERFWATCH_DATA_DIR` or `--data-dir` names. |
| `perfwatch top [--since 24h] [--peaks-only] [--limit 20] [--json]` | Rank the programs by CPU time over the range: CPU-seconds, average and maximum CPU, maximum private RAM, instances. `--peaks-only` counts only the seconds inside peak episodes. |
| `perfwatch peaks [--since 7d] [--json]` | List the peak episodes in the range, newest first, with their maximum CPU and RAM, unattributed CPU, pre-roll and top programs, then the process instances behind them with who started each and its command line, and the coding agent's tool calls during each. |
| `perfwatch actions [--since 24h] [--sort time\|cpu\|ram] [--limit 50] [--json]` | List a coding agent's tool calls with the machine's average and maximum CPU, maximum RAM and RAM growth over each, and the peak episode each overlaps. `--sort cpu` puts the calls that drove CPU highest first, `--sort ram` those that grew RAM most. |
| `perfwatch hook` | Record one tool call from a Claude Code hook's JSON on stdin; see Coding agent actions. |
| `perfwatch serve [--port 7313]` | Serve the dashboard on `http://127.0.0.1:7313/`, loopback only, until Ctrl+C. |
| `perfwatch export --html report.html [--since 7d]` | Write the dashboard of the range to one HTML file that opens from disk, with no server and no network. |
| `perfwatch version` | Print the version. |

`run --print` reads like this:

```
12:28:13  cpu 47.7%  ram 81.7%  unattributed 1.6%  | vmmem 37.9%, com.docker.backend.exe x2 3.1%, docker.exe x2 1.2%, brave.exe x18 0.8%, conhost.exe x19 0.8%
```

- Per-process CPU is a share of the whole machine: 100% means every logical CPU was busy.
- Instances of one program are summed under its name, with the instance count after `x`; `svchost.exe` is split by its `-k` service group.
- `--since` takes Go durations (`90m`, `24h`) or whole days (`7d`); `top` starts the range on the minute.
- `top` reads the per-minute top 20s, so a program that ranked out of a minute adds nothing for it there. `(unattributed)` is always a row of its own, and `(other programs)` is the busy CPU of the programs not listed: cut by `--limit`, ranked out of a minute or a peak second, or in the minute still being collected.
- `unattributed` is busy CPU that no sampled process accounts for: interrupt and DPC time, processes that started and exited between two samples, and the last partial second of every process that exited.

## Data

`run` keeps everything in SQLite (WAL mode) at `%LOCALAPPDATA%\perfwatch\perfwatch.db`; set `PERFWATCH_DATA_DIR` to use another directory.

| What | Granularity | Kept |
|------|-------------|------|
| Machine CPU, RAM and unattributed CPU | 1 s | 7 days |
| Machine rollups: average, maximum, p95 | 1 min | 1 year |
| Per-program CPU-seconds, max CPU, max private RAM, instances: the top 20 by CPU plus the top 20 by RAM | 1 min | 90 days |
| Peak episodes: machine usage and the top 10 programs by CPU plus the top 10 by private RAM, from 30 s before the start until the episode cools down | 1 s | 180 days after the end |
| Peak episodes: the top 10 process instances by CPU plus the top 10 by private RAM, each with its command line and running parent chain | 1 s | 180 days after the end |
| Coding agent tool calls: tool, command, start and end | per call | 180 days after the end |

Retention runs when `run` starts and once an hour after.
An interval longer than 5 s (the machine slept or the collector was stopped) is not stored, so it shows as a gap.

- One `run` writes to a data directory at a time; a second one exits with an error.
- Machine rollups are written with every sample, so killing `run` loses none of them; the per-program breakdown of the minute in progress is lost.
- Replaying samples already stored changes nothing, even after their 1 s rows have expired.

## Peak episodes

An episode starts when CPU reaches 85% or RAM reaches 90% for 3 consecutive samples, and ends after 10 consecutive samples with CPU below 75% and RAM below 85%.
Its end is the last sample before that cool-down; a gap in the samples, or `run` stopping, ends it too.
To change the thresholds, put a `perfwatch.toml` in the data directory; every key is optional, an unknown key is an error, and `preroll_seconds` is at most 300:

```toml
[peaks]
cpu_start_percent = 85
cpu_end_percent = 75
ram_start_percent = 90
ram_end_percent = 85
start_samples = 3
end_samples = 10
preroll_seconds = 30
```

## Coding agent actions

A program name says `node.exe` used the CPU, not which command.
Two records answer that.

- Every process instance a peak episode names keeps its command line and the running processes that started it, as in `claude.exe[100] > bash.exe[200] > node.exe`.
  A process run by another account, such as a `svchost.exe`, denies its command line to a collector that is not elevated.
- `perfwatch hook`, registered as a Claude Code hook, records each tool call: the tool, its command (or its input as JSON for tools without one), when it started and when it returned.
  `perfwatch actions` then shows what the machine did during each call, and `perfwatch peaks` lists the calls during each episode.
  A shell tool's command is not on any command line Claude Code starts for the PowerShell tool, so only the hook records it.

Register the hook in `~/.claude/settings.json` for every tool, with the path of your `perfwatch.exe`; `async` keeps it from delaying the agent:

```json
{
  "hooks": {
    "PreToolUse":         [{ "matcher": "", "hooks": [{ "type": "command", "command": "C:\\path\\to\\perfwatch.exe", "args": ["hook"], "async": true }] }],
    "PostToolUse":        [{ "matcher": "", "hooks": [{ "type": "command", "command": "C:\\path\\to\\perfwatch.exe", "args": ["hook"], "async": true }] }],
    "PostToolUseFailure": [{ "matcher": "", "hooks": [{ "type": "command", "command": "C:\\path\\to\\perfwatch.exe", "args": ["hook"], "async": true }] }]
  }
}
```

The hook writes to the same data directory as the collector, so it uses `PERFWATCH_DATA_DIR` too.
A call whose end never arrives is shown as `open`, and its usage is read up to 10 minutes after its start.
A call run in the background returns at once, so its own window is short; its processes still show in `peaks`.
Processes that start and exit within one second still count as unattributed CPU.

## Dashboard

`perfwatch serve` shows, in pt-BR:

- CPU and RAM over the chosen range (1 hour to 30 days), with the start thresholds, gaps where the collector did not run and peak episodes shaded; drag across the chart to zoom in.
- Each episode on click: duration, trigger, maximum CPU and RAM, unattributed CPU, its pre-roll and its top programs.
- The program ranking of the range, also counting only peak seconds.

Every chart has a text summary and its data as a table.
The page, its script and [uPlot](https://github.com/leeoniya/uPlot) 1.6.32 (MIT, `internal/cli/web/vendor/uPlot.LICENSE`) are embedded in the binary; the page loads nothing from the network.
The server answers only requests addressed to `127.0.0.1` or `localhost` on its port.

`perfwatch export` writes the same views for a fixed range into one file, with its data, styles and scripts inline; it has no range buttons or zoom.
The file names every program in the ranking and its path on disk, so read it before sharing it.

## Log

`run` writes JSON lines to `perfwatch.log` in the data directory: its start and stop, each episode, and once a minute its own CPU (as a share of one core) and working set.
At 5 MB the log rotates to `perfwatch.log.1`, keeping three old files.

## Development

```sh
go vet ./...
go test ./...                      # end-to-end: builds perfwatch and diffs its output against testdata/golden
go test ./cmd/perfwatch -update    # rewrite the golden files after an intended output change
PERFWATCH_NOW=2026-03-02T09:10:00Z perfwatch top    # fix now, and show times in UTC, as the tests do
go build ./cmd/perfwatch
```

Read the diff of every rewritten golden file before committing it: it is the behavior change.

## License

[MIT](LICENSE)
