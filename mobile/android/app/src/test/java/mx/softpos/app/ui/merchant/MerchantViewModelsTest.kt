package mx.softpos.app.ui.merchant

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_CODE
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.payments.Sale
import mx.softpos.app.payments.SalesApi
import mx.softpos.app.payments.SalesPage
import mx.softpos.app.ui.charge.FakeMerchantApi
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

@OptIn(ExperimentalCoroutinesApi::class)
class MerchantViewModelsTest {
    private val dispatcher = UnconfinedTestDispatcher()

    @BeforeEach fun setUp() = Dispatchers.setMain(dispatcher)
    @AfterEach fun tearDown() = Dispatchers.resetMain()

    @Test
    fun homeShowsAvailableAndTaxSetAsideSeparately() {
        val vm = HomeViewModel(FakeMerchantApi(), { "mer_1" }, dispatcher)
        vm.load()
        val b = vm.state.value.balances!!
        assertEquals(150_000L, b.availableCents)
        assertEquals(23_456L, b.taxEscrowCents)
        assertNull(vm.state.value.error)
    }

    @Test
    fun homeWithoutMerchantOrNetwork() {
        val none = HomeViewModel(FakeMerchantApi(), { null }, dispatcher)
        none.load()
        assertEquals(LoadError.NO_MERCHANT, none.state.value.error)

        val api = FakeMerchantApi(balances = ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, ""))
        val offline = HomeViewModel(api, { "mer_1" }, dispatcher)
        offline.load()
        assertEquals(LoadError.NETWORK, offline.state.value.error)
        assertEquals(false, offline.state.value.isLoading)
    }

    @Test
    fun salesListAndDetailLookup() {
        val sales = listOf(Sale("s2", 500, "SPEI", "settled"), Sale("s1", 100, "CARD", "succeeded"))
        val api = object : SalesApi {
            override fun listSales(merchantId: String, limit: Int) = ApiResult.Success(SalesPage(sales), 200)
        }
        val vm = SalesViewModel(api, { "mer_1" }, dispatcher)
        vm.load()
        assertEquals(sales, vm.state.value.sales)
        assertEquals(500L, vm.sale("s2")?.amountCents)
        assertNull(vm.sale("missing"))
    }

    @Test
    fun salesEndpointMissingIsReportedAsNotAvailable() {
        val api = object : SalesApi {
            override fun listSales(merchantId: String, limit: Int) = ApiResult.Error(404, "HTTP_404", "")
        }
        val vm = SalesViewModel(api, { "mer_1" }, dispatcher)
        vm.load()
        assertEquals(LoadError.NOT_AVAILABLE, vm.state.value.error)
    }

    @Test
    fun settingsShowsProfileAndLogsOut() {
        var loggedOut = false
        val vm = SettingsViewModel(FakeMerchantApi(), { "mer_1" }, { "owner" }, { loggedOut = true }, dispatcher)
        vm.load()
        assertEquals("646180157000000004", vm.state.value.profile?.virtualClabe)
        assertEquals("owner", vm.state.value.role)
        vm.logout()
        assertEquals(true, loggedOut)
    }
}
