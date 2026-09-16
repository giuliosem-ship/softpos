import unittest
from internal.stp.clabe import (
    compute_clabe_control_digit,
    validate_clabe,
    generate_clabe,
    InvalidClabeError
)

class TestStpClabe(unittest.TestCase):
    def test_compute_control_digit_known_valid(self):
        # Known standard Banxico/STP CLABEs:
        # Example 1: 032180000118359719 (BBVA) -> control digit is 9
        # Example 2: 64618012345678901 -> compute control digit
        first_17 = "03218000011835971"
        control = compute_clabe_control_digit(first_17)
        self.assertEqual(control, 9)
        self.assertTrue(validate_clabe("032180000118359719"))

    def test_generate_stp_clabe(self):
        # Generate CLABE under STP bank code (646)
        clabe = generate_clabe(account_seq=1234567, plaza="180")
        self.assertEqual(len(clabe), 18)
        self.assertTrue(clabe.startswith("646180"))
        # Account sequence formatted with leading zeros to 11 digits: 00001234567
        self.assertEqual(clabe[6:17], "00001234567")
        self.assertTrue(validate_clabe(clabe))

    def test_validate_clabe_invalid_length(self):
        self.assertFalse(validate_clabe("6461801234567890")) # 16 digits
        self.assertFalse(validate_clabe("6461801234567890123")) # 19 digits

    def test_validate_clabe_invalid_characters(self):
        self.assertFalse(validate_clabe("64618012345678901A"))

    def test_validate_clabe_wrong_check_digit(self):
        # Valid: 032180000118359719. Corrupt last digit to 8
        self.assertFalse(validate_clabe("032180000118359718"))

    def test_generate_clabe_overflow_raises(self):
        # Sequence greater than 11 digits (99,999,999,999)
        with self.assertRaises(InvalidClabeError):
            generate_clabe(account_seq=100_000_000_000)

if __name__ == "__main__":
    unittest.main()
