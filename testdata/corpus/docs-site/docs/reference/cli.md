# CLI Reference

The `acme` binary exposes the API via a thin command wrapper.

## Global flags

- `--config <path>` — override the default config location
- `--token <value>` — override the config-file token
- `--verbose` — emit debug logs to stderr

## Commands

### acme call

Send a one-shot request.

```sh
acme call --method GET /users/me
```

### acme login

Interactive token-acquisition flow.

### acme version

Print the binary version and exit.
