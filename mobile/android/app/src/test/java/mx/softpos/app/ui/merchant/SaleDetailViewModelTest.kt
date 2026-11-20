package mx.softpos.app.ui.merchant

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_CODE
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.payments.CardPaymentStatus
import mx.softpos.app.payments.FakeCardBackend
import mx.softpos.app.payments.Sale
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

@OptIn(ExperimentalCoroutinesApi::class)
class SaleDetailViewModelTest {
    private val dispatcher = UnconfinedTestDispatcher()
    private val backend = FakeCardBackend()
    private var keys = 0

    @BeforeEach fun setUp() = Dispatchers.setMain(dispatcher)
    @AfterEach fun tearDown() = Dispatchers.resetMain()

    private val cardSale = Sale("s1", "card", "CAPTURED", 25_000, cardPaymentId = "cp_9")

    private fun vm(role: String = "owner", sale: Sale? = cardSale) =
        SaleDetailViewModel(sale, backend, { role }, backend, dispatcher) { "rk_${++keys}" }.also { it.load() }

    @Test
    fun ownerRefundsInFullAndSeesRefundedState() {
        backend.seedCaptured("cp_9", 25_000)
        val vm = vm()
        assertTrue(vm.state.value.canRefund)
        assertEquals(25_000L, vm.state.value.refundableCents)

        vm.refundAll()
        val s = vm.state.value
        assertEquals(25_000L, s.lastRefund?.amountCents)
        assertEquals(CardPaymentStatus.REFUNDED, s.payment?.status)
        assertEquals(25_000L, s.payment?.refundedCents)
        assertFalse(s.canRefund)
        assertTrue("refund:cp_9" in backend.simulated)
    }

    @Test
    fun partialRefundParsesCentsAndValidates() {
        backend.seedCaptured("cp_9", 25_000)
        val vm = vm()
        vm.onRefundInput("abc")
        assertEquals("", vm.state.value.refundInput)
        vm.onRefundInput("300.01")
        vm.refundPartial()
        assertEquals(RefundError.EXCEEDS_REMAINING, vm.state.value.refundError)
        vm.onRefundInput("50.5")
        vm.refundPartial()
        assertEquals(5_050L, vm.state.value.lastRefund?.amountCents)
        assertEquals(CardPaymentStatus.PARTIALLY_REFUNDED, vm.state.value.payment?.status)
        assertEquals(19_950L, vm.state.value.refundableCents)
        vm.onRefundInput("0")
        vm.refundPartial()
        assertEquals(RefundError.INVALID_AMOUNT, vm.state.value.refundError)
    }

    @Test
    fun unknownOutcomeRetriesWithSameKeyOtherErrorsDoNot() {
        backend.seedCaptured("cp_9", 10_000)
        backend.refundErrors += ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, "")
        backend.refundErrors += ApiResult.Error(502, "ACQUIRER_UNAVAILABLE", "")
        backend.refundErrors += ApiResult.Error(502, "ACQUIRER_REJECTED", "")
        val seen = mutableListOf<String>()
        val api = object : mx.softpos.app.payments.CardPaymentsApi by backend {
            override fun refund(id: String, idempotencyKey: String, amountCents: Long?) =
                backend.refund(id, idempotencyKey, amountCents).also { seen += idempotencyKey }
        }
        val vm = SaleDetailViewModel(cardSale, api, { "owner" }, null, dispatcher) { "rk_${++keys}" }
        vm.load()
        vm.refundAll(); assertEquals(RefundError.NETWORK, vm.state.value.refundError)
        vm.refundAll(); assertEquals(RefundError.ACQUIRER_UNAVAILABLE, vm.state.value.refundError)
        vm.refundAll(); assertEquals(RefundError.ACQUIRER_REJECTED, vm.state.value.refundError)
        vm.refundAll()
        assertEquals(listOf("rk_1", "rk_1", "rk_1", "rk_2"), seen)
        // No simulator (release-like): the request stays pending until the processor confirms.
        assertEquals(10_000L, vm.state.value.lastRefund?.amountCents)
        assertEquals(0L, vm.state.value.payment?.refundedCents)
    }

    @Test
    fun cashierNeverSeesRefund() {
        backend.seedCaptured("cp_9", 25_000)
        val vm = vm(role = "cashier")
        assertFalse(vm.state.value.canRefund)
        vm.refundAll()
        assertNull(vm.state.value.lastRefund)
    }

    @Test
    fun chargebackAndNonCardSales() {
        backend.seedCaptured("cp_9", 25_000, chargeback = 25_000, status = CardPaymentStatus.CHARGEBACK)
        val vm = vm()
        assertEquals(CardPaymentStatus.CHARGEBACK, vm.state.value.payment?.status)
        assertFalse(vm.state.value.canRefund)

        val spei = vm(sale = Sale("s2", "spei", "SETTLED", 100))
        assertNull(spei.state.value.payment)
        assertFalse(spei.state.value.canRefund)
    }
}
