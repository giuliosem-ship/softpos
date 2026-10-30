package mx.softpos.app.ui

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import mx.softpos.app.core.security.AttestationVerdict
import mx.softpos.app.core.security.DeviceAttestationGuard
import mx.softpos.app.core.security.DeviceSecuritySignals
import mx.softpos.app.ui.cfdi.SatCifData
import mx.softpos.app.ui.cfdi.SatConstanciaUrlParser
import mx.softpos.app.ui.pos.PaymentMethod
import mx.softpos.app.ui.pos.PosTerminalViewModel

enum class AppScreen {
    POS_TERMINAL,
    NFC_TAP_RADAR,
    CODI_QR_DISPLAY,
    CFDI_INVOICE,
    TAX_VAULT,
    RBF_CAPITAL,
    SECURITY_BLOCKED
}

class SoftPosAppController(
    val posViewModel: PosTerminalViewModel = PosTerminalViewModel()
) {
    private val _currentScreen = MutableStateFlow(AppScreen.POS_TERMINAL)
    val currentScreen: StateFlow<AppScreen> = _currentScreen.asStateFlow()

    private val _lastResolvedReceptor = MutableStateFlow<SatCifData?>(null)
    val lastResolvedReceptor: StateFlow<SatCifData?> = _lastResolvedReceptor.asStateFlow()

    fun navigateTo(screen: AppScreen) {
        _currentScreen.value = screen
    }

    fun startCheckout(securitySignals: DeviceSecuritySignals): AppScreen {
        check(posViewModel.uiState.value.amountCents > 0L) { "Checkout amount must be greater than zero" }
        val target = when (posViewModel.uiState.value.paymentMethod) {
            PaymentMethod.CARD_NFC -> when (DeviceAttestationGuard.verifyForNfcTap(securitySignals)) {
                is AttestationVerdict.Blocked -> AppScreen.SECURITY_BLOCKED
                is AttestationVerdict.Allowed -> AppScreen.NFC_TAP_RADAR
            }
            PaymentMethod.CODI_QR -> AppScreen.CODI_QR_DISPLAY
        }
        _currentScreen.value = target
        return target
    }

    fun onPaymentSucceeded() {
        _currentScreen.value = AppScreen.CFDI_INVOICE
    }

    fun resolveInvoiceReceptorFromQr(qrUrl: String?): SatCifData {
        val receptor = if (qrUrl.isNullOrBlank()) {
            SatConstanciaUrlParser.publicoEnGeneral()
        } else {
            SatConstanciaUrlParser.parseQrUrl(qrUrl)
        }
        _lastResolvedReceptor.value = receptor
        return receptor
    }
}
