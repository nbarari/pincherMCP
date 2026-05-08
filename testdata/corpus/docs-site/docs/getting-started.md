# Getting Started

This guide walks through installing Acme and running your first request.

## Installation

Acme ships as a single binary. Install via:

```sh
go install github.com/example/acme@latest
```

### Verifying the install

Run `acme version` to confirm the binary is on your PATH.

## First request

Create a config file at `~/.acme.yaml`:

```yaml
endpoint: https://api.example.com
token: $ACME_TOKEN
```

Then send a request:

```sh
acme call --method GET /users/me
```

### Common errors

If the request hangs, check that your token is exported. If you see
`401 Unauthorized`, your token is missing or expired.

## Next steps

Read the [API reference](reference/api.md) for the full set of methods,
or the [CLI reference](reference/cli.md) for command options.
