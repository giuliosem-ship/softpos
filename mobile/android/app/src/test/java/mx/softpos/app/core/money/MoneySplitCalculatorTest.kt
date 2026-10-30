package mx.softpos.app.core.money

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class MoneySplitCalculatorTest {

    @Test
    fun `getResicoMonthlyRateBps returns exact basis points for all 5 SAT RESICO tiers`() {
        assertEquals(100L, MoneySplitCalculator.getResicoMonthlyRateBps(0L))
        assertEquals(100L, MoneySplitCalculator.getResicoMonthlyRateBps(2_500_000L))
        assertEquals(110L, MoneySplitCalculator.getResicoMonthlyRateBps(2_500_001L))
        assertEquals(110L, MoneySplitCalculator.getResicoMonthlyRateBps(5_000_000L))
        assertEquals(150L, MoneySplitCalculator.getResicoMonthlyRateBps(5_000_001L))
        assertEquals(150L, MoneySplitCalculator.getResicoMonthlyRateBps(8_333_333L))
        assertEquals(200L, MoneySplitCalculator.getResicoMonthlyRateBps(8_333_334L))
        assertEquals(200L, MoneySplitCalculator.getResicoMonthlyRateBps(20_833_333L))
        assertEquals(250L, MoneySplitCalculator.getResicoMonthlyRateBps(20_833_334L))
        assertEquals(250L, MoneySplitCalculator.getResicoMonthlyRateBps(350_000_000L))
    }

    @Test
    fun `getResicoMonthlyRateBps rejects negative accumulated sales`() {
        assertThrows(IllegalArgumentException::class.java) {
            MoneySplitCalculator.getResicoMonthlyRateBps(-1L)
        }
    }

    @Test
    fun `calculatePreview splits 1160 MXN (116000 cents) with exact integer precision`() {
        val preview = MoneySplitCalculator.calculatePreview(
            grossCents = 116_000L,
            platformFeeBps = 300L,
            isrRateBps = 100L,
            rbfHoldbackBps = 1000L,
            isPersonaMoral = false
        )

        assertEquals(116_000L, preview.grossCents)
        assertEquals(100_000L, preview.baseCents)
        assertEquals(16_000L, preview.ivaCents)
        assertEquals(1_000L, preview.isrCents)
        assertEquals(3_480L, preview.platformFeeCents)
        assertEquals(11_600L, preview.rbfSweepCents)
        assertEquals(83_920L, preview.netMerchantCents)
        assertFalse(preview.isPersonaMoralWithholding)
        assertEquals(3_480L, preview.codiSavingsCents)
    }

    @Test
    fun `calculatePreview applies 125 bps (1_25 percent Art 113-J LISR) when isPersonaMoral is true`() {
        val preview = MoneySplitCalculator.calculatePreview(
            grossCents = 116_000L,
            platformFeeBps = 290L,
            isrRateBps = 100L,
            rbfHoldbackBps = 1000L,
            isPersonaMoral = true
        )

        assertEquals(100_000L, preview.baseCents)
        assertEquals(16_000L, preview.ivaCents)
        assertEquals(1_250L, preview.isrCents) // 1.25% of 100,000 cents
        assertEquals(3_364L, preview.platformFeeCents) // 2.90% of 116,000 cents
        assertEquals(11_600L, preview.rbfSweepCents)
        assertEquals(83_786L, preview.netMerchantCents)
        assertTrue(preview.isPersonaMoralWithholding)
        assertEquals(3_364L, preview.codiSavingsCents)
    }

    @Test
    fun `calculatePreview rejects non-positive grossCents`() {
        assertThrows(IllegalArgumentException::class.java) {
            MoneySplitCalculator.calculatePreview(grossCents = 0L)
        }
    }
}
