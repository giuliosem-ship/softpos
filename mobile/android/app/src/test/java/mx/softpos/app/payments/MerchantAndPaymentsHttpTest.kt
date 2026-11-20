package mx.softpos.app.payments

import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.merchant.HttpMerchantApi
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

class MerchantAndPaymentsHttpTest {
    private lateinit var server: MockWebServer
    private val client = OkHttpClient()

    @BeforeEach fun setUp() { server = MockWebServer().apply { start() } }
    @AfterEach fun tearDown() { server.shutdown() }

    private fun base() = server.url("/").toString()

    @Test
    fun balancesMatchBackendShape() {
        server.enqueue(
            MockResponse().setBody(
                """{"merchant_id":"mer_1","available_cents":9223372036854775807,"tax_escrow_cents":16,"rbf_debt_cents":0,"currency":"MXN"}"""
            )
        )
        val r = HttpMerchantApi(base(), client).getBalances("mer_1") as ApiResult.Success
        assertEquals(Long.MAX_VALUE, r.data.availableCents)
        assertEquals(16L, r.data.taxEscrowCents)
        assertEquals("/api/v1/merchants/mer_1/balances", server.takeRequest().path)
    }

    @Test
    fun merchantIdIsPathEscaped() {
        server.enqueue(MockResponse().setBody("""{"id":"a/b","clabe":"646180157000000004"}"""))
        HttpMerchantApi(base(), client).getMerchant("a/b")
        assertEquals("/api/v1/merchants/a%2Fb", server.takeRequest().path)
    }

    @Test
    fun merchantProfileReadsBackendClabeField() {
        // Exact shape of pkg/api/server.go Merchant (json tag "clabe").
        server.enqueue(
            MockResponse().setBody(
                """{"id":"mer_1","rfc":"XAXX010101000","name":"Tacos","is_resico":true,"clabe":"646180157000000004"}"""
            )
        )
        val r = HttpMerchantApi(base(), client).getMerchant("mer_1") as ApiResult.Success
        assertEquals("646180157000000004", r.data.virtualClabe)
    }

    @Test
    fun cardIntentMatchesBackendContract() {
        server.enqueue(
            MockResponse().setResponseCode(201).setBody(
                """{"card_payment_id":"cp_1","status":"CREATED","amount_cents":12345,"currency":"MXN","provider":"mock",
                   "provider_payment_id":"mock_pi_1","client_secret":"mock_cs_1","expires_at":"2026-11-20T18:15:00Z",
                   "created_at":"2026-11-20T18:00:00Z"}"""
            )
        )
        val api = HttpCardPaymentsApi(base(), client)
        val created = (api.createIntent("idem-1", 12_345) as ApiResult.Success).data
        assertEquals("cp_1", created.cardPaymentId)
        assertEquals("mock_pi_1", created.providerPaymentId)
        assertEquals("mock_cs_1", created.clientSecret)
        assertFalse(created.toString().contains("mock_cs_1"), "client_secret never in toString")
        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/v1/card-payments/intents", req.path)
        assertEquals("idem-1", req.getHeader("Idempotency-Key"))
        // No is_persona_moral: the backend derives it.
        assertEquals("""{"amount_cents":12345,"currency":"MXN"}""", req.body.readUtf8())
    }

    @Test
    fun cardPaymentStatusAndSettlement() {
        server.enqueue(
            MockResponse().setBody(
                """{"card_payment_id":"cp_1","merchant_id":"mer_1","provider":"mock","provider_payment_id":"p",
                   "status":"PARTIALLY_REFUNDED","amount_cents":25000,"currency":"MXN","captured_cents":25000,
                   "refunded_cents":5000,"chargeback_cents":0,"final":true,
                   "settlement":{"payment_id":"cp_1","gross_amount_cents":25000,"platform_fee_cents":625,
                   "tax_escrow_cents":1000,"rbf_sweep_cents":0,"net_available_cents":23375},
                   "created_at":"x","updated_at":"y"}"""
            )
        )
        val p = (HttpCardPaymentsApi(base(), client).getPayment("cp_1") as ApiResult.Success).data
        assertTrue(p.final)
        assertEquals(20_000L, p.refundableCents)
        assertEquals(23_375L, p.settlement?.netAvailableCents)
        assertEquals("/api/v1/card-payments/cp_1", server.takeRequest().path)
    }

