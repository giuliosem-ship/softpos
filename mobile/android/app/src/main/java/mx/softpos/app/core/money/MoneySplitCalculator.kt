package mx.softpos.app.core.money

/**
 * Pure integer (`Long` cents and basis points) tax and fee split calculator for SoftPOS Android.
 * Zero floating-point operations to match Go backend (`pkg/fiscal/resico.go` and `pkg/ledger/engine.go`).
 */
data class SplitPreview(
    val grossCents: Long,
    val baseCents: Long,
    val ivaCents: Long,
    val isrCents: Long,
    val platformFeeCents: Long,
    val rbfSweepCents: Long,
    val netMerchantCents: Long
)

object MoneySplitCalculator {
    fun calculatePreview(
        grossCents: Long,
        platformFeeBps: Long = 290L,
        isrRateBps: Long = 100L,
        rbfHoldbackBps: Long = 1000L
    ): SplitPreview {
        require(grossCents > 0) { "grossCents must be positive" }
        val baseCents = (grossCents * 100L + 58L) / 116L
        val ivaCents = grossCents - baseCents
        val isrCents = (baseCents * isrRateBps + 5000L) / 10000L
        val platformFeeCents = (grossCents * platformFeeBps + 5000L) / 10000L
        val rbfSweepCents = (grossCents * rbfHoldbackBps + 5000L) / 10000L
        val netMerchantCents = grossCents - ivaCents - isrCents - platformFeeCents - rbfSweepCents

        return SplitPreview(
            grossCents = grossCents,
            baseCents = baseCents,
            ivaCents = ivaCents,
            isrCents = isrCents,
            platformFeeCents = platformFeeCents,
            rbfSweepCents = rbfSweepCents,
            netMerchantCents = netMerchantCents
        )
    }
}
