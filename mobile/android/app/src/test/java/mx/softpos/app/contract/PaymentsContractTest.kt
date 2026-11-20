package mx.softpos.app.contract

import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.isUnknownOutcome
import mx.softpos.app.payments.CardPaymentStatus
import mx.softpos.app.payments.HttpCardPaymentsApi
import mx.softpos.app.payments.HttpSalesApi
import mx.softpos.app.payments.SaleKind
import mx.softpos.app.payments.SaleStatus
import mx.softpos.app.payments.SaleStatusGroup
import mx.softpos.app.ui.merchant.isKybBlocked
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

/** Card payments, refunds and sales against bodies of pkg/api (handlers_card_payments.go, handlers_sales.go). */
class PaymentsContractTest {
    private lateinit var server: MockWebServer

    @BeforeEach fun setUp() { server = MockWebServer().apply { start() } }
    @AfterEach fun tearDown() { server.shutdown() }

    private fun respond(code: Int, fixture: String) = server.enqueue(MockResponse().setResponseCode(code).setBody(Fixtures.read(fixture)))
    private fun cards() = HttpCardPaymentsApi(server.url("/").toString(), OkHttpClient())
    private fun sales() = HttpSalesApi(server.url("/").toString(), OkHttpClient())

    @Test
    fun intentAndPaymentDecode() {
        respond(201, "intent_created_201.json")
        val intent = (cards().createIntent("k1", 1234) as ApiResult.Success).data
        assertEquals(CardPaymentStatus.CREATED, intent.status)
        assertTrue(intent.clientSecret.startsWith("mock_cs_"))
        assertFalse(intent.toString().contains(intent.clientSecret), "client_secret never printed")
        assertEquals("k1", server.takeRequest().getHeader("Idempotency-Key"))

        respond(200, "card_payment_created.json")
        val p = (cards().getPayment(intent.cardPaymentId) as ApiResult.Success).data
        assertFalse(p.final, "CREATED/AUTHORIZED are not final; every other status is")
        assertEquals(1234L, p.amountCents)
        assertNull(p.settlement)
    }

    @Test
    fun cancelAndRefundShapes() {
        respond(202, "card_cancel_requested_202.json")
        val c = (cards().cancel("cp_1") as ApiResult.Success).data
        assertTrue(c.requested && !c.canceled)

        respond(202, "card_refund_202.json")
        val r = (cards().refund("cp_1", "rk", 500) as ApiResult.Success).data
        assertEquals("PENDING", r.status)
        assertEquals(500L, r.amountCents)

        respond(502, "card_refund_rejected_502.json")
        val rejected = cards().refund("cp_1", "rk2", 500) as ApiResult.Error
        assertEquals("ACQUIRER_REJECTED", rejected.errorCode)
        assertFalse(rejected.isUnknownOutcome(), "a definitive rejection releases the key")
    }

    @Test
    fun salesStatusesOfEveryKind() {
        respond(200, "sales_mixed_page.json")
        val page = (sales().listSales("m") as ApiResult.Success).data
        assertEquals(listOf(SaleKind.CODI, SaleKind.SPEI, SaleKind.CARD), page.items.map { it.kind })
        // merchant_sales: spei = RECEIVED (credited abono), codi = CONFIRMED.
        assertEquals(
            listOf(SaleStatusGroup.APPROVED, SaleStatusGroup.APPROVED, SaleStatusGroup.PARTIALLY_REFUNDED),
            page.items.map { SaleStatus.group(it.status) }
        )
        assertEquals("cp_a", page.items.last().cardPaymentId)
        assertEquals("MTc2MzY1NTc5NzIwMDAwMDAwMHxjcF9h", page.nextCursor)

        respond(200, "sales_page.json")
        val last = (sales().listSales("m") as ApiResult.Success).data
        assertNull(last.nextCursor)
        assertEquals(SaleStatusGroup.PENDING, SaleStatus.group(last.items.single().status))
    }

    @Test
    fun kybGateCodes() {
        val notApproved = ApiResult.Error(403, "MERCHANT_NOT_APPROVED", Fixtures.read("intent_not_approved_403.json"))
        assertTrue(notApproved.isKybBlocked())
        // Approved but blocked by an operator: not an onboarding problem (own message, no KYB CTA).
        assertFalse(ApiResult.Error(403, "MERCHANT_BLOCKED", "").isKybBlocked())
        assertFalse(ApiResult.Error(403, "MERCHANT_KYB_PENDING", "").isKybBlocked(), "exact backend codes only")
        assertFalse(ApiResult.Error(403, "ROLE_FORBIDDEN", "").isKybBlocked())
        assertFalse(ApiResult.Error(403, "TENANT_MISMATCH", "").isKybBlocked())
        assertFalse(ApiResult.Error(503, "KYB_UNAVAILABLE", "").isKybBlocked())
    }
}
