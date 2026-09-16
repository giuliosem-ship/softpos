from dataclasses import dataclass, field
from enum import Enum
from typing import List, Dict, Optional
from datetime import datetime

class CurrencyMismatchError(Exception):
    """Exception raised for currency mismatch in Money operations."""
    pass

class UnbalancedJournalError(Exception):
    """Exception raised for unbalanced journal entries."""
    pass

@dataclass(frozen=True)
class Money:
    amount_cents: int
    currency: str = "MXN"

    def __add__(self, other):
        if self.currency != other.currency:
            raise CurrencyMismatchError("Currencies do not match")
        return Money(self.amount_cents + other.amount_cents, self.currency)

    def __sub__(self, other):
        if self.currency != other.currency:
            raise CurrencyMismatchError("Currencies do not match")
        return Money(self.amount_cents - other.amount_cents, self.currency)

    def __eq__(self, other):
        return (self.amount_cents == other.amount_cents) and (self.currency == other.currency)

    def __lt__(self, other):
        if self.currency != other.currency:
            raise CurrencyMismatchError("Currencies do not match")
        return self.amount_cents < other.amount_cents

    def __le__(self, other):
        if self.currency != other.currency:
            raise CurrencyMismatchError("Currencies do not match")
        return self.amount_cents <= other.amount_cents

    def __gt__(self, other):
        if self.currency != other.currency:
            raise CurrencyMismatchError("Currencies do not match")
        return self.amount_cents > other.amount_cents

    def __ge__(self, other):
        if self.currency != other.currency:
            raise CurrencyMismatchError("Currencies do not match")
        return self.amount_cents >= other.amount_cents

    def __str__(self):
        return f"${self.amount_cents / 100:.2f} {self.currency}"

class AccountType(Enum):
    ASSET = "ASSET"
    LIABILITY = "LIABILITY"
    EQUITY = "EQUITY"
    REVENUE = "REVENUE"
    EXPENSE = "EXPENSE"

@dataclass(frozen=True)
class Account:
    id: str = ""
    code: str = ""
    name: str = ""
    type: Optional[AccountType] = None
    account_type: Optional[AccountType] = None
    balance: Money = field(default_factory=lambda: Money(0, "MXN"))

    def __post_init__(self):
        if not self.id and self.code:
            object.__setattr__(self, "id", self.code)
        if self.type is None and self.account_type is not None:
            object.__setattr__(self, "type", self.account_type)
        elif self.account_type is None and self.type is not None:
            object.__setattr__(self, "account_type", self.type)

class Direction(Enum):
    DEBIT = "DEBIT"
    CREDIT = "CREDIT"

@dataclass(frozen=True)
class Entry:
    account_id: str
    direction: Direction
    money: Money

@dataclass(frozen=True)
class JournalEntry:
    id: str
    timestamp: datetime
    description: str
    entries: List[Entry]
    metadata: Dict[str, str] = field(default_factory=dict)

    def __post_init__(self):
        if not self.entries:
            raise ValueError("Entries list cannot be empty")
        if not self.is_balanced():
            raise UnbalancedJournalError("Journal entries are unbalanced")

    def is_balanced(self) -> bool:
        total_debit = sum(entry.money.amount_cents for entry in self.entries if entry.direction == Direction.DEBIT)
        total_credit = sum(entry.money.amount_cents for entry in self.entries if entry.direction == Direction.CREDIT)
        return total_debit == total_credit

    @property
    def total_debit(self) -> Money:
        return Money(sum(entry.money.amount_cents for entry in self.entries if entry.direction == Direction.DEBIT), "MXN")

    @property
    def total_credit(self) -> Money:
        return Money(sum(entry.money.amount_cents for entry in self.entries if entry.direction == Direction.CREDIT), "MXN")
