package mx.softpos.app.contract

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.KernoJson
import mx.softpos.app.fiscal.CancelInvoiceRequest
import mx.softpos.app.fiscal.CreateInvoiceRequest
import mx.softpos.app.fiscal.CsdFile
import mx.softpos.app.fiscal.HttpCsdApi
import mx.softpos.app.fiscal.HttpInvoiceApi
import mx.softpos.app.fiscal.InvoiceFileKind
import mx.softpos.app.fiscal.InvoiceStatus
import mx.softpos.app.fiscal.Persona
import mx.softpos.app.fiscal.Receptor
import mx.softpos.app.fiscal.ReceptorValidator
import mx.softpos.app.fiscal.SatCatalogCache
import mx.softpos.app.fiscal.csdInvalidReason
import mx.softpos.app.fiscal.invalidReceptorFields
import mx.softpos.app.fiscal.isCsdNotFound
import mx.softpos.app.ui.fiscal.CsdGate
import mx.softpos.app.ui.fiscal.InvoiceError
import mx.softpos.app.ui.fiscal.InvoiceFormViewModel
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okio.Buffer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertArrayEquals
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

/**
 * CSD, invoices and SAT catalogs (batch J). These fixtures are handwritten
 * from the agreed contract, not captured: re-verify them against the Go
 * handlers' responses once they land.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class FiscalContractTest {
    private lateinit var server: MockWebServer
    private val dispatcher = UnconfinedTestDispatcher()

    @BeforeEach fun setUp() {
        server = MockWebServer().apply { start() }
        Dispatchers.setMain(dispatcher)
    }

    @AfterEach fun tearDown() {
        server.shutdown()
        Dispatchers.resetMain()
    }

    private fun respond(code: Int, fixture: String, headers: Map<String, String> = emptyMap()) =
        server.enqueue(MockResponse().setResponseCode(code).setBody(Fixtures.read(fixture)).apply { headers.forEach { (k, v) -> setHeader(k, v) } })

    private fun csd() = HttpCsdApi(server.url("/").toString(), OkHttpClient())
    private fun invoices() = HttpInvoiceApi(server.url("/").toString(), OkHttpClient())

    private val receptor = Receptor("TEST010203AB1", "JUANA PEREZ", "612", "06600", "G03", "juana@example.com")

    @Test
    fun csdGetAndNotFound() {
        respond(200, "csd_get_200.json")
        val c = (csd().getCsd("mer_1") as ApiResult.Success).data
        assertTrue(c.isActive)
        assertEquals("30001000000500003416", c.serial)
        assertEquals("2029-01-01T00:00:00Z", c.validTo)
        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertEquals("/api/v1/merchants/mer_1/csd", req.path)

        respond(404, "csd_not_found_404.json")
        assertTrue((csd().getCsd("mer_1") as ApiResult.Error).isCsdNotFound())
    }

    @Test
    fun csdUploadIsMultipartWithCerKeyPassword() {
        respond(200, "csd_put_200.json")
        val password = "contraseña".toCharArray()
        val r = csd().uploadCsd("mer_1", CsdFile("a.cer", byteArrayOf(0x30, 1)), CsdFile("a.key", byteArrayOf(0x30, 2)), password)
        assertEquals("ACTIVE", (r as ApiResult.Success).data.status)
        val req = server.takeRequest()
        assertEquals("PUT", req.method)
        assertEquals("/api/v1/merchants/mer_1/csd", req.path)
        assertTrue(req.getHeader("Content-Type")!!.startsWith("multipart/form-data; boundary="))
        val body = req.body.readUtf8()
        assertTrue(body.contains("""name="cer"; filename="a.cer""""))
        assertTrue(body.contains("""name="key"; filename="a.key""""))
        assertTrue(body.contains("name=\"password\"\r\nContent-Length: 11\r\n\r\ncontraseña\r\n"), "UTF-8 value without filename")

        respond(422, "csd_put_invalid_422.json")
        val e = csd().uploadCsd("mer_1", CsdFile("a.cer", byteArrayOf(1)), CsdFile("a.key", byteArrayOf(2)), "x".toCharArray()) as ApiResult.Error
        assertEquals("KEY_PASSWORD_INVALID", e.csdInvalidReason())
    }

    @Test
    fun createInvoiceShapesAndErrors() {
        respond(201, "invoice_create_201_stamped.json")
        val inv = (invoices().create("mer_1", "ik_1", CreateInvoiceRequest(listOf("sale_a", "sale_b"), receptor)) as ApiResult.Success).data
        assertEquals(InvoiceStatus.STAMPED, inv.status)
        assertEquals(11_600L, inv.totalCents)
        assertEquals(inv.subtotalCents + inv.ivaCents, inv.totalCents)
        assertEquals(listOf("sale_a", "sale_b"), inv.saleIds)
        assertFalse(inv.receptor.toString().contains("TEST010203AB1"), "receptor never printed")

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/v1/merchants/mer_1/invoices", req.path)
        assertEquals("ik_1", req.getHeader("Idempotency-Key"))
        val json = KernoJson.parseToJsonElement(req.body.readUtf8()).jsonObject
        assertEquals(listOf("sale_a", "sale_b"), json["sale_ids"]!!.jsonArray.map { it.jsonPrimitive.content })
        val r = json["receptor"]!!.jsonObject
        assertEquals(setOf("rfc", "nombre", "regimen_fiscal", "codigo_postal", "uso_cfdi", "email"), r.keys)

        respond(201, "invoice_create_201_pending.json")
        val pending = (invoices().create("mer_1", "ik_2", CreateInvoiceRequest(listOf("sale_c"), receptor.copy(email = null))) as ApiResult.Success).data
        assertEquals(InvoiceStatus.PENDING, pending.status)
        assertNull(pending.uuid)
        assertFalse(KernoJson.parseToJsonElement(server.takeRequest().body.readUtf8()).jsonObject["receptor"]!!.jsonObject.containsKey("email"), "email omitted when absent")

        respond(409, "invoice_sale_already_invoiced_409.json")
        assertEquals("SALE_ALREADY_INVOICED", (invoices().create("mer_1", "ik_3", CreateInvoiceRequest(listOf("sale_a"), receptor)) as ApiResult.Error).errorCode)

        respond(422, "invoice_receptor_invalid_422.json")
        val invalid = invoices().create("mer_1", "ik_4", CreateInvoiceRequest(listOf("sale_a"), receptor)) as ApiResult.Error
        assertEquals(listOf("rfc", "regimen_fiscal", "codigo_postal"), invalid.invalidReceptorFields())
    }

    @Test
    fun listGetDownloadAndCancel() {
        respond(200, "invoices_page.json")
        val page = (invoices().list("mer_1") as ApiResult.Success).data
        assertEquals(listOf(InvoiceStatus.PENDING, InvoiceStatus.CANCELED), page.items.map { it.status })
        assertEquals("MTc2MzY1NTk5OTAwMHxpbnZfMDE=", page.nextCursor)
        assertEquals("/api/v1/merchants/mer_1/invoices?limit=50", server.takeRequest().path)

        respond(200, "invoices_last_page.json")
        assertNull((invoices().list("mer_1", before = page.nextCursor) as ApiResult.Success).data.nextCursor)
        assertEquals("/api/v1/merchants/mer_1/invoices?limit=50&before=MTc2MzY1NTk5OTAwMHxpbnZfMDE%3D", server.takeRequest().path)

        respond(200, "invoice_get_failed.json")
        val failed = (invoices().get("mer_1", "inv_03") as ApiResult.Success).data
        assertEquals("CFDI40145", failed.error)
        assertEquals("/api/v1/merchants/mer_1/invoices/inv_03", server.takeRequest().path)

        val pdf = "%PDF-1.7\n".toByteArray()
        server.enqueue(MockResponse().setHeader("Content-Type", "application/pdf").setBody(Buffer().write(pdf)))
        assertArrayEquals(pdf, (invoices().download("mer_1", "inv_01", InvoiceFileKind.PDF) as ApiResult.Success).data)
        assertEquals("/api/v1/merchants/mer_1/invoices/inv_01/pdf", server.takeRequest().path)
        server.enqueue(MockResponse().setHeader("Content-Type", "application/xml").setBody("<cfdi:Comprobante/>"))
        assertTrue(invoices().download("mer_1", "inv_01", InvoiceFileKind.XML) is ApiResult.Success)
        assertEquals("/api/v1/merchants/mer_1/invoices/inv_01/xml", server.takeRequest().path)

        respond(200, "invoice_cancel_requested_200.json")
        val c = (invoices().cancel("mer_1", "inv_01", CancelInvoiceRequest("01", "6F1B2C3D-0000-4A5B-8C9D-112233445599")) as ApiResult.Success).data
        assertEquals(InvoiceStatus.CANCEL_REQUESTED, c.status)
        val req = server.takeRequest()
        assertEquals("/api/v1/merchants/mer_1/invoices/inv_01/cancel", req.path)
        assertEquals("""{"motivo":"01","folio_sustitucion":"6F1B2C3D-0000-4A5B-8C9D-112233445599"}""", req.body.readUtf8())

        respond(200, "invoice_cancel_canceled_200.json")
        assertEquals(InvoiceStatus.CANCELED, (invoices().cancel("mer_1", "inv_01", CancelInvoiceRequest("02")) as ApiResult.Success).data.status)
        assertEquals("""{"motivo":"02"}""", server.takeRequest().body.readUtf8())
    }

    @Test
    fun catalogsDecodeAndRevalidateWithEtag() {
        respond(200, "sat_catalogs_200.json", mapOf("ETag" to "\"sat-2026-11\""))
        var now = 0L
        val cache = SatCatalogCache(invoices(), ttlMillis = 10) { now }
        val cat = (cache.get() as ApiResult.Success).data
        assertEquals("/api/v1/catalogs/sat?names=regimen_fiscal%2Cuso_cfdi", server.takeRequest().path)
        assertEquals(listOf("605", "612", "616", "626"), ReceptorValidator.regimenesFor(cat, Persona.FISICA).map { it.code })
        assertEquals(listOf("601", "626"), ReceptorValidator.regimenesFor(cat, Persona.MORAL).map { it.code })
        assertEquals(listOf("D01", "S01"), ReceptorValidator.usosFor(cat, "605").map { it.code })

        now = 20
        server.enqueue(MockResponse().setResponseCode(304))
        assertEquals(cat, (cache.get() as ApiResult.Success).data)
        assertEquals("\"sat-2026-11\"", server.takeRequest().getHeader("If-None-Match"))
    }

    @Test
    fun formOverHttpRetriesWithSameKeyAfter502() {
        respond(200, "csd_get_200.json")
        respond(200, "sat_catalogs_200.json")
        val vm = InvoiceFormViewModel(listOf("sale_a", "sale_b"), invoices(), csd(), SatCatalogCache(invoices()), { "mer_1" }, dispatcher)
        vm.load()
        server.takeRequest(); server.takeRequest()
        assertEquals(CsdGate.ACTIVE, vm.state.value.csd)
        vm.onRfc("TEST010203AB1"); vm.onNombre("JUANA PEREZ"); vm.onRegimen("612"); vm.onUsoCfdi("G03"); vm.onCodigoPostal("06600")

        server.enqueue(MockResponse().setResponseCode(502).setBody("""{"error":"PAC_UNAVAILABLE"}"""))
        vm.submit()
        assertEquals(InvoiceError.PROVIDER_UNAVAILABLE, vm.state.value.error)
        val first = server.takeRequest().getHeader("Idempotency-Key")

        respond(201, "invoice_create_201_stamped.json")
        vm.submit()
        assertEquals(first, server.takeRequest().getHeader("Idempotency-Key"))
        assertEquals(InvoiceStatus.STAMPED, vm.state.value.result?.status)

        // No CSD: the form is blocked before anything is sent.
        val vm2 = InvoiceFormViewModel(listOf("sale_a"), invoices(), csd(), SatCatalogCache(invoices()), { "mer_1" }, dispatcher)
        respond(404, "csd_not_found_404.json")
        respond(200, "sat_catalogs_200.json")
        vm2.load()
        assertEquals(CsdGate.MISSING, vm2.state.value.csd)
        assertFalse(vm2.state.value.canSubmit)
    }
}
