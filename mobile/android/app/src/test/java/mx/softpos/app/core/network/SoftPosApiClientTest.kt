package mx.softpos.app.core.network

import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class SoftPosApiClientTest {
    private lateinit var server: MockWebServer
    private lateinit var client: SoftPosApiClient

    @BeforeEach
    fun setUp() {
        server = MockWebServer()
        server.start()
        val baseUrl = server.url("/").toString().removeSuffix("/")
        client = SoftPosApiClient(
            baseUrl = baseUrl,
            integrityTokenProvider = { "play-integrity-jwt-token-123" }
        )
    }

    @AfterEach
    fun tearDown() {
        server.shutdown()
    }

    @Test
    fun `authorizeEmv sends POST with Idempotency-Key and X-Play-Integrity-Token and returns Success`() {
        server.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("""{"status":"AUTHORIZED","payment_id":"pay_001"}""")
        )

        val result = client.authorizeEmv(
            idempotencyKey = "idem-emv-001",
            merchantId = "merch_123",
            amountCents = 116_000L,
            tlvHex = "9F02060000001160005F2A020484"
        )

        assertTrue(result is ApiResult.Success)
        val success = result as ApiResult.Success<String>
        assertEquals(200, success.statusCode)
        assertTrue(success.data.contains("AUTHORIZED"))

        val recorded = server.takeRequest()
        assertEquals("POST", recorded.method)
        assertEquals("/api/v1/payments/emv/authorize", recorded.path)
        assertEquals("idem-emv-001", recorded.getHeader("Idempotency-Key"))
        assertEquals("play-integrity-jwt-token-123", recorded.getHeader("X-Play-Integrity-Token"))
        val bodyStr = recorded.body.readUtf8()
        assertTrue(bodyStr.contains("merch_123"))
        assertTrue(bodyStr.contains("116000"))
        assertTrue(bodyStr.contains("9F02060000001160005F2A020484"))
    }

    @Test
    fun `generateCodiQr and stampCfdi map 409 to IDEMPOTENCY_CONFLICT and 422 to SAT_COMPLIANCE_REJECTED`() {
        server.enqueue(
            MockResponse()
                .setResponseCode(409)
                .setBody("""{"error":"duplicate transaction"}""")
        )
        server.enqueue(
            MockResponse()
                .setResponseCode(422)
                .setBody("""{"error":"RFC in 69-B definitivos"}""")
        )

        val codiResult = client.generateCodiQr(
            idempotencyKey = "idem-codi-001",
            merchantId = "merch_123",
            amountCents = 50_000L,
            concept = "CAFE"
        )
        assertTrue(codiResult is ApiResult.Error)
        val codiError = codiResult as ApiResult.Error
        assertEquals(409, codiError.statusCode)
        assertEquals("IDEMPOTENCY_CONFLICT", codiError.errorCode)

        val cfdiResult = client.stampCfdi(
            idempotencyKey = "idem-cfdi-001",
            paymentId = "pay_001",
            receptorRfc = "XAXX010101000",
            receptorCp = "06600",
            regimenFiscal = "626"
        )
        assertTrue(cfdiResult is ApiResult.Error)
        val cfdiError = cfdiResult as ApiResult.Error
        assertEquals(422, cfdiError.statusCode)
        assertEquals("SAT_COMPLIANCE_REJECTED", cfdiError.errorCode)

        val req1 = server.takeRequest()
        assertEquals("/api/v1/payments/codi/qr", req1.path)
        assertEquals("idem-codi-001", req1.getHeader("Idempotency-Key"))

        val req2 = server.takeRequest()
        assertEquals("/api/v1/invoices/cfdi", req2.path)
        assertEquals("idem-cfdi-001", req2.getHeader("Idempotency-Key"))
    }
}
