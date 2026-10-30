package mx.softpos.app.core.money

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class MoneySplitCalculatorTest {
    @Test
    fun `calculatePreview splits 1160 MXN (116000 cents) with exact integer precision`() {
        val preview = MoneySplitCalculator.calculatePreview(
            grossCents = 116_000L,
            platformFeeBps = 300L, // 3.00% including IVA example
            isrRateBps = 100L,     // 1.00% RESICO
            rbfHoldbackBps = 1000L // 10.00% RBF Sweep
        )

        assertEquals(100_000L, preview.baseCents)
        assertEquals(16_000L, preview.ivaCents)
        assertEquals(1_000L, preview.isrCents)
        assertEquals(3_480L, preview.platformFeeCents)
        assertEquals(11_600L, preview.rbfSweepCents)
        assertEquals(83_920L, preview.netMerchantCents)
    }
}
