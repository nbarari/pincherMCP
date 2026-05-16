"""FastAPI-shaped route handlers — exercises decorators on Functions
AND async route handlers AND DI-style cross-file CALLS.
"""

from app.deps import get_current_user, get_async_session
from app.models import User, Item


def route(path: str):
    """Synthetic decorator stand-in for `@app.get(path)` etc. — keeps the
    corpus dependency-free while preserving the Decorator AST shape the
    extractor must handle."""

    def wrap(fn):
        return fn

    return wrap


@route("/users/me")
def read_current_user() -> dict:
    """Sync route handler with a decorator — exercises decorator
    extraction + cross-file CALLS edge to deps.get_current_user."""
    user = get_current_user()
    return user.dict()


@route("/users/{user_id}")
async def read_user(user_id: int) -> dict:
    """Async route handler with a decorator — exercises
    @route + AsyncFunctionDef + cross-file CALLS chain."""
    session = await get_async_session()
    user = get_current_user()
    return {"user": user.dict(), "session": session, "id": user_id}


@route("/items")
def list_items() -> list:
    """Route handler that constructs a project-local Class — exercises
    cross-file CALLS edge to models.Item constructor."""
    items = [Item(sku="A", price=1.0), Item(sku="B", price=2.5)]
    return [{"sku": i.sku, "total": i.total(1)} for i in items]
