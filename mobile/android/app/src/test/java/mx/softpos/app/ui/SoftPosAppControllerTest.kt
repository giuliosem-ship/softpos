package mx.softpos.app.ui

import mx.softpos.app.core.security.DeviceSecuritySignals
import mx.softpos.app.ui.pos.PaymentMethod
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class SoftPosAppControllerTest {

    private val cleanSignals = DeviceSecuritySignals(
        isRooted = false,
        isDebuggerAttached = false,
        hasMemoryHooks = false,
        isHardwareKeystoreBacked = true,
        meetsStrongIntegrity = true,
        attestationToken = "play_integrity_valid_jwt"
    )

    private val compromisedSignals = cleanSignals.copy(isRooted = true)

    @Test
    fun `startCheckout requires positive amount and routes CARD_NFC based on DeviceAttestationGuard`() {
        val controller = SoftPosAppController()
        assertEquals(AppScreen.POS_TERMINAL, controller.currentScreen.value)
        assertNull(controller.lastResolvedReceptor.value)

        // Cannot start checkout with 0 amount
        assertThrows(IllegalStateException::class.java) {
            controller.startCheckout(cleanSignals)
        }

        // Enter $150.00 MXN (15000 cents)
        listOf(1, 5, 0, 0, 0).forEach { controller.posViewModel.onDigit(it) }

        // Compromised device blocks NFC Tap
        val blockedScreen = controller.startCheckout(compromisedSignals)
        assertEquals(AppScreen.SECURITY_BLOCKED, blockedScreen)
        assertEquals(AppScreen.SECURITY_BLOCKED, controller.currentScreen.value)

        // Clean device transitions to NFC_TAP_RADAR
        val radarScreen = controller.startCheckout(cleanSignals)
        assertEquals(AppScreen.NFC_TAP_RADAR, radarScreen)
        assertEquals(AppScreen.NFC_TAP_RADAR, controller.currentScreen.value)
    }

    @Test
    fun `startCheckout routes CODI_QR to CODI_QR_DISPLAY and onPaymentSucceeded moves to CFDI_INVOICE`() {
        val controller = SoftPosAppController()
        listOf(2, 5, 0, 0, 0).forEach { controller.posViewModel.onDigit(it) }
        controller.posViewModel.setPaymentMethod(PaymentMethod.CODI_QR)

        val codiScreen = controller.startCheckout(compromisedSignals)
        assertEquals(AppScreen.CODI_QR_DISPLAY, codiScreen)
        assertEquals(AppScreen.CODI_QR_DISPLAY, controller.currentScreen.value)

        controller.onPaymentSucceeded()
        assertEquals(AppScreen.CFDI_INVOICE, controller.currentScreen.value)

        controller.navigateTo(AppScreen.TAX_VAULT)
        assertEquals(AppScreen.TAX_VAULT, controller.currentScreen.value)
    }

    @Test
    fun `resolveInvoiceReceptorFromQr falls back to publicoEnGeneral when blank or parses SAT QR URL`() {
        val controller = SoftPosAppController()

        val generic = controller.resolveInvoiceReceptorFromQr(null)
        assertEquals("XAXX010101000", generic.rfc)
        assertTrue(generic.isGeneric)
        assertEquals(generic, controller.lastResolvedReceptor.value)

        val parsed = controller.resolveInvoiceReceptorFromQr(
            "https://siat.sat.gob.mx/app/qr/faces/pages/mobile/validadorqr.jsf?D1=10&D2=1&D3=14070123456_GODE561231GR8"
        )
        assertEquals("14070123456", parsed.idCif)
        assertEquals("GODE561231GR8", parsed.rfc)
        assertFalse(parsed.isGeneric)
        assertEquals(parsed, controller.lastResolvedReceptor.value)
    }
}
