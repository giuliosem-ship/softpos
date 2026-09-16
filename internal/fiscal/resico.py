from dataclasses import dataclass
from typing import Final

@dataclass(frozen=True)
class ResicoSplitResult:
    base_amount_cents: int
    iva_amount_cents: int
    isr_rate: float
    isr_escrow_cents: int
    total_tax_escrow_cents: int
    net_merchant_cents: int
    is_persona_moral_withholding: bool

def get_resico_monthly_rate(accumulated_sales_cents: int) -> float:
    if accumulated_sales_cents <= 2_500_000:
        return 0.010
    elif accumulated_sales_cents <= 5_000_000:
        return 0.011
    elif accumulated_sales_cents <= 8_333_333:
        return 0.015
    elif accumulated_sales_cents <= 20_833_333:
        return 0.020
    else:
        return 0.025

def calculate_resico_tax_split(
    gross_amount_cents: int,
    accumulated_monthly_sales_cents: int = 0,
    customer_is_persona_moral: bool = False
) -> ResicoSplitResult:
    if gross_amount_cents <= 0 or accumulated_monthly_sales_cents < 0:
        raise ValueError("Invalid input: gross_amount_cents must be > 0 and accumulated_monthly_sales_cents must be >= 0")

    base_amount_cents = round(gross_amount_cents / 1.16)
    iva_amount_cents = gross_amount_cents - base_amount_cents

    if customer_is_persona_moral:
        isr_rate = 0.0125
        isr_escrow_cents = round(base_amount_cents * isr_rate)
        total_tax_escrow_cents = iva_amount_cents
        net_merchant_cents = base_amount_cents - isr_escrow_cents
        is_pm = True
    else:
        isr_rate = get_resico_monthly_rate(accumulated_monthly_sales_cents + base_amount_cents)
        isr_escrow_cents = round(base_amount_cents * isr_rate)
        total_tax_escrow_cents = iva_amount_cents + isr_escrow_cents
        net_merchant_cents = gross_amount_cents - total_tax_escrow_cents
        is_pm = False

    return ResicoSplitResult(
        base_amount_cents=base_amount_cents,
        iva_amount_cents=iva_amount_cents,
        isr_rate=isr_rate,
        isr_escrow_cents=isr_escrow_cents,
        total_tax_escrow_cents=total_tax_escrow_cents,
        net_merchant_cents=net_merchant_cents,
        is_persona_moral_withholding=is_pm
    )
