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
import mx.softpos.app.payments.FakeSalesApi
import mx.softpos.app.payments.SaleCard
import mx.softpos.app.payments.SendReceiptResponse
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

    private val sales = FakeSalesApi()

    private fun vm(role: String = "owner", sale: Sale? = cardSale, saleId: String = sale?.id ?: "s1") =
        SaleDetailViewModel(saleId, sale, sales, { "mer_1" }, backend, { role }, backend, dispatcher) { "rk_${++keys}" }.also { it.load() }

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
        // Stored and sent to the processor, then the state write failed: outcome unknown.
        backend.refundErrors += ApiResult.Error(500, "CARD_REFUND_STORE_FAILED", "")
        backend.refundErrors += ApiResult.Error(502, "ACQUIRER_REJECTED", "")
        val seen = mutableListOf<String>()
        val api = object : mx.softpos.app.payments.CardPaymentsApi by backend {
            override fun refund(id: String, idempotencyKey: String, amountCents: Long?) =
                backend.refund(id, idempotencyKey, amountCents).also { seen += idempotencyKey }
        }
        val vm = SaleDetailViewModel(cardSale.id, cardSale, sales, { "mer_1" }, api, { "owner" }, null, dispatcher) { "rk_${++keys}" }
        vm.load()
        vm.refundAll(); assertEquals(RefundError.NETWORK, vm.state.value.refundError)
        vm.refundAll(); assertEquals(RefundError.ACQUIRER_UNAVAILABLE, vm.state.value.refundError)
        vm.refundAll(); assertEquals(RefundError.UNKNOWN, vm.state.value.refundError)
        vm.refundAll(); assertEquals(RefundError.ACQUIRER_REJECTED, vm.state.value.refundError)
        vm.refundAll()
        assertEquals(listOf("rk_1", "rk_1", "rk_1", "rk_1", "rk_2"), seen)
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

        val spei = vm(sale = Sale("s2", "spei", "RECEIVED", 100))
        assertNull(spei.state.value.payment)
        assertFalse(spei.state.value.canRefund)
    }

    private val receiptUrl = "https://pagos.kerno.mx/r/tok"
    private fun detailed(status: String = "CAPTURED") = cardSale.copy(
        status = status, receiptUrl = receiptUrl, receiptExpiresAt = "2027-02-20T04:28:54Z",
        card = SaleCard(brand = "visa", maskedPan = "**** 4242", authCode = "A3723F")
    )

    @Test
    fun detailIsReadFromTheBackendWithReceiptAndCard() {
        backend.seedCaptured("cp_9", 25_000)
        sales.details["s1"] = detailed()
        val vm = vm(role = "cashier")
        assertEquals(listOf("mer_1" to "s1"), sales.detailCalls)
        val s = vm.state.value
        assertEquals(receiptUrl, s.receiptUrl)
        assertEquals("4242", s.sale?.card?.last4)
        assertTrue(s.canSendReceipt, "cashiers send receipts too")
        assertEquals(25_000L, s.payment?.capturedCents, "the card payment is still read")
    }

    @Test
    fun detailWithoutListEntryLoadsFromTheBackend() {
        backend.seedCaptured("cp_9", 25_000)
        sales.details["s1"] = detailed()
        val vm = vm(sale = null, saleId = "s1")
        assertEquals("s1", vm.state.value.sale?.id)
        assertEquals(25_000L, vm.state.value.payment?.capturedCents)

        val missing = vm(sale = null, saleId = "nope")
        assertNull(missing.state.value.sale)
        assertEquals(LoadError.NOT_AVAILABLE, missing.state.value.error)
    }

    @Test
    fun detailFailureKeepsTheListSaleWithoutReceipt() {
        backend.seedCaptured("cp_9", 25_000)
        val vm = vm() // no detail in the fake -> 404
        assertEquals(cardSale.id, vm.state.value.sale?.id)
        assertNull(vm.state.value.error)
        assertNull(vm.state.value.receiptUrl)
        assertFalse(vm.state.value.canSendReceipt)
    }

    @Test
    fun operatorCannotSendAndUnconfirmedHasNoReceipt() {
        sales.details["s1"] = detailed()
        assertFalse(vm(role = "operator").state.value.canSendReceipt)
        sales.details["s1"] = cardSale.copy(status = "CREATED")
        val created = vm()
        assertNull(created.state.value.receiptUrl)
        assertFalse(created.state.value.canSendReceipt)
    }

    @Test
    fun sendReceiptValidatesAndNormalizesThePhone() {
        sales.details["s1"] = detailed()
        val vm = vm()
        vm.sendReceipt()
        assertEquals(ReceiptError.NO_DESTINATION, vm.state.value.receiptError)
        vm.onReceiptEmail("no-es-correo")
        vm.sendReceipt()
        assertEquals(ReceiptError.INVALID_EMAIL, vm.state.value.receiptError)
        vm.onReceiptEmail("")
        vm.onReceiptPhone("55 1234")
        vm.sendReceipt()
        assertEquals(ReceiptError.INVALID_PHONE, vm.state.value.receiptError)
        assertTrue(sales.sends.isEmpty())

        vm.onReceiptPhone("55 1234 5678")
        vm.onReceiptEmail(" cliente@example.com ")
        vm.sendReceipt()
        val (id, req) = sales.sends.single()
        assertEquals("s1", id)
        assertEquals("+525512345678", req.phone)
        assertEquals("cliente@example.com", req.email)
        val s = vm.state.value
        assertNull(s.receiptError)
        assertEquals(mapOf("email" to true, "sms" to true), s.receiptSent)
        assertEquals("", s.receiptEmail, "inputs cleared after sending")
        assertEquals("", s.receiptPhone)
    }

    @Test
    fun sendReceiptMapsBackendAnswers() {
        sales.details["s1"] = detailed()
        val vm = vm()
        fun sendWith(answer: ApiResult<SendReceiptResponse>): SaleDetailUiState {
            sales.sendAnswers += answer
            vm.onReceiptEmail("a@example.com")
            vm.sendReceipt()
            return vm.state.value
        }
        val partial = sendWith(ApiResult.Error(502, "HTTP_502", Fixtures502))
        assertEquals(ReceiptError.SEND_FAILED, partial.receiptError)
        assertEquals(mapOf("sms" to false), partial.receiptSent)
        assertEquals("a@example.com", partial.receiptEmail, "kept for a retry")

        val invalid = sendWith(ApiResult.Error(422, "RECEIPT_DESTINATION_INVALID", """{"error":"RECEIPT_DESTINATION_INVALID","fields":{"phone":"INVALID_FORMAT"}}"""))
        assertEquals(ReceiptError.INVALID_PHONE, invalid.receiptError)
        assertEquals(ReceiptError.NOT_CONFIRMED, sendWith(ApiResult.Error(409, "SALE_NOT_CONFIRMED", "")).receiptError)
        assertEquals(ReceiptError.NOT_ALLOWED, sendWith(ApiResult.Error(403, "ROLE_FORBIDDEN", "")).receiptError)
        assertEquals(ReceiptError.NOT_CONFIGURED, sendWith(ApiResult.Error(503, "RECEIPTS_NOT_CONFIGURED", "")).receiptError)
        assertEquals(ReceiptError.NETWORK, sendWith(ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, "")).receiptError)
        assertEquals(ReceiptError.UNKNOWN, sendWith(ApiResult.Error(500, "HTTP_500", "")).receiptError)
    }

    private companion object {
        val Fixtures502 = mx.softpos.app.contract.Fixtures.read("receipt_send_port_failed_502.json")
    }
}
