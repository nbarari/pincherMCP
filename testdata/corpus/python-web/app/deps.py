"""Dependency providers — exercises module-level Functions that the
routes module references via cross-file IMPORTS + CALLS edges. FastAPI
`Depends(get_current_user)` and Django middleware are the canonical
real-world shapes this targets.
"""

from app.models import User


def get_current_user() -> User:
    """Synchronous DI provider — referenced from routes.py as a callable
    value (the IMPORTS edge resolves to here; no CALLS edge unless the
    route body invokes the function directly)."""
    return User(name="anonymous")


async def get_async_session() -> dict:
    """Async DI provider — exercises AsyncFunctionDef extraction."""
    return {"session_id": "stub"}
