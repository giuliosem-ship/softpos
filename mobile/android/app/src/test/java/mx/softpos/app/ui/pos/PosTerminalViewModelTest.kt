package mx.softpos.app.ui.pos

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class PosTerminalViewModelTest {

    @Test
    fun `keypad digits accumulate cents and compute real-time SplitPreview`() {
        val viewModel = PosTerminalViewModel(
            initialIsrRateBps = 100L,
            initialRbfHoldbackBps = 1000L,
            cardFeeBps = 290L
        )

        // Leading zero ignored when empty
        viewModel.onDigit(0)
        assertEquals(0L, viewModel.uiState.value.amountCents)
        assertNull(viewModel.uiState.value.splitPreview)

        // Enter 1, 1, 6, 0, 0, 0 -> 116,000 cents ($1,160.00 MXN)
        listOf(1, 1, 6, 0, 0, 0).forEach { viewModel.onDigit(it) }

        val state = viewModel.uiState.value
        assertEquals("116000", state.rawDigits)
        assertEquals(116_000L, state.amountCents)
        val preview = state.splitPreview
        assertNotNull(preview)
        assertEquals(100_000L, preview!!.baseCents)
        assertEquals(16_000L, preview.ivaCents)
        assertEquals(1_000L, preview.isrCents)
        assertEquals(3_364L, preview.platformFeeCents)
    }

    @Test
    fun `switching to CODI_QR sets platformFeeCents to zero and toggling Persona Moral applies 125 bps ISR`() {
        val viewModel = PosTerminalViewModel()
        listOf(1, 1, 6, 0, 0, 0).forEach { viewModel.onDigit(it) }

        viewModel.setPaymentMethod(PaymentMethod.CODI_QR)
        val codiPreview = viewModel.uiState.value.splitPreview!!
        assertEquals(0L, codiPreview.platformFeeCents)

        viewModel.togglePersonaMoral()
        assertTrue(viewModel.uiState.value.isPersonaMoral)
        val pmPreview = viewModel.uiState.value.splitPreview!!
        assertEquals(1_250L, pmPreview.isrCents)
        assertTrue(pmPreview.isPersonaMoralWithholding)
    }

    @Test
    fun `onBackspace and onClear update digits and clear preview at zero`() {
        val viewModel = PosTerminalViewModel()
        viewModel.onDigit(5)
        viewModel.onDigit(0)
        assertEquals(50L, viewModel.uiState.value.amountCents)

        viewModel.onBackspace()
        assertEquals(5L, viewModel.uiState.value.amountCents)

        viewModel.onBackspace()
        assertEquals(0L, viewModel.uiState.value.amountCents)
        assertNull(viewModel.uiState.value.splitPreview)

        viewModel.onDigit(9)
        viewModel.onDigit(9)
        viewModel.onClear()
        assertEquals("", viewModel.uiState.value.rawDigits)
        assertEquals(0L, viewModel.uiState.value.amountCents)
        assertNull(viewModel.uiState.value.splitPreview)
        assertFalse(viewModel.uiState.value.isPersonaMoral)
    }
}
