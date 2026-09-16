import unittest
from datetime import datetime
from internal.ledger.models import (
    Money,
    AccountType,
    Account,
    Entry,
    Direction,
    JournalEntry,
    UnbalancedJournalError,
    CurrencyMismatchError
)

class TestLedgerModels(unittest.TestCase):
    def test_money_arithmetic_and_comparison(self):
        m1 = Money(amount_cents=1000, currency="MXN")
        m2 = Money(amount_cents=500, currency="MXN")
        
        # Addition
        m3 = m1 + m2
        self.assertEqual(m3.amount_cents, 1500)
        self.assertEqual(m3.currency, "MXN")
        
        # Subtraction
        m4 = m1 - m2
        self.assertEqual(m4.amount_cents, 500)
        
        # Comparisons
        self.assertTrue(m1 > m2)
        self.assertTrue(m2 < m1)
        self.assertEqual(m1, Money(1000, "MXN"))
        
        # Display
        self.assertEqual(str(m1), "$10.00 MXN")

    def test_money_currency_mismatch(self):
        m_mxn = Money(1000, "MXN")
        m_usd = Money(1000, "USD")
        with self.assertRaises(CurrencyMismatchError):
            _ = m_mxn + m_usd

    def test_account_creation(self):
        acc = Account(
            id="acc_001",
            code="LIABILITY:MERCHANT:test_123:AVAILABLE",
            name="Merchant Available Funds",
            type=AccountType.LIABILITY
        )
        self.assertEqual(acc.code, "LIABILITY:MERCHANT:test_123:AVAILABLE")
        self.assertEqual(acc.type, AccountType.LIABILITY)
        self.assertEqual(acc.balance.amount_cents, 0)

    def test_balanced_journal_entry(self):
        # SoftPOS payment of $100.00 MXN (10,000 cents)
        # Platform fee 2.9% + IVA = $3.36 MXN (336 cents)
        # Tax Escrow = $16.00 MXN (1600 cents)
        # Net Available = $80.64 MXN (8064 cents)
        
        entries = [
            Entry(
                account_id="acc_acquirer",
                direction=Direction.DEBIT,
                money=Money(10000, "MXN")
            ),
            Entry(
                account_id="acc_revenue_fee",
                direction=Direction.CREDIT,
                money=Money(336, "MXN")
            ),
            Entry(
                account_id="acc_tax_escrow",
                direction=Direction.CREDIT,
                money=Money(1600, "MXN")
            ),
            Entry(
                account_id="acc_merchant_available",
                direction=Direction.CREDIT,
                money=Money(8064, "MXN")
            )
        ]
        
        journal = JournalEntry(
            id="tx_001",
            timestamp=datetime.now(),
            description="Tap to Phone $100 MXN payment",
            entries=entries
        )
        
        self.assertEqual(len(journal.entries), 4)
        self.assertTrue(journal.is_balanced())
        self.assertEqual(journal.total_debit.amount_cents, 10000)
        self.assertEqual(journal.total_credit.amount_cents, 10000)

    def test_unbalanced_journal_entry_raises(self):
        entries = [
            Entry(
                account_id="acc_acquirer",
                direction=Direction.DEBIT,
                money=Money(10000, "MXN")
            ),
            Entry(
                account_id="acc_merchant_available",
                direction=Direction.CREDIT,
                money=Money(9000, "MXN")  # Unbalanced by 1,000 cents
            )
        ]
        
        with self.assertRaises(UnbalancedJournalError):
            JournalEntry(
                id="tx_invalid",
                timestamp=datetime.now(),
                description="Unbalanced transaction",
                entries=entries
            )

    def test_empty_journal_entry_raises(self):
        with self.assertRaises(ValueError):
            JournalEntry(
                id="tx_empty",
                timestamp=datetime.now(),
                description="Empty transaction",
                entries=[]
            )

if __name__ == "__main__":
    unittest.main()
