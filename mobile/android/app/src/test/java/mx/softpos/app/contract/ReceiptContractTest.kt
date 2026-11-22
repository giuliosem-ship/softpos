package mx.softpos.app.contract

import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.KernoJson
import mx.softpos.app.payments.HttpSalesApi
import mx.softpos.app.payments.SaleCard
import mx.softpos.app.payments.SendReceiptRequest
import mx.softpos.app.payments.invalidReceiptFields
import mx.softpos.app.payments.receiptSendResult
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

/**
 * Sale detail + receipt (AND-39) against bodies captured from pkg/api
 * (handlers_sales.go, handlers_receipts.go) by TestCaptureAndroidFixtures.
 */
class ReceiptContractTest {
    private lateinit var server: MockWebServer

    @BeforeEach fun setUp() { server = MockWebServer().apply { start() } }
    @AfterEach fun tearDown() { server.shutdown() }

    private fun respond(code: Int, fixture: String) = server.enqueue(MockResponse().setResponseCode(code).setBody(Fixtures.read(fixture)))
    private fun sales() = HttpSalesApi(server.url("/").toString(), OkHttpClient())
    private fun raw(fixture: String, key: String) =
        KernoJson.parseToJsonElement(Fixtures.read(fixture)).jsonObject.getValue(key).jsonPrimitive.content

    @Test
    fun cardSaleDetailCarriesReceiptAndMaskedCard() {
        respond(200, "sale_detail_card_receipt_200.json")
        val sale = (sales().getSale("m", "cp_4f543c27b1a576128a74d135") as ApiResult.Success).data
        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertEquals("/api/v1/merchants/m/sales/cp_4f543c27b1a576128a74d135", req.path)
        assertEquals("CAPTURED", sale.status)
        // RECEIPT_BASE_URL empty -> relative "/r/…": resolved against the API base so the link can be shared.
        val relative = raw("sale_detail_card_receipt_200.json", "receipt_url")
        assertTrue(relative.startsWith("/r/"))
        assertEquals(server.url(relative).toString(), sale.receiptUrl)
        assertEquals(raw("sale_detail_card_receipt_200.json", "receipt_expires_at"), sale.receiptExpiresAt)
        val card = sale.card!!
        assertEquals("visa", card.brand)
        assertEquals("**** 4242", card.maskedPan)
        assertEquals("4242", card.last4)
        assertTrue(card.authCode.isNotBlank())
    }

    @Test
    fun absoluteReceiptUrlIsKept() {
        respond(200, "sale_detail_card_receipt_absolute_200.json")
        val sale = (sales().getSale("m", "cp_4f543c27b1a576128a74d135") as ApiResult.Success).data
        val absolute = raw("sale_detail_card_receipt_absolute_200.json", "receipt_url")
        assertTrue(absolute.startsWith("https://pagos.kerno.mx/r/"))
        assertEquals(absolute, sale.receiptUrl)
    }

    @Test
    fun speiAndUnconfirmedSales() {
        respond(200, "sale_detail_spei_200.json")
        val spei = (sales().getSale("m", "spei_40012:FXSPEI0001") as ApiResult.Success).data
        assertEquals("/api/v1/merchants/m/sales/spei_40012:FXSPEI0001", server.takeRequest().path)
        assertNull(spei.card, "no card block for SPEI")
        assertTrue(spei.receiptUrl!!.contains("/r/"))

        respond(200, "sale_detail_unconfirmed_200.json")
        val created = (sales().getSale("m", "cp_83c307802ee611da295b6d3c") as ApiResult.Success).data
        assertNull(created.receiptUrl, "no receipt before the money is collected")
        assertNull(created.card?.last4, "the backend sends an empty card object before the tap")

        respond(404, "sale_not_found_404.json")
        assertEquals("SALE_NOT_FOUND", (sales().getSale("m", "nope") as ApiResult.Error).errorCode)
    }

    @Test
    fun maskedPanNeverKeepsMoreThanFourDigits() {
        // Defensive: even a misbehaving backend cannot make the app hold a longer PAN.
        val c = SaleCard(brand = "visa", maskedPan = "4152 3133 0000 4242", authCode = "A1").sanitized()
        assertEquals("**** 4242", c.maskedPan)
        assertEquals("4242", c.last4)
        assertNull(SaleCard(maskedPan = "**** 12").sanitized().last4, "fewer than 4 digits is not shown")
        assertFalse(c.toString().contains("4152"))
    }

    @Test
    fun sendReceiptRequestAndAnswers() {
        respond(200, "receipt_send_200.json")
        val ok = (sales().sendReceipt("m", "cp_1", SendReceiptRequest(email = "cliente@example.com", phone = "+525512345678")) as ApiResult.Success).data
        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/v1/merchants/m/sales/cp_1/receipt", req.path)
        val body = KernoJson.parseToJsonElement(req.body.readUtf8()).jsonObject
        assertEquals(setOf("email", "phone"), body.keys)
        assertTrue(ok.sent.getValue("email") && ok.sent.getValue("sms"))
        assertTrue(ok.receiptUrl.startsWith(server.url("/r/").toString()), "resolved like the detail link")

        respond(200, "receipt_send_200.json")
        sales().sendReceipt("m", "cp_1", SendReceiptRequest(email = "cliente@example.com"))
        assertEquals(setOf("email"), KernoJson.parseToJsonElement(server.takeRequest().body.readUtf8()).jsonObject.keys, "no empty phone sent")

        respond(502, "receipt_send_port_failed_502.json")
        val failed = sales().sendReceipt("m", "cp_1", SendReceiptRequest(phone = "+525512345678")) as ApiResult.Error
        assertEquals(502, failed.statusCode)
        assertEquals(mapOf("sms" to false), failed.receiptSendResult()?.sent)

        respond(422, "receipt_send_invalid_422.json")
        val invalid = sales().sendReceipt("m", "cp_1", SendReceiptRequest(email = "x", phone = "1")) as ApiResult.Error
        assertEquals("RECEIPT_DESTINATION_INVALID", invalid.errorCode)
        assertEquals(setOf("email", "phone"), invalid.invalidReceiptFields())

        respond(409, "receipt_send_not_confirmed_409.json")
        assertEquals("SALE_NOT_CONFIRMED", (sales().sendReceipt("m", "cp_1", SendReceiptRequest(email = "a@b.mx")) as ApiResult.Error).errorCode)
        respond(403, "receipt_send_forbidden_403.json")
        assertEquals("ROLE_FORBIDDEN", (sales().sendReceipt("m", "cp_1", SendReceiptRequest(email = "a@b.mx")) as ApiResult.Error).errorCode)
        respond(503, "receipt_send_not_configured_503.json")
        val off = sales().sendReceipt("m", "cp_1", SendReceiptRequest(email = "a@b.mx")) as ApiResult.Error
        assertEquals("RECEIPTS_NOT_CONFIGURED", off.errorCode)
        assertNull(off.receiptSendResult())
    }
}
