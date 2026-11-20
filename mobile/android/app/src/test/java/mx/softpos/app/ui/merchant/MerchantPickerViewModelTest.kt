package mx.softpos.app.ui.merchant

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import mx.softpos.app.auth.FakeAuthApi
import mx.softpos.app.auth.InMemoryTokenStore
import mx.softpos.app.auth.MembershipInfo
import mx.softpos.app.auth.SessionManager
import mx.softpos.app.core.network.ApiResult
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

@OptIn(ExperimentalCoroutinesApi::class)
class MerchantPickerViewModelTest {
    private val dispatcher = UnconfinedTestDispatcher()
    private val members = listOf(MembershipInfo("mer_1", "owner"), MembershipInfo("mer_2", "cashier"))

    @BeforeEach fun setUp() = Dispatchers.setMain(dispatcher)
    @AfterEach fun tearDown() = Dispatchers.resetMain()

    @Test
    fun selectingRescopesSessionThroughRefresh() {
        val api = FakeAuthApi()
        val sessions = SessionManager(InMemoryTokenStore(), api)
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1", merchant = "").copy(memberships = members))
        api.refreshResult = { ApiResult.Success(FakeAuthApi.pair("at_2", "rt_2", merchant = "mer_2").copy(role = "cashier", memberships = members), 200) }
        var switched = 0
        val vm = MerchantPickerViewModel(members, null, sessions::switchMerchant, { switched++ }, dispatcher)

        vm.select("mer_2")
        assertEquals(listOf<String?>("mer_2"), api.refreshMerchantIds)
        assertEquals("mer_2", sessions.current()?.merchantId)
        assertEquals("cashier", sessions.current()?.role)
        assertEquals(2, sessions.current()?.memberships?.size)
        assertEquals(1, switched)

        vm.select("mer_2") // already current: no second refresh
        assertEquals(1, api.refreshCalls.get())
        assertEquals(2, switched)
    }

    @Test
    fun failedSwitchKeepsSessionUnless401() {
        val api = FakeAuthApi()
        val sessions = SessionManager(InMemoryTokenStore(), api)
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1", merchant = "mer_1").copy(memberships = members))
        val vm = MerchantPickerViewModel(members, "mer_1", sessions::switchMerchant, {}, dispatcher)

        api.refreshResult = { ApiResult.Error(403, "NOT_A_MERCHANT_MEMBER", "") }
        vm.select("mer_2")
        assertEquals(SwitchError.NOT_A_MEMBER, vm.state.value.error)
        assertEquals("mer_1", sessions.current()?.merchantId)

        api.refreshResult = { ApiResult.Error(503, "AUTH_INTERNAL_ERROR", "") }
        vm.select("mer_2")
        assertNotNull(sessions.current())

        api.refreshResult = { ApiResult.Error(401, "REFRESH_TOKEN_REUSED", "") }
        vm.select("mer_2")
        assertEquals(null, sessions.current())
    }
}
