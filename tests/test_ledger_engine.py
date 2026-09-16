import unittest
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
from internal.ledger.engine import (
    LedgerEngine,
    AccountNotFoundError,
    DuplicateAccountError
)

class TestLedgerEngine(unittest.TestCase):
    def setUp(self):
        self.engine = LedgerEngine()
        # Initialize standard system accounts
        self.engine.create_account("ASSET:ACQUIRER_RECEIVABLE", AccountType.ASSET, "Acquirer Clearing")
        self.engine.create_account("ASSET:STP_CONCENTRATOR", AccountType.ASSET, "STP Liquid Concentrator")
        self.engine.create_account("REVENUE:FEES", AccountType.REVENUE, "Platform Fee Revenue")
        self.engine.create_account("ASSET:RBF_LOANS_RECEIVABLE", AccountType.ASSET, "RBF Loan Balance")

    def test_create_and_query_account(self):
        acc = self.engine.create_account("LIABILITY:MERCHANT:m100:AVAILABLE", AccountType.LIABILITY, "Merchant Available")
        self.assertEqual(acc.code, "LIABILITY:MERCHANT:m100:AVAILABLE")
        self.assertEqual(self.engine.get_balance("LIABILITY:MERCHANT:m100:AVAILABLE").amount_cents, 0)

    def test_duplicate_account_raises(self):
        with self.assertRaises(DuplicateAccountError):
            self.engine.create_account("ASSET:ACQUIRER_RECEIVABLE", AccountType.ASSET, "Duplicate")

    def test_non_existent_account_raises(self):
        with self.assertRaises(AccountNotFoundError):
            self.engine.get_balance("NON_EXISTENT_ACCOUNT")

    def test_post_journal_entry_updates_balances(self):
        self.engine.create_account("LIABILITY:MERCHANT:m100:AVAILABLE", AccountType.LIABILITY, "Merchant Available")
        
        entries = [
            Entry(
                account_id="ASSET:STP_CONCENTRATOR",
                direction=Direction.DEBIT,
                money=Money(50000, "MXN") # $500.00 MXN deposited
            ),
            Entry(
                account_id="LIABILITY:MERCHANT:m100:AVAILABLE",
                direction=Direction.CREDIT,
                money=Money(50000, "MXN")
            )
        ]
        journal = JournalEntry(
            id="tx_dep_01",
            timestamp=datetime.now(),
            description="SPEI Inbound Deposit",
            entries=entries
        )
        self.engine.post_transaction(journal)

        # Asset accounts increase with DEBIT
        self.assertEqual(self.engine.get_balance("ASSET:STP_CONCENTRATOR").amount_cents, 50000)
        # Liability accounts increase with CREDIT
        self.assertEqual(self.engine.get_balance("LIABILITY:MERCHANT:m100:AVAILABLE").amount_cents, 50000)

    def test_record_split_payment(self):
        merchant_id = "merch_oxxo_01"
        self.engine.create_account(f"LIABILITY:MERCHANT:{merchant_id}:AVAILABLE", AccountType.LIABILITY, "Available")
        self.engine.create_account(f"LIABILITY:MERCHANT:{merchant_id}:TAX_ESCROW", AccountType.LIABILITY, "Tax Escrow")

        # Initial RBF loan disbursement of $500.00 MXN (50,000 cents)
        disburse_entries = [
            Entry("ASSET:RBF_LOANS_RECEIVABLE", Direction.DEBIT, Money(50000, "MXN")),
            Entry(f"LIABILITY:MERCHANT:{merchant_id}:AVAILABLE", Direction.CREDIT, Money(50000, "MXN"))
        ]
        self.engine.post_transaction(JournalEntry("loan_disb_01", datetime.now(), "Loan Disbursement", disburse_entries))
        self.assertEqual(self.engine.get_balance("ASSET:RBF_LOANS_RECEIVABLE").amount_cents, 50000)

        # SoftPOS Sale of $1,000.00 MXN (100,000 cents)
        # Platform Fee: 2.9% + IVA = $33.64 MXN (3,364 cents)
        # Tax Escrow: 1.5% ISR + 16% IVA = $150.80 MXN (15,080 cents)
        # RBF Loan Sweep: 10% = $100.00 MXN (10,000 cents) amortizes loan to 40,000
        # Net Available: $715.56 MXN (71,556 cents) increases available funds to 50000 + 71556 = 121,556
        # Total Credits: 3364 + 15080 + 10000 + 71556 = 100,000 cents (Equal to Debit)

        journal = self.engine.record_split_payment(
            gross_mxn_cents=100000,
            merchant_id=merchant_id,
            platform_fee_cents=3364,
            tax_escrow_cents=15080,
            rbf_sweep_cents=10000
        )

        self.assertTrue(journal.is_balanced())
        self.assertEqual(self.engine.get_balance("ASSET:ACQUIRER_RECEIVABLE").amount_cents, 100000)
        self.assertEqual(self.engine.get_balance("REVENUE:FEES").amount_cents, 3364)
        self.assertEqual(self.engine.get_balance(f"LIABILITY:MERCHANT:{merchant_id}:TAX_ESCROW").amount_cents, 15080)
        self.assertEqual(self.engine.get_balance("ASSET:RBF_LOANS_RECEIVABLE").amount_cents, 40000)
        self.assertEqual(self.engine.get_balance(f"LIABILITY:MERCHANT:{merchant_id}:AVAILABLE").amount_cents, 121556)

    def test_split_payment_negative_amounts_raises(self):
        with self.assertRaises(ValueError):
            self.engine.record_split_payment(
                gross_mxn_cents=10000,
                merchant_id="m1",
                platform_fee_cents=-500,
                tax_escrow_cents=1000,
                rbf_sweep_cents=0
            )

    def test_split_payment_excess_deductions_raises(self):
        # Deductions exceed gross amount
        with self.assertRaises(ValueError):
            self.engine.record_split_payment(
                gross_mxn_cents=1000,
                merchant_id="m1",
                platform_fee_cents=600,
                tax_escrow_cents=500,
                rbf_sweep_cents=0
            )

if __name__ == "__main__":
    unittest.main()
