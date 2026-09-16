import unittest
from internal.identity.rfc_curp import (
    validate_rfc,
    parse_rfc_type,
    validate_curp,
    parse_curp,
    InvalidRFCError,
    InvalidCURPError
)

class TestRfcCurp(unittest.TestCase):
    def test_validate_rfc_persona_fisica(self):
        # Valid PF RFC (13 characters)
        self.assertTrue(validate_rfc("GARM850101ABC"))
        self.assertEqual(parse_rfc_type("GARM850101ABC"), "PERSONA_FISICA")

    def test_validate_rfc_persona_moral(self):
        # Valid PM RFC (12 characters)
        self.assertTrue(validate_rfc("ABC850101XYZ"))
        self.assertEqual(parse_rfc_type("ABC850101XYZ"), "PERSONA_MORAL")

    def test_validate_rfc_genericos(self):
        # Mexican Public in general
        self.assertTrue(validate_rfc("XAXX010101000"))
        self.assertEqual(parse_rfc_type("XAXX010101000"), "GENERICO")
        # Foreign generic
        self.assertTrue(validate_rfc("XEXX010101000"))
        self.assertEqual(parse_rfc_type("XEXX010101000"), "GENERICO")

    def test_invalid_rfc_raises(self):
        self.assertFalse(validate_rfc("INVALID_RFC_123"))
        with self.assertRaises(InvalidRFCError):
            parse_rfc_type("INVALID_RFC")

    def test_validate_and_parse_curp(self):
        # CURP: GARM850101HDFRRN09
        # Birth date: 1985-01-01, Gender: H, State: DF
        curp = "GARM850101HDFRRN09"
        self.assertTrue(validate_curp(curp))
        
        parsed = parse_curp(curp)
        self.assertEqual(parsed["birth_date"], "1985-01-01")
        self.assertEqual(parsed["gender"], "HOMBRE")
        self.assertEqual(parsed["state_code"], "DF")

    def test_parse_curp_mujer(self):
        # Female CURP
        curp = "GARM920512MNLRRN01"
        self.assertTrue(validate_curp(curp))
        parsed = parse_curp(curp)
        self.assertEqual(parsed["birth_date"], "1992-05-12")
        self.assertEqual(parsed["gender"], "MUJER")
        self.assertEqual(parsed["state_code"], "NL")

    def test_invalid_curp_raises(self):
        self.assertFalse(validate_curp("12345"))
        with self.assertRaises(InvalidCURPError):
            parse_curp("INVALID_CURP_FORMAT")

if __name__ == "__main__":
    unittest.main()
