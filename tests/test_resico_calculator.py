import unittest
from internal.fiscal.resico import (
    calculate_resico_tax_split,
    get_resico_monthly_rate,
    ResicoSplitResult
)

class TestResicoCalculator(unittest.TestCase):
    def test_monthly_brackets(self):
        # Brackets in cents:
        # <= 25,000 MXN (2,500,000 cents): 1.0%
        self.assertEqual(get_resico_monthly_rate(1_000_000), 0.010)
        self.assertEqual(get_resico_monthly_rate(2_500_000), 0.010)
        
        # <= 50,000 MXN (5,000,000 cents): 1.1%
        self.assertEqual(get_resico_monthly_rate(3_500_000), 0.011)
        self.assertEqual(get_resico_monthly_rate(5_000_000), 0.011)

        # <= 83,333.33 MXN: 1.5%
        self.assertEqual(get_resico_monthly_rate(6_000_000), 0.015)

        # <= 208,333.33 MXN: 2.0%
        self.assertEqual(get_resico_monthly_rate(10_000_000), 0.020)

        # <= 291,666.66 MXN: 2.5%
        self.assertEqual(get_resico_monthly_rate(25_000_000), 0.025)

    def test_b2c_sale_tax_split(self):
        # Sale of $1,160.00 MXN (116,000 cents) to Public in General (Persona Física)
        # Base: $1,000.00 MXN (100,000 cents)
        # IVA (16%): $160.00 MXN (16,000 cents)
        # Monthly accumulated sales: $20,000.00 MXN (2,000,000 cents) -> ISR Rate 1.0%
        # ISR Apartado (1.0% of 100,000): $10.00 MXN (1,000 cents)
        # Total Tax Escrow (IVA + ISR): $170.00 MXN (17,000 cents)
        # Net for Merchant: 116,000 - 17,000 = $990.00 MXN (99,000 cents)

        result: ResicoSplitResult = calculate_resico_tax_split(
            gross_amount_cents=116000,
            accumulated_monthly_sales_cents=2000000,
            customer_is_persona_moral=False
        )

        self.assertEqual(result.base_amount_cents, 100000)
        self.assertEqual(result.iva_amount_cents, 16000)
        self.assertEqual(result.isr_rate, 0.010)
        self.assertEqual(result.isr_escrow_cents, 1000)
        self.assertEqual(result.total_tax_escrow_cents, 17000)
        self.assertEqual(result.net_merchant_cents, 99000)
        self.assertFalse(result.is_persona_moral_withholding)

    def test_b2b_persona_moral_withholding(self):
        # Sale to Persona Moral (e.g. Corporate card)
        # Art. 113-J LISR mandates 1.25% ISR withholding by the PM
        # Sale $1,160.00 MXN (Base 100,000 cents)
        # ISR Retenido por PM (1.25%): 1,250 cents
        # Merchant must only escrow IVA (16,000 cents) because PM pays the 1.25% ISR directly to SAT
        result = calculate_resico_tax_split(
            gross_amount_cents=116000,
            accumulated_monthly_sales_cents=2000000,
            customer_is_persona_moral=True
        )

        self.assertEqual(result.base_amount_cents, 100000)
        self.assertEqual(result.iva_amount_cents, 16000)
        self.assertEqual(result.isr_rate, 0.0125)
        self.assertEqual(result.isr_escrow_cents, 1250)
        self.assertTrue(result.is_persona_moral_withholding)
        # Only IVA is placed into tax escrow by the merchant (the PM withholds the ISR)
        self.assertEqual(result.total_tax_escrow_cents, 16000)
        self.assertEqual(result.net_merchant_cents, 100000 - 1250) # Net receivable after PM withholding

    def test_invalid_negative_amount_raises(self):
        with self.assertRaises(ValueError):
            calculate_resico_tax_split(-5000, 100000)

if __name__ == "__main__":
    unittest.main()
