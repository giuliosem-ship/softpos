package mx.softpos.app.ui.rbf

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class RbfDashboardPresenterTest {

    @Test
    fun `buildLoanUiModel computes repayment cap progress percent and clamps overpayments`() {
        val activeLoan = RbfDashboardPresenter.buildLoanUiModel(
            loanId = "loan_mx_01",
            principalCents = 1_000_000L,
            flatFeeCents = 120_000L,
            repaidCents = 560_000L,
            holdbackBps = 1000L
        )

        assertEquals("loan_mx_01", activeLoan.loanId)
        assertEquals(1_120_000L, activeLoan.repaymentCapCents)
        assertEquals(560_000L, activeLoan.repaidCents)
        assertEquals(560_000L, activeLoan.remainingCents)
        assertEquals(50, activeLoan.progressPercent)
        assertFalse(activeLoan.isSettled)

        // Overpaid beyond cap should clamp remainingCents to 0 and progressPercent to 100
        val settledLoan = RbfDashboardPresenter.buildLoanUiModel(
            loanId = "loan_mx_01",
            principalCents = 1_000_000L,
            flatFeeCents = 120_000L,
            repaidCents = 1_200_000L,
            holdbackBps = 1000L
        )
        assertEquals(0L, settledLoan.remainingCents)
        assertEquals(100, settledLoan.progressPercent)
        assertTrue(settledLoan.isSettled)
    }

    @Test
    fun `calculateSweepForSale applies holdback bps and caps at remainingCents`() {
        // 10% (1000 bps) of 50,000 cents = 5,000 cents, remaining is 20,000 cents -> 5,000 cents
        assertEquals(5_000L, RbfDashboardPresenter.calculateSweepForSale(50_000L, 1000L, 20_000L))

        // 10% of 50,000 cents = 5,000 cents, but only 1,200 cents remaining -> capped at 1,200 cents
        assertEquals(1_200L, RbfDashboardPresenter.calculateSweepForSale(50_000L, 1000L, 1_200L))
    }

    @Test
    fun `rejects invalid loan parameters and negative sweep inputs`() {
        assertThrows(IllegalArgumentException::class.java) {
            RbfDashboardPresenter.buildLoanUiModel("loan_1", 0L, 100L, 0L, 1000L)
        }
        assertThrows(IllegalArgumentException::class.java) {
            RbfDashboardPresenter.buildLoanUiModel("loan_1", 1000L, -1L, 0L, 1000L)
        }
        assertThrows(IllegalArgumentException::class.java) {
            RbfDashboardPresenter.buildLoanUiModel("loan_1", 1000L, 100L, -1L, 1000L)
        }
        assertThrows(IllegalArgumentException::class.java) {
            RbfDashboardPresenter.buildLoanUiModel("loan_1", 1000L, 100L, 0L, 0L)
        }
        assertThrows(IllegalArgumentException::class.java) {
            RbfDashboardPresenter.calculateSweepForSale(-100L, 1000L, 500L)
        }
        assertThrows(IllegalArgumentException::class.java) {
            RbfDashboardPresenter.calculateSweepForSale(100L, 1000L, -1L)
        }
    }
}
