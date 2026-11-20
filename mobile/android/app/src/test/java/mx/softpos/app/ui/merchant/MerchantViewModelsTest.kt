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

    private class PagedSales(val pages: Map<String?, SalesPage>) : SalesApi {
        val cursors = mutableListOf<String?>()
        override fun listSales(merchantId: String, limit: Int, before: String?): ApiResult<SalesPage> {
            cursors += before
            return pages[before]?.let { ApiResult.Success(it, 200) } ?: ApiResult.Error(404, "HTTP_404", "")
        }
    }

    @Test
    fun salesListPaginatesWithCursorAndDedupes() {
        val s1 = Sale("s1", "card", "CAPTURED", 100, cardPaymentId = "cp_1")
        val s2 = Sale("s2", "spei", "SETTLED", 500)
        val s3 = Sale("s3", "codi", "CONFIRMED", 700)
        val api = PagedSales(mapOf(null to SalesPage(listOf(s1, s2), "c1"), "c1" to SalesPage(listOf(s2, s3), null)))
        val vm = SalesViewModel(api, { "mer_1" }, dispatcher)
        vm.load()
        assertEquals(listOf(s1, s2), vm.state.value.sales)
        assertEquals(true, vm.state.value.canLoadMore)

        vm.loadMore()
        assertEquals(listOf(s1, s2, s3), vm.state.value.sales)
        assertNull(vm.state.value.nextCursor)
        vm.loadMore() // last page: no request
        assertEquals(listOf(null, "c1"), api.cursors)
        assertEquals(500L, vm.sale("s2")?.amountCents)
        assertNull(vm.sale("missing"))

        vm.load() // refresh starts over from the first page
        assertEquals(listOf(s1, s2), vm.state.value.sales)
    }

    @Test
    fun salesEndpointMissingIsReportedAsNotAvailable() {
        val vm = SalesViewModel(PagedSales(emptyMap()), { "mer_1" }, dispatcher)
        vm.load()
        assertEquals(LoadError.NOT_AVAILABLE, vm.state.value.error)
    }

    @Test
    fun homeRoutesToOnboardingOnceWhileKybIsNotApproved() {
        val kyb = mx.softpos.app.ui.onboarding.FakeKybApi(ApiResult.Success(mx.softpos.app.kyb.KybStatus("IN_REVIEW"), 200))
        val vm = HomeViewModel(FakeMerchantApi(), { "mer_1" }, dispatcher, kyb)
        vm.load()
        assertEquals(true, vm.state.value.needsOnboarding)
        assertEquals(true, vm.consumeAutoRoute())
        assertEquals(false, vm.consumeAutoRoute())

        kyb.status = ApiResult.Success(mx.softpos.app.kyb.KybStatus("APPROVED"), 200)
        val approved = HomeViewModel(FakeMerchantApi(), { "mer_1" }, dispatcher, kyb)
        approved.load()
        assertEquals(false, approved.state.value.needsOnboarding)
        assertEquals(false, approved.consumeAutoRoute())

        kyb.status = ApiResult.Error(404, "HTTP_404", "") // backend without KYB endpoint: no routing
        val old = HomeViewModel(FakeMerchantApi(), { "mer_1" }, dispatcher, kyb)
        old.load()
        assertEquals(false, old.state.value.needsOnboarding)
        assertEquals(150_000L, old.state.value.balances?.availableCents)
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
