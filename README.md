# ocpp-tap

A self-hosted OCPP 1.6-J proxy and telemetry collector. A minimal forwarder passes frames unchanged between your chargers and your CPMS; a read-only tap turns a copy of the traffic into live events. No third-party cloud in the charging path. Binary and image: `octap`.

Status: early development, nothing usable yet. See [`docs/PRD.md`](docs/PRD.md) for what is planned and [`docs/adr/`](docs/adr/) for architecture decisions.

## Development

```sh
make ci    # gofmt, go vet, golangci-lint, go test -race
make e2e   # charger -> proxy -> CPMS through the test harness
```

Contributors and agents: read [`CLAUDE.md`](CLAUDE.md) first.

## License

TODO(license): not decided yet (MIT or Apache-2.0). Files copied from [`wolffseb/headless-csms`](https://github.com/wolffseb/headless-csms) keep their MIT notice; see `NOTICE`.
