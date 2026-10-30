package mx.softpos.app.core.money

data class SplitPreview(
    val grossCents: Long,
    val baseCents: Long,
    val ivaCents: Long,
    val isrCents: Long,
    val platformFeeCents: Long,
    val rbfSweepCents: Long,
    val netMerchantCents: Long,
    val isPersonaMoralWithholding: Boolean = false,
    val codiSavingsCents: Long = platformFeeCents
)

object MoneySplitCalculator {

    fun getResicoMonthlyRateBps(accumulatedSalesCents: Long): Long {
        require(accumulatedSalesCents >= 0L) { "Accumulated sales cents must be non-negative" }
        return when {
            accumulatedSalesCents <= 2_500_000L -> 100L
            accumulatedSalesCents <= 5_000_000L -> 110L
            accumulatedSalesCents <= 8_333_333L -> 150L
            accumulatedSalesCents <= 20_833_333L -> 200L
            else -> 250L
        }
    }

    fun calculatePreview(
        grossCents: Long,
        platformFeeBps: Long = 290L,
        isrRateBps: Long = 100L,
        rbfHoldbackBps: Long = 1000L,
        isPersonaMoral: Boolean = false
    ): SplitPreview {
        require(grossCents > 0L) { "Gross cents must be positive" }
        require(platformFeeBps >= 0L) { "Platform fee basis points must be non-negative" }
        require(isrRateBps >= 0L) { "ISR rate basis points must be non-negative" }
        require(rbfHoldbackBps >= 0L) { "RBF holdback basis points must be non-negative" }

        val baseCents = (grossCents * 100L + 58L) / 116L
        val ivaCents = grossCents - baseCents
        val effectiveIsrBps = if (isPersonaMoral) 125L else isrRateBps
        val isrCents = (baseCents * effectiveIsrBps + 5000L) / 10000L
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
            netMerchantCents = netMerchantCents,
            isPersonaMoralWithholding = isPersonaMoral
        )
    }
}