    @Test
    fun acquirerUnavailableKeepsErrorCode() {
        server.enqueue(MockResponse().setResponseCode(502).setBody("""{"error":"ACQUIRER_UNAVAILABLE"}"""))
        val r = HttpCardPaymentsApi(base(), client).createIntent("k", 100) as ApiResult.Error
        assertEquals(502, r.statusCode)
        assertEquals("ACQUIRER_UNAVAILABLE", r.errorCode)
    }

    @Test
    fun cancelDistinguishesImmediateAndRequested() {
        server.enqueue(MockResponse().setResponseCode(200).setBody("""{"card_payment_id":"cp_1","status":"CANCELED"}"""))
        server.enqueue(MockResponse().setResponseCode(202).setBody("""{"card_payment":{},"cancel_requested":true}"""))
        val api = HttpCardPaymentsApi(base(), client)
        assertEquals(CancelResult(canceled = true, requested = false), (api.cancel("cp_1") as ApiResult.Success).data)
        assertEquals(CancelResult(canceled = false, requested = true), (api.cancel("cp_1") as ApiResult.Success).data)
        assertEquals("/api/v1/card-payments/cp_1/cancel", server.takeRequest().path)
    }

    @Test
    fun devSimulatorPostsOutcome() {
        server.enqueue(MockResponse().setBody("""{"events":[]}"""))
        HttpDevCardSimulator(base(), client).simulate("cp_1", DevCardSimulator.REFUND, "cr_1")
        val req = server.takeRequest()
        assertEquals("/api/v1/dev/card-payments/cp_1/simulate", req.path)
        assertEquals("""{"outcome":"refund","refund_id":"cr_1"}""", req.body.readUtf8())
    }

    @Test
    fun salesListUsesCursorPagination() {
        server.enqueue(
            MockResponse().setBody(
                """{"items":[{"id":"s1","kind":"card","status":"CAPTURED","amount_cents":100,"created_at":"t","card_payment_id":"cp_1"},
                   {"id":"s2","kind":"spei","status":"SETTLED","amount_cents":200,"created_at":"t"}],"next_cursor":"c2"}"""
            )
        )
        server.enqueue(MockResponse().setBody("""{"items":[],"next_cursor":null}"""))
        val api = HttpSalesApi(base(), client)
        val first = (api.listSales("mer_1") as ApiResult.Success).data
        assertEquals("cp_1", first.items[0].cardPaymentId)
        assertNull(first.items[1].cardPaymentId)
        assertEquals("c2", first.nextCursor)
        assertEquals("/api/v1/merchants/mer_1/sales?limit=50", server.takeRequest().path)

        val last = (api.listSales("mer_1", before = "c2") as ApiResult.Success).data
        assertNull(last.nextCursor)
        assertEquals("/api/v1/merchants/mer_1/sales?limit=50&before=c2", server.takeRequest().path)
    }

    @Test
    fun saleStatusGroups() {
        assertEquals(SaleStatusGroup.APPROVED, SaleStatus.group("CAPTURED"))
        assertEquals(SaleStatusGroup.FAILED, SaleStatus.group("DECLINED"))
        assertEquals(SaleStatusGroup.PARTIALLY_REFUNDED, SaleStatus.group("PARTIALLY_REFUNDED"))
        assertEquals(SaleStatusGroup.CHARGEBACK, SaleStatus.group("CHARGEBACK"))
        assertEquals(SaleStatusGroup.PENDING, SaleStatus.group("CREATED"))
    }
}
