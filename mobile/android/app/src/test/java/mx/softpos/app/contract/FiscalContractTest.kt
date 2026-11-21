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
import mx.softpos.app.fiscal.CancelMotivo
import mx.softpos.app.fiscal.CancellationStatus
import mx.softpos.app.fiscal.Invoice
import mx.softpos.app.fiscal.InvoiceKind
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
import mx.softpos.app.ui.fiscal.CancelError
import mx.softpos.app.ui.fiscal.CsdGate
import mx.softpos.app.ui.fiscal.InvoiceDetailViewModel
import mx.softpos.app.ui.fiscal.InvoiceError
import mx.softpos.app.ui.fiscal.InvoiceFormViewModel
import mx.softpos.app.ui.fiscal.ReceptorField
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
 * CSD, invoices and SAT catalogs (batch J). Fixtures captured from the Go
 * handlers (pkg/api, DEV/memory mode, mock PAC); response status codes below
 * are the ones the handlers returned.
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
        assertEquals("CACX7605101P8", c.rfc)
        assertEquals("2030-11-20T04:12:09Z", c.validTo)
        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertEquals("/api/v1/merchants/mer_1/csd", req.path)

        respond(404, "csd_not_found_404.json")
        assertTrue((csd().getCsd("mer_1") as ApiResult.Error).isCsdNotFound())

        // A stored certificate that expired afterwards: 200 with status EXPIRED.
        respond(200, "csd_get_expired_200.json")
        val expired = (csd().getCsd("mer_1") as ApiResult.Success).data
        assertEquals("EXPIRED", expired.status)
        assertFalse(expired.isActive)
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
        assertEquals("WRONG_PASSWORD", e.csdInvalidReason())
        respond(422, "csd_put_missing_key_422.json")
        val missing = csd().uploadCsd("mer_1", CsdFile("a.cer", byteArrayOf(1)), CsdFile("a.key", byteArrayOf(2)), "x".toCharArray()) as ApiResult.Error
        assertEquals("MISSING_KEY", missing.csdInvalidReason())
        respond(403, "csd_put_forbidden_403.json")
        assertEquals("ROLE_FORBIDDEN", (csd().uploadCsd("mer_1", CsdFile("a.cer", byteArrayOf(1)), CsdFile("a.key", byteArrayOf(2)), "x".toCharArray()) as ApiResult.Error).errorCode)
    }

    @Test
    fun createInvoiceShapesAndErrors() {
        respond(201, "invoice_create_201_stamped.json")
        val inv = (invoices().create("mer_1", "ik_1", CreateInvoiceRequest(listOf("sale_a", "sale_b"), receptor)) as ApiResult.Success).data
        assertEquals(InvoiceStatus.STAMPED, inv.status)
        assertEquals(60_936L, inv.totalCents)
        assertEquals("1", inv.folio)
        assertEquals(listOf("cp_dfcd7d7673079ad93e82b339", "spei_40012:SPEIAND0001"), inv.saleIds)
        assertFalse(inv.receptor.toString().contains("EKU9003173C9"), "receptor never printed")

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/v1/merchants/mer_1/invoices", req.path)
        assertEquals("ik_1", req.getHeader("Idempotency-Key"))
        val json = KernoJson.parseToJsonElement(req.body.readUtf8()).jsonObject
        assertEquals(listOf("sale_a", "sale_b"), json["sale_ids"]!!.jsonArray.map { it.jsonPrimitive.content })
        val r = json["receptor"]!!.jsonObject
        assertEquals(setOf("rfc", "nombre", "regimen_fiscal", "codigo_postal", "uso_cfdi", "email"), r.keys)

        // PAC down is not a 5xx: 201 with PENDING + error PAC_UNAVAILABLE (the backoffice retries it).
        respond(201, "invoice_create_201_pending.json")
        val pending = (invoices().create("mer_1", "ik_2", CreateInvoiceRequest(listOf("sale_c"), receptor.copy(email = null))) as ApiResult.Success).data
        assertEquals(InvoiceStatus.PENDING, pending.status)
        assertEquals("PAC_UNAVAILABLE", pending.error)
        assertNull(pending.uuid)
        assertFalse(KernoJson.parseToJsonElement(server.takeRequest().body.readUtf8()).jsonObject["receptor"]!!.jsonObject.containsKey("email"), "email omitted when absent")

        respond(201, "invoice_create_201_failed.json")
        val failed = (invoices().create("mer_1", "ik_2b", CreateInvoiceRequest(listOf("sale_d"), receptor)) as ApiResult.Success).data
        assertEquals(InvoiceStatus.FAILED, failed.status)
        assertEquals("CFDI40145", failed.error)

        respond(409, "invoice_sale_already_invoiced_409.json")
        assertEquals("SALE_ALREADY_INVOICED", (invoices().create("mer_1", "ik_3", CreateInvoiceRequest(listOf("sale_a"), receptor)) as ApiResult.Error).errorCode)

        respond(422, "invoice_receptor_invalid_422.json")
        val invalid = invoices().create("mer_1", "ik_4", CreateInvoiceRequest(listOf("sale_a"), receptor)) as ApiResult.Error
        assertEquals(setOf("codigo_postal", "regimen_fiscal", "uso_cfdi"), invalid.invalidReceptorFields().toSet())
        assertEquals(
            setOf(ReceptorField.CODIGO_POSTAL, ReceptorField.REGIMEN, ReceptorField.USO_CFDI),
            invalid.invalidReceptorFields().mapNotNull(InvoiceFormViewModel::fieldOf).toSet()
        )

        for ((code, fixture) in listOf(
            422 to "invoice_csd_not_found_422.json", 422 to "invoice_csd_expired_422.json",
            422 to "invoice_fiscal_profile_incomplete_422.json", 422 to "invoice_sale_not_found_422.json",
            409 to "invoice_idempotency_key_reused_409.json", 422 to "invoice_replacement_invalid_422.json"
        )) {
            respond(code, fixture)
            val err = invoices().create("mer_1", "ik_x", CreateInvoiceRequest(listOf("sale_a"), receptor)) as ApiResult.Error
            assertEquals(code, err.statusCode)
            assertTrue(err.errorCode.isNotBlank() && !err.errorCode.startsWith("HTTP_"), fixture)
            server.takeRequest()
        }
    }

    @Test
    fun listGetDownloadAndCancel() {
        respond(200, "invoices_page.json")
        val page = (invoices().list("mer_1") as ApiResult.Success).data
        assertEquals(listOf(InvoiceStatus.STAMPED, InvoiceStatus.FAILED, InvoiceStatus.PENDING), page.items.map { it.status })
        assertEquals("MTc5NTIzNDMyOTc3NTkwNTAwMHxpbnZfODJkYjZkYTA2NjJhZTU2MmQxMzJmMThl", page.nextCursor)
        assertEquals("/api/v1/merchants/mer_1/invoices?limit=50", server.takeRequest().path)

        respond(200, "invoices_last_page.json")
        val last = (invoices().list("mer_1", before = page.nextCursor) as ApiResult.Success).data
        assertNull(last.nextCursor)
        assertEquals(4, last.items.size)
        assertEquals("/api/v1/merchants/mer_1/invoices?limit=50&before=MTc5NTIzNDMyOTc3NTkwNTAwMHxpbnZfODJkYjZkYTA2NjJhZTU2MmQxMzJmMThl", server.takeRequest().path)

        respond(200, "invoice_get_200.json")
        assertEquals(InvoiceStatus.STAMPED, (invoices().get("mer_1", "inv_03") as ApiResult.Success).data.status)
        assertEquals("/api/v1/merchants/mer_1/invoices/inv_03", server.takeRequest().path)
        respond(404, "invoice_not_found_404.json")
        assertEquals("INVOICE_NOT_FOUND", (invoices().get("mer_1", "inv_x") as ApiResult.Error).errorCode)
        server.takeRequest()

        val pdf = "%PDF-1.7\n".toByteArray()
        server.enqueue(MockResponse().setHeader("Content-Type", "application/pdf").setBody(Buffer().write(pdf)))
        assertArrayEquals(pdf, (invoices().download("mer_1", "inv_01", InvoiceFileKind.PDF) as ApiResult.Success).data)
        assertEquals("/api/v1/merchants/mer_1/invoices/inv_01/pdf", server.takeRequest().path)
        server.enqueue(MockResponse().setHeader("Content-Type", "application/xml").setBody("<cfdi:Comprobante/>"))
        assertTrue(invoices().download("mer_1", "inv_01", InvoiceFileKind.XML) is ApiResult.Success)
        assertEquals("/api/v1/merchants/mer_1/invoices/inv_01/xml", server.takeRequest().path)

        // Receptor acceptance needed: 202 CANCEL_REQUESTED.
        respond(202, "invoice_cancel_requested_202.json")
        val requested = invoices().cancel("mer_1", "inv_01", CancelInvoiceRequest("02")) as ApiResult.Success
        assertEquals(202, requested.statusCode)
        assertEquals(InvoiceStatus.CANCEL_REQUESTED, requested.data.status)
        val req = server.takeRequest()
        assertEquals("/api/v1/merchants/mer_1/invoices/inv_01/cancel", req.path)
        assertEquals("""{"motivo":"02"}""", req.body.readUtf8())

        respond(200, "invoice_cancel_canceled_200.json")
        val canceled = (invoices().cancel("mer_1", "inv_01", CancelInvoiceRequest("01", "1C8799D7-BE03-431A-9B02-C138420A9C6C")) as ApiResult.Success).data
        assertEquals(InvoiceStatus.CANCELED, canceled.status)
        assertEquals("""{"motivo":"01","folio_sustitucion":"1C8799D7-BE03-431A-9B02-C138420A9C6C"}""", server.takeRequest().body.readUtf8())

        for ((code, fixture) in listOf(
            422 to "invoice_cancel_invalid_422.json", 422 to "invoice_cancel_replacement_not_stamped_422.json",
            422 to "invoice_cancel_motivo04_individual_422.json", 409 to "invoice_cancel_not_cancelable_409.json",
            502 to "invoice_cancel_pac_unavailable_502.json", 403 to "invoice_cancel_forbidden_403.json"
        )) {
            respond(code, fixture)
            assertEquals(code, (invoices().cancel("mer_1", "inv_01", CancelInvoiceRequest("02")) as ApiResult.Error).statusCode)
            server.takeRequest()
        }
    }

    @Test
    fun invoiceShapeCarriesKindRetentionCancellationAndReplacement() {
        fun decode(f: String) = KernoJson.decodeFromString(Invoice.serializer(), Fixtures.read(f))
        val pm = decode("invoice_get_200.json")
        assertEquals(InvoiceKind.INDIVIDUAL, pm.kind)
        assertEquals(664L, pm.isrRetenidoCents)
        assertEquals(pm.subtotalCents + pm.ivaCents - pm.isrRetenidoCents, pm.totalCents)
        assertNull(pm.cancellation)

        val global = decode("invoice_get_global_200.json")
        assertTrue(global.isGlobal)
        assertEquals("M2026-11", global.globalPeriod)
        assertEquals(listOf("02", "03", "04"), CancelMotivo.allowedFor(global))
        assertEquals(listOf("01", "02", "03"), CancelMotivo.allowedFor(pm))

        val rejected = decode("invoice_get_cancel_rejected_200.json")
        assertEquals(InvoiceStatus.STAMPED, rejected.status)
        assertEquals(CancellationStatus.REJECTED, rejected.cancellation?.status)
        assertTrue(rejected.cancellationRejected)
        assertEquals(CancellationStatus.REQUESTED, decode("invoice_cancel_requested_202.json").cancellation?.status)

        val canceled = decode("invoice_cancel_canceled_200.json")
        assertEquals("01", canceled.cancellation?.motivo)
        assertEquals("1C8799D7-BE03-431A-9B02-C138420A9C6C", canceled.cancellation?.folioSustitucion)
        assertEquals(canceled.invoiceId, decode("invoice_create_201_replacement.json").replacesInvoiceId)
    }

    /** Motivo 01 end to end: substitute first (replaces_invoice_id), then cancel the original with its UUID. */
    @Test
    fun motivo01SubstituteThenCancelOverHttp() {
        val original = KernoJson.decodeFromString(Invoice.serializer(), Fixtures.read("invoice_get_200.json"))
        respond(200, "csd_get_200.json")
        respond(200, "invoice_get_200.json")
        respond(200, "sat_catalogs_200.json")
        val form = InvoiceFormViewModel(emptyList(), invoices(), csd(), SatCatalogCache(invoices()), { "mer_1" }, dispatcher,
            replacesInvoiceId = original.invoiceId)
        form.load()
        server.takeRequest()
        assertEquals("/api/v1/merchants/mer_1/invoices/${original.invoiceId}", server.takeRequest().path)
        server.takeRequest()
        assertEquals(original.saleIds, form.state.value.saleIds)
        assertEquals("EKU9003173C9", form.state.value.form.rfc)
        form.onUsoCfdi("G01")

        respond(201, "invoice_create_201_replacement.json")
        form.submit()
        val body = KernoJson.parseToJsonElement(server.takeRequest().body.readUtf8()).jsonObject
        assertEquals(original.invoiceId, body["replaces_invoice_id"]!!.jsonPrimitive.content)
        assertEquals("G01", body["receptor"]!!.jsonObject["uso_cfdi"]!!.jsonPrimitive.content)
        val substitute = form.state.value.result!!
        assertTrue(substitute.isStamped)

        // Normal invoices never send the field.
        respond(201, "invoice_create_201_stamped.json")
        invoices().create("mer_1", "k", CreateInvoiceRequest(listOf("s"), receptor))
        assertFalse(KernoJson.parseToJsonElement(server.takeRequest().body.readUtf8()).jsonObject.containsKey("replaces_invoice_id"))

        respond(200, "invoice_get_200.json")
        val detail = InvoiceDetailViewModel(original.invoiceId, invoices(), { "mer_1" }, { "owner" }, { _, _, _ -> null }, dispatcher,
            substituteUuid = substitute.uuid)
        detail.load()
        server.takeRequest()
        assertEquals(CancelMotivo.WITH_RELATION, detail.state.value.motivo)
        respond(422, "invoice_cancel_replacement_not_stamped_422.json")
        detail.cancel()
        server.takeRequest()
        assertEquals(CancelError.REPLACEMENT_NOT_STAMPED, detail.state.value.cancelError)
        respond(200, "invoice_cancel_canceled_200.json")
        detail.cancel()
        assertEquals("""{"motivo":"01","folio_sustitucion":"${substitute.uuid}"}""", server.takeRequest().body.readUtf8())
        assertEquals(InvoiceStatus.CANCELED, detail.state.value.invoice?.status)

        // 409 INVOICE_NOT_CANCELABLE reloads the invoice.
        respond(200, "invoice_get_200.json")
        val again = InvoiceDetailViewModel(original.invoiceId, invoices(), { "mer_1" }, { "owner" }, { _, _, _ -> null }, dispatcher)
        again.load(); server.takeRequest()
        respond(409, "invoice_cancel_not_cancelable_409.json")
        respond(200, "invoice_cancel_canceled_200.json")
        again.cancel()
        server.takeRequest(); server.takeRequest()
        assertEquals(CancelError.NOT_CANCELABLE, again.state.value.cancelError)
        assertEquals(InvoiceStatus.CANCELED, again.state.value.invoice?.status)
    }

    /** The backend sends no ETag (only Cache-Control and X-SAT-Catalog-Version): after the TTL the cache refetches in full. */
    @Test
    fun catalogsDecodeAndRefetchWithoutEtag() {
        val headers = mapOf("Cache-Control" to "private, max-age=3600", "X-SAT-Catalog-Version" to "2026.11-1")
        respond(200, "sat_catalogs_200.json", headers)
        var now = 0L
        val cache = SatCatalogCache(invoices(), ttlMillis = 10) { now }
        val cat = (cache.get() as ApiResult.Success).data
        assertEquals("/api/v1/catalogs/sat?names=regimen_fiscal%2Cuso_cfdi", server.takeRequest().path)
        assertTrue(cat.regimenFiscal.size >= 19 && cat.usoCfdi.size >= 24)
        val fisica = ReceptorValidator.regimenesFor(cat, Persona.FISICA).map { it.code }
        val moral = ReceptorValidator.regimenesFor(cat, Persona.MORAL).map { it.code }
        assertTrue("612" in fisica && "626" in fisica && "601" !in fisica)
        assertTrue("601" in moral && "626" in moral && "612" !in moral)
        assertTrue(ReceptorValidator.usosFor(cat, "601").any { it.code == "G03" })

        now = 20
        respond(200, "sat_catalogs_200.json", headers)
        assertEquals(cat, (cache.get() as ApiResult.Success).data)
        assertNull(server.takeRequest().getHeader("If-None-Match"), "no ETag was ever received")
    }

    @Test
    fun formOverHttpRetriesWithSameKeyAfter503() {
        respond(200, "csd_get_200.json")
        respond(200, "sat_catalogs_200.json")
        val vm = InvoiceFormViewModel(listOf("sale_a", "sale_b"), invoices(), csd(), SatCatalogCache(invoices()), { "mer_1" }, dispatcher)
        vm.load()
        server.takeRequest(); server.takeRequest()
        assertEquals(CsdGate.ACTIVE, vm.state.value.csd)
        vm.onRfc("EKU9003173C9"); vm.onNombre("ESCUELA KEMPER URGATE"); vm.onRegimen("601"); vm.onUsoCfdi("G03"); vm.onCodigoPostal("26015")

        // 503 INVOICES_UNAVAILABLE (store down) is the create's only 5xx: the outcome is unknown.
        server.enqueue(MockResponse().setResponseCode(503).setBody("""{"error":"INVOICES_UNAVAILABLE"}"""))
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
