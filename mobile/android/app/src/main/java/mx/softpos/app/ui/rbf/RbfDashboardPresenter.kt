package mx.softpos.app.ui.rbf

import kotlin.math.min

data class RbfLoanUiModel(
    val loanId: String,
    val principalCents: Long,
    val flatFeeCents: Long,
    val repaymentCapCents: Long,
    val repaidCents: Long,
    val remainingCents: Long,
    val holdbackBps: Long,
    val progressPercent: Int,
    val isSettled: Boolean
)

object RbfDashboardPresenter {

    fun buildLoanUiModel(
        loanId: String,
        principalCents: Long,
        flatFeeCents: Long,
        repaidCents: Long,
        holdbackBps: Long
    ): RbfLoanUiModel {
        require(loanId.isNotBlank()) { "Loan ID must not be blank" }
        require(principalCents > 0L) { "Principal cents must be greater than zero" }
        require(flatFeeCents >= 0L) { "Flat fee cents must be non-negative" }
        require(repaidCents >= 0L) { "Repaid cents must be non-negative" }
        require(holdbackBps > 0L) { "Holdback basis points must be greater than zero" }

        val repaymentCapCents = principalCents + flatFeeCents
        val effectiveRepaid = min(repaidCents, repaymentCapCents)
        val remainingCents = repaymentCapCents - effectiveRepaid
        val progressPercent = ((effectiveRepaid * 100L) / repaymentCapCents).toInt()
        val isSettled = remainingCents == 0L

        return RbfLoanUiModel(
            loanId = loanId,
            principalCents = principalCents,
            flatFeeCents = flatFeeCents,
            repaymentCapCents = repaymentCapCents,
            repaidCents = effectiveRepaid,
            remainingCents = remainingCents,
            holdbackBps = holdbackBps,
            progressPercent = progressPercent,
            isSettled = isSettled
        )
    }

    fun calculateSweepForSale(saleGrossCents: Long, holdbackBps: Long, remainingCents: Long): Long {
        require(saleGrossCents >= 0L) { "Sale gross cents must be non-negative" }
        require(holdbackBps > 0L) { "Holdback basis points must be greater than zero" }
        require(remainingCents >= 0L) { "Remaining cents must be non-negative" }

        val rawSweep = (saleGrossCents * holdbackBps + 5000L) / 10000L
        return min(rawSweep, remainingCents)
    }
}
