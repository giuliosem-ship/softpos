package mx.softpos.app.ui.pos

import java.util.Locale
import kotlinx.coroutines.flow.StateFlow
import mx.softpos.app.core.money.SplitPreview

data class PosFormattedSummary(
    val formattedGrossMxn: String,
    val formattedNetMxn: String,
    val formattedTaxEscrowMxn: String,
    val formattedRbfSweepMxn: String,
    val formattedFeeMxn: String,
    val codiSavingsBadge: String?,
    val voiceConfirmationText: String
)

object PosScreenDescriptor {
    fun formatMxnCents(cents: Long): String {
        val wholePesos = cents / 100L
        val centavos = kotlin.math.abs(cents % 100L)
        val formattedWhole = String.format(Locale.US, "%,d", wholePesos)
        val formattedCents = String.format(Locale.US, "%02d", centavos)
        return "$$formattedWhole.$formattedCents MXN"
    }

    fun buildSummary(state: PosUiState): PosFormattedSummary {
        val preview = state.splitPreview
        val gross = state.amountCents
        val net = preview?.netMerchantCents ?: 0L
        val taxEscrow = if (preview != null) preview.ivaCents + preview.isrCents else 0L
        val rbfSweep = preview?.rbfSweepCents ?: 0L
        val fee = preview?.platformFeeCents ?: 0L
        val codiBadge = if (preview != null && preview.codiSavingsCents > 0L) {
            "Ahorra ${formatMxnCents(preview.codiSavingsCents)} con CoDi (0% comisión)"
        } else {
            null
        }
        return PosFormattedSummary(
            formattedGrossMxn = formatMxnCents(gross),
            formattedNetMxn = formatMxnCents(net),
            formattedTaxEscrowMxn = formatMxnCents(taxEscrow),
            formattedRbfSweepMxn = formatMxnCents(rbfSweep),
            formattedFeeMxn = formatMxnCents(fee),
            codiSavingsBadge = codiBadge,
            voiceConfirmationText = "Pago recibido por ${formatMxnCents(gross)}"
        )
    }
}
