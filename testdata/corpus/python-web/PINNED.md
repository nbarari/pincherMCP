# Pinned corpus: `python-web`

A small synthetic Python project that exercises Python AST patterns
common in FastAPI/Django codebases — decorators, async functions,
class inheritance, dependency-injection-shaped cross-file CALLS.

Filed under #1184 (v0.64). The existing `python-app` corpus pins
basic Class + Method + Function + cross-file IMPORTS+CALLS extraction.
`python-web` extends that surface with the shapes that previously
went unverified end-to-end:

- `@decorator` on `def` (sync) AND on `async def`
- Class inheritance — `class User(BaseModel):`
- Multiple classes in one file (currentClass tracker must reset)
- Cross-file CALLS chains spanning three files
- `async def` extraction (AsyncFunctionDef path)

Snapshot pinning means any change to these shapes shows up as a
reviewable diff in `python-web.snapshot.json`. Pre-#1184 the only
gate on the Python AST extractor was the `python-app` corpus, which
shipped no decorators and no inheritance — both common in real
Python.

## Layout

- `pyproject.toml` — establishes the project root.
- `app/__init__.py` — package marker.
- `app/models.py` — `BaseModel` + `User(BaseModel)` + `Item(BaseModel)`.
  Two inherited classes exercise multi-class currentClass tracking.
- `app/deps.py` — `get_current_user()` (sync) + `get_async_session()`
  (async). Stand-in for FastAPI `Depends()` providers.
- `app/routes.py` — three decorated route handlers: one sync, one
  async, one chaining cross-file constructors. Decorators are the
  high-value pin: pre-pincher-Python-AST the regex extractor couldn't
  see `@decorator` correctly.

## Why this matters

Real-world Python codebases are decorator-heavy and inheritance-heavy.
A corpus that doesn't exercise those shapes can't catch regressions
in them. Adding `python-web` to the `CORPORA` list keeps the AST
extractor honest against the patterns that ship in 80% of Python
deployments touched by users (FastAPI + Django web work + Pydantic
schemas).
