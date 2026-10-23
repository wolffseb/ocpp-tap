# ocpp-tap

A self-hosted OCPP 1.6-J proxy and telemetry collector. A minimal forwarder passes frames unchanged between your chargers and your CPMS; a read-only tap turns a copy of the traffic into live events. No third-party cloud in the charging path. Binary and image: `octap`.

Status: early development. The forwarder works (SP1); the tap, API and web UI do not exist yet. See [`docs/PRD.md`](docs/PRD.md) for what is planned and [`docs/adr/`](docs/adr/) for architecture decisions.

## Running the forwarder

```sh
octap run -config /data/config.json
```

```json
{
  "version": 1,
  "listen": ":9000",
  "chargers": [
    {
      "id": "CP1",
      "password": "the password you enter on the charger",
      "upstream_url": "wss://cpms.example.com/ocpp/CP1",
      "upstream_password": "the password the CPMS gave you for CP1"
    }
  ]
}
```

Point the charger at `ws://<octap-host>:9000/ocpp/CP1` (any path works; the identity is the last segment), with username `CP1` and the `password` above. Optional fields: `tls_cert_file` and `tls_key_file` (serve `wss://` to chargers), `upstream_ca_file` (extra CAs for `wss://` upstreams), `upstream_username` (default: the charger id). If the CPMS cannot be reached, octap refuses the charger, so it stays in its own offline mode.

## Development

```sh
make ci    # gofmt, go vet, golangci-lint, go test -race
make e2e   # charger -> proxy -> CPMS through the test harness
```

Contributors and agents: read [`CLAUDE.md`](CLAUDE.md) first.

## License

TODO(license): not decided yet (MIT or Apache-2.0). Files copied from [`wolffseb/headless-csms`](https://github.com/wolffseb/headless-csms) keep their MIT notice; see `NOTICE`.
