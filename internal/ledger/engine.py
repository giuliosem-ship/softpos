from dataclasses import dataclass, field
from collections import defaultdict
from typing import Dict, List, Optional
from datetime import datetime

from internal.ledger.models import (
    Money,
    AccountType,
    Direction,
    Account,
    Entry,
    JournalEntry,
    UnbalancedJournalError
)

class AccountNotFoundError(Exception):
    pass

class DuplicateAccountError(Exception):
    pass

class LedgerEngine:
    def __init__(self):
        self._accounts: Dict[str, Account] = {}
        self._entries_by_account: Dict[str, List[Entry]] = defaultdict(list)
        self._journals: List[JournalEntry] = []

    def create_account(self, code: str, account_type: AccountType, name: str) -> Account:
        if code in self._accounts:
            raise DuplicateAccountError(f"Account with code '{code}' already exists.")
        account = Account(id=code, code=code, name=name, type=account_type)
        self._accounts[code] = account
        return account

    def get_account(self, account_code: str) -> Account:
        if account_code not in self._accounts:
            raise AccountNotFoundError(f"Account with code '{account_code}' not found.")
        return self._accounts[account_code]

    def get_balance(self, account_code: str) -> Money:
        account = self.get_account(account_code)
        entries = self._entries_by_account[account_code]
        
        total_debits = sum(e.money.amount_cents for e in entries if e.direction == Direction.DEBIT)
        total_credits = sum(e.money.amount_cents for e in entries if e.direction == Direction.CREDIT)

        # Normal balances: Asset & Expense = Debits - Credits
        # Liability, Equity & Revenue = Credits - Debits
        acct_type = account.type or account.account_type
        if acct_type in (AccountType.ASSET, AccountType.EXPENSE):
            balance_cents = total_debits - total_credits
        else:
            balance_cents = total_credits - total_debits

        return Money(amount_cents=balance_cents, currency="MXN")

    def post_transaction(self, journal_entry: JournalEntry) -> None:
        if not journal_entry.is_balanced():
            raise UnbalancedJournalError(f"Journal entry {journal_entry.id} is not balanced.")

        # Validate that all touched accounts exist before mutating state (Atomicity)
        for entry in journal_entry.entries:
            if entry.account_id not in self._accounts:
                raise AccountNotFoundError(f"Account '{entry.account_id}' does not exist.")

        # Atomic commit to internal ledger store
        for entry in journal_entry.entries:
            self._entries_by_account[entry.account_id].append(entry)
        self._journals.append(journal_entry)

    def record_split_payment(
        self,
        gross_mxn_cents: int,
        merchant_id: str,
        platform_fee_cents: int,
        tax_escrow_cents: int,
        rbf_sweep_cents: int
    ) -> JournalEntry:
        if (
            gross_mxn_cents < 0
            or platform_fee_cents < 0
            or tax_escrow_cents < 0
            or rbf_sweep_cents < 0
        ):
            raise ValueError("Transaction and split amounts cannot be negative.")

        total_deductions = platform_fee_cents + tax_escrow_cents + rbf_sweep_cents
        if total_deductions > gross_mxn_cents:
            raise ValueError("Deductions exceed gross amount.")

        net_available_cents = gross_mxn_cents - total_deductions

        entries = [
            Entry(
                account_id="ASSET:ACQUIRER_RECEIVABLE",
                direction=Direction.DEBIT,
                money=Money(gross_mxn_cents, "MXN")
            ),
            Entry(
                account_id="REVENUE:FEES",
                direction=Direction.CREDIT,
                money=Money(platform_fee_cents, "MXN")
            ),
            Entry(
                account_id=f"LIABILITY:MERCHANT:{merchant_id}:TAX_ESCROW",
                direction=Direction.CREDIT,
                money=Money(tax_escrow_cents, "MXN")
            ),
            Entry(
                account_id="ASSET:RBF_LOANS_RECEIVABLE",
                direction=Direction.CREDIT,
                money=Money(rbf_sweep_cents, "MXN")
            ),
            Entry(
                account_id=f"LIABILITY:MERCHANT:{merchant_id}:AVAILABLE",
                direction=Direction.CREDIT,
                money=Money(net_available_cents, "MXN")
            )
        ]

        journal = JournalEntry(
            id=f"split_tx_{datetime.now().strftime('%Y%m%d%H%M%S%f')}",
            timestamp=datetime.now(),
            description=f"SoftPOS Split Payment - Merchant {merchant_id}",
            entries=entries
        )

        self.post_transaction(journal)
        return journal
