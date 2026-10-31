package mx.softpos.app.ui

import kotlinx.coroutines.flow.StateFlow
import mx.softpos.app.ui.pos.PosScreenDescriptor

data class NavTabItem(
    val screen: AppScreen,
    val labelEs: String,
    val iconName: String,
    val isSelected: Boolean
)

data class ScreenRenderModel(
    val activeScreen: AppScreen,
    val titleEs: String,
    val formattedAmountMxn: String,
    val bottomNavTabs: List<NavTabItem>,
    val isCheckoutReady: Boolean
)

object SoftPosComposeDescriptor {
    val PRIMARY_TABS: List<Pair<AppScreen, String>> = listOf(
        AppScreen.POS_TERMINAL to "Cobrar",
        AppScreen.CFDI_INVOICE to "Facturar",
        AppScreen.TAX_VAULT to "Bóveda SAT",
        AppScreen.RBF_CAPITAL to "Capital RBF"
    )

    fun describe(controller: SoftPosAppController): ScreenRenderModel {
        val active = controller.currentScreen.value
        val posState = controller.posViewModel.uiState.value

        val titleEs = when (active) {
            AppScreen.POS_TERMINAL -> "Cobrar"
            AppScreen.NFC_TAP_RADAR -> "Acerca la Tarjeta o Celular"
            AppScreen.CODI_QR_DISPLAY -> "Cobro CoDi SPEI (0%)"
            AppScreen.CFDI_INVOICE -> "Facturar CFDI 4.0"
            AppScreen.TAX_VAULT -> "Bóveda SAT"
            AppScreen.RBF_CAPITAL -> "Capital RBF"
            AppScreen.SECURITY_BLOCKED -> "Dispositivo No Autorizado"
        }

        val bottomNavTabs = PRIMARY_TABS.map { (screen, labelEs) ->
            NavTabItem(
                screen = screen,
                labelEs = labelEs,
                iconName = screen.name.lowercase(),
                isSelected = (screen == active)
            )
        }

        return ScreenRenderModel(
            activeScreen = active,
            titleEs = titleEs,
            formattedAmountMxn = PosScreenDescriptor.formatMxnCents(posState.amountCents),
            bottomNavTabs = bottomNavTabs,
            isCheckoutReady = posState.amountCents > 0L
        )
    }
}
