"""Pydantic-shaped models — exercises ClassDef inheritance + per-class
Methods. Django ORM and FastAPI bodies converge on this pattern.
"""

from typing import Optional


class BaseModel:
    """Stand-in for pydantic.BaseModel — keeps the corpus dependency-free
    while preserving the inheritance shape the extractor must handle."""

    def dict(self) -> dict:
        return self.__dict__


class User(BaseModel):
    """Inherits from BaseModel — exercises base-class parent linkage in
    the Python AST extractor's Method parenting."""

    def __init__(self, name: str, email: Optional[str] = None) -> None:
        self.name = name
        self.email = email

    def display_name(self) -> str:
        """Override-style method — same name as a hypothetical base helper.
        Exercises Method extraction with explicit self-typed receiver."""
        return self.name


class Item(BaseModel):
    """Second class in the same file — exercises currentClass tracker
    resetting between class blocks."""

    def __init__(self, sku: str, price: float) -> None:
        self.sku = sku
        self.price = price

    def total(self, quantity: int) -> float:
        return self.price * quantity
