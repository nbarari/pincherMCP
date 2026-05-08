# API Reference

The Acme HTTP API is RESTful and accepts/returns JSON.

## Authentication

All requests require a bearer token in the `Authorization` header.

### Token rotation

Tokens expire after 24 hours. Rotate via `POST /auth/refresh`.

## Endpoints

### GET /users/me

Returns the authenticated user's profile.

### POST /requests

Submits a new request. The body must include `method`, `path`, and
optionally `headers`.

### DELETE /sessions/{id}

Terminates an active session.

## Error codes

- `400` — malformed request
- `401` — missing or invalid token
- `429` — rate limit exceeded
