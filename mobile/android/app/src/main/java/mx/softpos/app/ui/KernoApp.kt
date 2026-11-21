package mx.softpos.app.ui

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.automirrored.filled.List
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.ShoppingCart
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.res.stringResource
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.navigation.NavGraph.Companion.findStartDestination
import androidx.navigation.NavHostController
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.currentBackStackEntryAsState
import androidx.navigation.compose.rememberNavController
import mx.softpos.app.AppContainer
import mx.softpos.app.R
import mx.softpos.app.ui.charge.ChargeScreen
import mx.softpos.app.ui.charge.ChargeViewModel
import mx.softpos.app.ui.login.LoginScreen
import mx.softpos.app.ui.login.LoginViewModel
import mx.softpos.app.ui.merchant.HomeScreen
import mx.softpos.app.ui.merchant.HomeViewModel
import mx.softpos.app.ui.merchant.MerchantPickerScreen
import mx.softpos.app.ui.merchant.MerchantPickerViewModel
import mx.softpos.app.ui.merchant.SaleDetailViewModel
import mx.softpos.app.ui.merchant.SaleDetailScreen
import mx.softpos.app.ui.merchant.SalesScreen
import mx.softpos.app.ui.merchant.SalesViewModel
import mx.softpos.app.ui.merchant.SettingsScreen
import mx.softpos.app.ui.merchant.SettingsViewModel
import mx.softpos.app.ui.fiscal.CsdScreen
import mx.softpos.app.ui.fiscal.CsdViewModel
import mx.softpos.app.ui.onboarding.KybScreen
import mx.softpos.app.ui.onboarding.KybViewModel

private object Routes {
    const val HOME = "home"
    const val CHARGE = "charge"
    const val SALES = "sales"
    const val SALE_DETAIL = "sales/{saleId}"
    const val SETTINGS = "settings"
    const val PICKER = "merchant-picker"
    const val ONBOARDING = "onboarding"
    const val CSD = "csd"
    fun saleDetail(id: String) = "sales/$id"
}

private data class Tab(val route: String, val label: Int, val icon: ImageVector)

private val tabs = listOf(
    Tab(Routes.HOME, R.string.nav_home, Icons.Filled.Home),
    Tab(Routes.CHARGE, R.string.nav_charge, Icons.Filled.ShoppingCart),
    Tab(Routes.SALES, R.string.nav_sales, Icons.AutoMirrored.Filled.List),
    Tab(Routes.SETTINGS, R.string.nav_settings, Icons.Filled.Person)
)

/** Root: the session decides between the login flow and the merchant app. */
@Composable
fun KernoApp(container: AppContainer) {
    val session by container.sessionManager.session.collectAsStateWithLifecycle()
    Surface(modifier = Modifier.fillMaxSize()) {
        val s = session
        when {
            s == null -> Box(Modifier.safeDrawingPadding()) {
                val vm = viewModel { LoginViewModel(container.authApi, container.sessionManager) }
                LoginScreen(vm)
            }
            // Several memberships and no tenant in the token: choose one first.
            s.merchantId.isBlank() && s.memberships.size > 1 -> Box(Modifier.safeDrawingPadding()) {
                key(s.userId) {
                    val vm = viewModel {
                        MerchantPickerViewModel(s.memberships, null, container.sessionManager::switchMerchant)
                    }
                    MerchantPickerScreen(vm, onLogout = container::logout)
                }
            }
            // Keyed on user + merchant so ViewModels never leak across sessions or tenants.
            else -> key(s.userId, s.merchantId) { MerchantApp(container) }
        }
    }
}

@Composable
private fun MerchantApp(container: AppContainer) {
    val nav = rememberNavController()
    Scaffold(bottomBar = { BottomBar(nav) }) { padding ->
        NavHost(nav, startDestination = Routes.HOME, modifier = Modifier.padding(padding)) {
            composable(Routes.HOME) {
                val vm = viewModel { HomeViewModel(container.merchantApi, container::currentMerchantId, kybApi = container.kybApi) }
                HomeScreen(vm, onCharge = { nav.navigateTab(Routes.CHARGE) }, onOnboarding = { nav.navigate(Routes.ONBOARDING) })
            }
            composable(Routes.ONBOARDING) {
                val vm = viewModel { KybViewModel(container.kybApi, container::currentMerchantId, container::currentRole) }
                KybScreen(vm, container.documentReader, onDone = { nav.popBackStack() })
            }
            composable(Routes.CHARGE) {
                val vm = viewModel {
                    ChargeViewModel(
                        cardApi = container.cardPaymentsApi,
                        tapToPay = container.tapToPay,
                        merchantApi = container.merchantApi,
                        merchantId = container::currentMerchantId
                    )
                }
                ChargeScreen(vm, onOnboarding = { nav.navigate(Routes.ONBOARDING) })
            }
            composable(Routes.SALES) { entry ->
                val vm = viewModel(entry) { SalesViewModel(container.salesApi, container::currentMerchantId) }
                SalesScreen(vm, onOpen = { nav.navigate(Routes.saleDetail(it)) })
            }
            composable(Routes.SALE_DETAIL) { entry ->
                // The list's ViewModel holds the loaded sale; the detail reads the card payment.
                val parent = remember(entry) { nav.getBackStackEntry(Routes.SALES) }
                val list = viewModel(parent) { SalesViewModel(container.salesApi, container::currentMerchantId) }
                val sale = list.sale(entry.arguments?.getString("saleId").orEmpty())
                val vm = viewModel(entry) {
                    SaleDetailViewModel(sale, container.cardPaymentsApi, container::currentRole, container.devCardSimulator)
                }
                SaleDetailScreen(vm, onBack = { nav.popBackStack() })
            }
            composable(Routes.SETTINGS) {
                val vm = viewModel {
                    SettingsViewModel(
                        container.merchantApi, container::currentMerchantId, container::currentRole, container::logout,
                        membershipCount = { container.memberships().size }
                    )
                }
                SettingsScreen(vm, onSwitchMerchant = { nav.navigate(Routes.PICKER) }, onCsd = { nav.navigate(Routes.CSD) })
            }
            composable(Routes.CSD) {
                val vm = viewModel {
                    CsdViewModel(container.csdApi, container.merchantApi, container::currentMerchantId, container::currentRole)
                }
                CsdScreen(vm, container.csdFileReader, onBack = { nav.popBackStack() })
            }
            composable(Routes.PICKER) {
                val vm = viewModel {
                    MerchantPickerViewModel(
                        container.memberships(), container.currentMerchantId(), container.sessionManager::switchMerchant,
                        onSwitched = { nav.popBackStack() }
                    )
                }
                MerchantPickerScreen(vm, onBack = { nav.popBackStack() })
            }
        }
    }
}

@Composable
private fun BottomBar(nav: NavHostController) {
    val entry by nav.currentBackStackEntryAsState()
    val current = entry?.destination?.route
    NavigationBar {
        tabs.forEach { tab ->
            val selected = current == tab.route || (tab.route == Routes.SALES && current == Routes.SALE_DETAIL)
            NavigationBarItem(
                selected = selected,
                onClick = { nav.navigateTab(tab.route) },
                icon = { Icon(tab.icon, contentDescription = null) },
                label = { Text(stringResource(tab.label)) }
            )
        }
    }
}

private fun NavHostController.navigateTab(route: String) = navigate(route) {
    popUpTo(graph.findStartDestination().id) { saveState = true }
    launchSingleTop = true
    restoreState = true
}
