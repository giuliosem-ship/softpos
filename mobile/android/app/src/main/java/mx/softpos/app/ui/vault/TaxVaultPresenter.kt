package mx.softpos.app.ui.vault

import mx.softpos.app.core.money.MoneySplitCalculator

data class TaxVaultState(
    val merchantRfc: String,
    val periodYearMonth: String,
    val monthlySalesCents: Long,
    val currentResicoRateBps: Long,
    val ivaEscrowCents: Long,
    val isrEscrowCents: Long,
    val totalTaxEscrowCents: Long,
    val cfdiCount: Int
)

data class AccountantPackagePayload(
    val merchantRfc: String,
    val periodYearMonth: String,
    val totalTaxEscrowCents: Long,
    val ivaEscrowCents: Long,
    val isrEscrowCents: Long,
    val resicoTierBps: Long,
    val cfdiUuids: List<String>,
    val summaryCsv: String
)

object TaxVaultPresenter {

    fun buildVaultState(
        merchantRfc: String,
        periodYearMonth: String,
        monthlySalesCents: Long,
        ivaEscrowCents: Long,
        isrEscrowCents: Long,
        cfdiCount: Int
    ): TaxVaultState {
        require(merchantRfc.isNotBlank()) { "Merchant RFC cannot be blank" }
        require(periodYearMonth.isNotBlank()) { "Period Year Month cannot be blank" }
        require(monthlySalesCents >= 0L) { "Monthly Sales Cents must be non-negative" }
        require(ivaEscrowCents >= 0L) { "IVA Escrow Cents must be non-negative" }
        require(isrEscrowCents >= 0L) { "ISR Escrow Cents must be non-negative" }
        require(cfdiCount >= 0) { "CFDI Count must be non-negative" }

        val currentResicoRateBps = MoneySplitCalculator.getResicoMonthlyRateBps(monthlySalesCents)
        val totalTaxEscrowCents = ivaEscrowCents + isrEscrowCents

        return TaxVaultState(
            merchantRfc = merchantRfc,
            periodYearMonth = periodYearMonth,
            monthlySalesCents = monthlySalesCents,
            currentResicoRateBps = currentResicoRateBps,
            ivaEscrowCents = ivaEscrowCents,
            isrEscrowCents = isrEscrowCents,
            totalTaxEscrowCents = totalTaxEscrowCents,
            cfdiCount = cfdiCount
        )
    }

    fun buildAccountantPackage(state: TaxVaultState, cfdiUuids: List<String>): AccountantPackagePayload {
        val summaryCsv = "RFC,PERIODO,VENTAS_CENTS,IVA_ESCROW_CENTS,ISR_ESCROW_CENTS,TOTAL_ESCROW_CENTS,TASA_BPS,CFDI_COUNT\n" +
            "${state.merchantRfc},${state.periodYearMonth},${state.monthlySalesCents},${state.ivaEscrowCents},${state.isrEscrowCents},${state.totalTaxEscrowCents},${state.currentResicoRateBps},${cfdiUuids.size}"

        return AccountantPackagePayload(
            merchantRfc = state.merchantRfc,
            periodYearMonth = state.periodYearMonth,
            totalTaxEscrowCents = state.totalTaxEscrowCents,
            ivaEscrowCents = state.ivaEscrowCents,
            isrEscrowCents = state.isrEscrowCents,
            resicoTierBps = state.currentResicoRateBps,
            cfdiUuids = cfdiUuids,
            summaryCsv = summaryCsv
        )
    }
}
