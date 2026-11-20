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
import mx.softpos.app.ui.merchant.SaleDetailScreen
import mx.softpos.app.ui.merchant.SalesScreen
import mx.softpos.app.ui.merchant.SalesViewModel
import mx.softpos.app.ui.merchant.SettingsScreen
import mx.softpos.app.ui.merchant.SettingsViewModel

private object Routes {
    const val HOME = "home"
    const val CHARGE = "charge"
    const val SALES = "sales"
    const val SALE_DETAIL = "sales/{saleId}"
    const val SETTINGS = "settings"
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
        if (session == null) {
            Box(Modifier.safeDrawingPadding()) {
                val vm = viewModel { LoginViewModel(container.authApi, container.sessionManager) }
                LoginScreen(vm)
            }
        } else {
            // Keyed on the user so ViewModels never leak across sessions.
            key(session?.userId) { MerchantApp(container) }
        }
    }
}

@Composable
private fun MerchantApp(container: AppContainer) {
    val nav = rememberNavController()
    Scaffold(bottomBar = { BottomBar(nav) }) { padding ->
        NavHost(nav, startDestination = Routes.HOME, modifier = Modifier.padding(padding)) {
            composable(Routes.HOME) {
                val vm = viewModel { HomeViewModel(container.merchantApi, container::currentMerchantId) }
                HomeScreen(vm, onCharge = { nav.navigateTab(Routes.CHARGE) })
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
                ChargeScreen(vm)
            }
            composable(Routes.SALES) { entry ->
                val vm = viewModel(entry) { SalesViewModel(container.salesApi, container::currentMerchantId) }
                SalesScreen(vm, onOpen = { nav.navigate(Routes.saleDetail(it)) })
            }
            composable(Routes.SALE_DETAIL) { entry ->
                // Shares the list's ViewModel so the detail reads the loaded sale.
                val parent = remember(entry) { nav.getBackStackEntry(Routes.SALES) }
                val vm = viewModel(parent) { SalesViewModel(container.salesApi, container::currentMerchantId) }
                SaleDetailScreen(vm.sale(entry.arguments?.getString("saleId").orEmpty()), onBack = { nav.popBackStack() })
            }
            composable(Routes.SETTINGS) {
                val vm = viewModel {
                    SettingsViewModel(container.merchantApi, container::currentMerchantId, container::currentRole, container::logout)
                }
                SettingsScreen(vm)
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
