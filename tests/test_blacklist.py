import unittest
from internal.identity.blacklist import (
    BlacklistService,
    BlacklistStatus,
    BlacklistedEntityError
)

class TestBlacklistService(unittest.TestCase):
    def setUp(self):
        self.service = BlacklistService()
        # Seed known EFOS cases
        self.service.add_entry("EFO110101XYZ", BlacklistStatus.DEFINITIVO, oficio_sat="500-05-2023-1234")
        self.service.add_entry("PRE120202ABC", BlacklistStatus.PRESUNTO, oficio_sat="500-05-2024-5678")
        self.service.add_entry("DES130303QWE", BlacklistStatus.DESVIRTUADO, oficio_sat="500-05-2024-9999")
        self.service.add_entry("SEN140404RTY", BlacklistStatus.SENTENCIA_FAVORABLE, oficio_sat="500-05-2024-7777")

    def test_clean_rfc_is_eligible(self):
        # Clean RFC not in SAT 69-B list
        self.assertEqual(self.service.get_status("GARM850101ABC"), BlacklistStatus.CLEAN)
        self.assertTrue(self.service.validate_merchant_eligibility("GARM850101ABC"))

    def test_desvirtuado_and_sentencia_are_eligible(self):
        # Taxpayers who cleared their status in legal proceedings
        self.assertEqual(self.service.get_status("DES130303QWE"), BlacklistStatus.DESVIRTUADO)
        self.assertTrue(self.service.validate_merchant_eligibility("DES130303QWE"))

        self.assertEqual(self.service.get_status("SEN140404RTY"), BlacklistStatus.SENTENCIA_FAVORABLE)
        self.assertTrue(self.service.validate_merchant_eligibility("SEN140404RTY"))

    def test_definitivo_efo_blocks_onboarding(self):
        # Confirmed simulated invoicing entity (EFOS Definitivo)
        self.assertEqual(self.service.get_status("EFO110101XYZ"), BlacklistStatus.DEFINITIVO)
        with self.assertRaises(BlacklistedEntityError):
            self.service.validate_merchant_eligibility("EFO110101XYZ")

    def test_presunto_efo_blocks_onboarding(self):
        # Presumed simulated invoicing entity (EFOS Presunto under audit)
        self.assertEqual(self.service.get_status("PRE120202ABC"), BlacklistStatus.PRESUNTO)
        with self.assertRaises(BlacklistedEntityError):
            self.service.validate_merchant_eligibility("PRE120202ABC")

if __name__ == "__main__":
    unittest.main()
