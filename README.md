# perfwatch

When a Windows PC turns slow, the process to blame has often exited or calmed down by the time you open Task Manager; perfwatch records CPU and RAM usage continuously, detects peak episodes, and shows which processes consumed the most, overall and during each peak.

It is a single Go binary for Windows 10 and 11.
The sampler sits behind an interface, so a Linux backend can be added later without touching storage or UI.

## Status

Under construction: the commands land one pull request at a time.

## Commands

| Command | What it does |
|---------|--------------|
| `perfwatch version` | Print the version. |

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
