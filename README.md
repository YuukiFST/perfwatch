# perfwatch

When a Windows PC turns slow, the process to blame has often exited or calmed down by the time you open Task Manager; perfwatch records CPU and RAM usage continuously, detects peak episodes, and shows which processes consumed the most, overall and during each peak.

It is a single Go binary for Windows 10 and 11.
The sampler sits behind an interface, so a Linux backend can be added later without touching storage or UI.

## Status

Under construction: the commands land one pull request at a time.

## Commands

| Command | What it does |
|---------|--------------|
| `perfwatch run [--print] [--for 10m] [--record file.jsonl] [--replay file.jsonl]` | Sample CPU and RAM every second until interrupted. `--print` shows one line per sample; `--record` saves the raw samples and `--replay` feeds them back instead of the live system. |
| `perfwatch version` | Print the version. |

`run --print` reads like this:

```
12:28:13  cpu 47.7%  ram 81.7%  unattributed 1.6%  | vmmem 37.9%, com.docker.backend.exe x2 3.1%, docker.exe x2 1.2%, brave.exe x18 0.8%, conhost.exe x19 0.8%
```

- Per-process CPU is a share of the whole machine: 100% means every logical CPU was busy.
- Instances of one program are summed under its name, with the instance count after `x`; `svchost.exe` is split by its `-k` service group.
- `unattributed` is busy CPU that no sampled process accounts for: interrupt and DPC time, and processes that started and exited between two samples.

## Development

```sh
go vet ./...
go test ./...                      # end-to-end: builds perfwatch and diffs its output against testdata/golden
go test ./cmd/perfwatch -update    # rewrite the golden files after an intended output change
go build ./cmd/perfwatch
```

Read the diff of every rewritten golden file before committing it: it is the behavior change.

## License

[MIT](LICENSE)
