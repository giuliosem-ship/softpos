# internal/stp/clabe.py

from dataclasses import dataclass, field
from typing import List

CLABE_WEIGHTS: List[int] = [3, 7, 1, 3, 7, 1, 3, 7, 1, 3, 7, 1, 3, 7, 1, 3, 7]

class InvalidClabeError(Exception):
    """Custom exception for invalid CLABE operations."""
    pass

def compute_clabe_control_digit(first_17_digits: str) -> int:
    if len(first_17_digits) != 17 or not first_17_digits.isdigit():
        raise InvalidClabeError("First 17 digits must be exactly 17 numeric characters.")
    
    sum_products = sum((int(digit) * CLABE_WEIGHTS[i]) % 10 for i, digit in enumerate(first_17_digits))
    return (10 - (sum_products % 10)) % 10

def validate_clabe(clabe_str: str) -> bool:
    if len(clabe_str) != 18 or not clabe_str.isdigit():
        return False
    
    first_17_digits = clabe_str[:17]
    expected_control_digit = compute_clabe_control_digit(first_17_digits)
    return int(clabe_str[17]) == expected_control_digit

def generate_clabe(account_seq: int, plaza: str = '180', bank: str = '646') -> str:
    if not (0 <= account_seq <= 99_999_999_999):
        raise InvalidClabeError("Account sequence must be between 0 and 99,999,999,999.")
    if len(plaza) != 3 or not plaza.isdigit():
        raise InvalidClabeError("Plaza must be exactly 3 numeric characters.")
    if len(bank) != 3 or not bank.isdigit():
        raise InvalidClabeError("Bank code must be exactly 3 numeric characters.")
    
    account_seq_str = f'{account_seq:011d}'
    first_17_digits = bank + plaza + account_seq_str
    control_digit = compute_clabe_control_digit(first_17_digits)
    return first_17_digits + str(control_digit)
