package mx.softpos.app.payments

import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.merchant.HttpMerchantApi
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
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
        server.enqueue(MockResponse().setBody("""{"id":"a/b","virtual_clabe":"646180157000000004"}"""))
        HttpMerchantApi(base(), client).getMerchant("a/b")
        assertEquals("/api/v1/merchants/a%2Fb", server.takeRequest().path)
    }

    @Test
    fun cardIntentSendsIdempotencyKeyAndIntegerCents() {
        server.enqueue(MockResponse().setResponseCode(201).setBody("""{"id":"cpi_1","client_secret":"cs","status":"requires_tap"}"""))
        server.enqueue(MockResponse().setBody("""{"id":"cpi_1","status":"succeeded"}"""))
        val api = HttpCardPaymentsApi(base(), client)

        val created = api.createIntent("idem-1", 12_345) as ApiResult.Success
        assertEquals("cpi_1", created.data.id)
        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/v1/card-payments/intents", req.path)
        assertEquals("idem-1", req.getHeader("Idempotency-Key"))
        assertEquals("""{"amount_cents":12345,"currency":"MXN"}""", req.body.readUtf8())

        val got = api.getPayment("cpi_1") as ApiResult.Success
        assertEquals("succeeded", got.data.status)
        assertEquals("/api/v1/card-payments/cpi_1", server.takeRequest().path)
    }

    @Test
    fun salesListUsesProvisionalPath() {
        server.enqueue(MockResponse().setBody("""{"sales":[{"id":"s1","amount_cents":100,"method":"CARD","status":"succeeded"}]}"""))
        val r = HttpSalesApi(base(), client).listSales("mer_1", 20) as ApiResult.Success
        assertEquals(1, r.data.sales.size)
        assertEquals("/api/v1/merchants/mer_1/sales?limit=20", server.takeRequest().path)
    }

    @Test
    fun inMemoryBackendIsIdempotentPerKey() {
        val backend = InMemoryPaymentsBackend()
        val a = (backend.createIntent("k", 100) as ApiResult.Success).data
        val b = (backend.createIntent("k", 100) as ApiResult.Success).data
        assertEquals(a.id, b.id)
        assertEquals(1, (backend.listSales("mer_1") as ApiResult.Success).data.sales.size)
    }
}
