package mx.softpos.app.ui.pos

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test

class PosScreenDescriptorTest {

    @Test
    fun `formatMxnCents formats zero, cents, and thousands accurately`() {
        assertEquals("$0.00 MXN", PosScreenDescriptor.formatMxnCents(0L))
        assertEquals("$0.50 MXN", PosScreenDescriptor.formatMxnCents(50L))
        assertEquals("$9.99 MXN", PosScreenDescriptor.formatMxnCents(999L))
        assertEquals("$1,160.00 MXN", PosScreenDescriptor.formatMxnCents(116_000L))
        assertEquals("$1,234,567.89 MXN", PosScreenDescriptor.formatMxnCents(123_456_789L))
    }

    @Test
    fun `buildSummary builds zero state when splitPreview is null`() {
        val state = PosUiState(amountCents = 0L, splitPreview = null)
        val summary = PosScreenDescriptor.buildSummary(state)
        assertEquals("$0.00 MXN", summary.formattedGrossMxn)
        assertEquals("$0.00 MXN", summary.formattedNetMxn)
        assertEquals("$0.00 MXN", summary.formattedTaxEscrowMxn)
        assertEquals("$0.00 MXN", summary.formattedRbfSweepMxn)
        assertEquals("$0.00 MXN", summary.formattedFeeMxn)
        assertNull(summary.codiSavingsBadge)
        assertEquals("Pago recibido por $0.00 MXN", summary.voiceConfirmationText)
    }

    @Test
    fun `buildSummary populates split breakdown and CoDi savings badge for card checkout`() {
        val vm = PosTerminalViewModel()
        listOf(1, 1, 6, 0, 0, 0).forEach { vm.onDigit(it) } // 116,000 cents = $1,160.00 MXN

        val summary = PosScreenDescriptor.buildSummary(vm.uiState.value)
        assertEquals("$1,160.00 MXN", summary.formattedGrossMxn)
        assertEquals("$170.00 MXN", summary.formattedTaxEscrowMxn) // 16000 IVA + 1000 ISR = 17000 cents
        assertEquals("$116.00 MXN", summary.formattedRbfSweepMxn)
        assertEquals("$33.64 MXN", summary.formattedFeeMxn)
        assertEquals("$840.36 MXN", summary.formattedNetMxn)
        assertEquals("Ahorra $33.64 MXN con CoDi (0% comisión)", summary.codiSavingsBadge)
        assertEquals("Pago recibido por $1,160.00 MXN", summary.voiceConfirmationText)

        vm.setPaymentMethod(PaymentMethod.CODI_QR)
        val codiSummary = PosScreenDescriptor.buildSummary(vm.uiState.value)
        assertEquals("$0.00 MXN", codiSummary.formattedFeeMxn)
        assertNull(codiSummary.codiSavingsBadge)
    }
}
