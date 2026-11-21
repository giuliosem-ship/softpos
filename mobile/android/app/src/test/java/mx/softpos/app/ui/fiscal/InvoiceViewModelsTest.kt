package mx.softpos.app.ui.fiscal

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_CODE
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.fiscal.CancelMotivo
import mx.softpos.app.fiscal.CsdInfo
import mx.softpos.app.fiscal.FakeInvoiceApi
import mx.softpos.app.fiscal.InvoiceFileKind
import mx.softpos.app.fiscal.InvoiceKind
import mx.softpos.app.fiscal.InvoicePage
import mx.softpos.app.fiscal.InvoiceStatus
import mx.softpos.app.fiscal.Persona
import mx.softpos.app.fiscal.SatCatalogCache
import mx.softpos.app.fiscal.stamped
import mx.softpos.app.payments.Sale
import mx.softpos.app.payments.SalesApi
import mx.softpos.app.payments.SalesPage
import mx.softpos.app.ui.merchant.SalesViewModel
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

@OptIn(ExperimentalCoroutinesApi::class)
class InvoiceViewModelsTest {
    private val dispatcher = UnconfinedTestDispatcher()
    private val api = FakeInvoiceApi()
    private val csd = FakeCsdApi(current = ApiResult.Success(CsdInfo("1", "TEST010203AB1", "2025-01-01T00:00:00Z", "2029-01-01T00:00:00Z", "ACTIVE"), 200))
    private var keys = 0

    @BeforeEach fun setUp() = Dispatchers.setMain(dispatcher)
    @AfterEach fun tearDown() = Dispatchers.resetMain()

    private fun form(sales: List<String> = listOf("s1", "s2")) =
        InvoiceFormViewModel(sales, api, csd, SatCatalogCache(api), { "mer_1" }, dispatcher) { "ik_${++keys}" }.also { it.load() }

    private fun InvoiceFormViewModel.fillValid() {
        onRfc("test010203ab1")
        onNombre("Juana Pérez")
        onRegimen("612")
        onUsoCfdi("G03")
        onCodigoPostal("06600")
    }

    @Test
    fun personaDrivesCatalogFilteringAndResetsIncompatibleChoices() {
        val vm = form()
        vm.onRfc("TEST010203AB1")
        assertEquals(Persona.FISICA, vm.state.value.persona)
        assertEquals(listOf("612", "626"), vm.state.value.regimenOptions.map { it.code })
        vm.onRegimen("612")
        vm.onUsoCfdi("D01")
        // Switching to a persona moral RFC drops 612 (física only) and therefore D01.
        vm.onRfc("ABC010203AB1")
        assertEquals("", vm.state.value.form.regimen)
        assertEquals("", vm.state.value.form.usoCfdi)
        vm.onRegimen("626")
        vm.onUsoCfdi("D01")
        assertTrue(vm.state.value.usoOptions.none { it.code == "D01" })
    }

    @Test
    fun submitSendsReceptorAndShowsStampedResult() {
        val vm = form()
        vm.fillValid()
        vm.onEmail("juana@example.com")
        vm.submit()
        val (key, req) = api.creates.single()
        assertEquals("ik_1", key)
        assertEquals(listOf("s1", "s2"), req.saleIds)
        assertEquals("TEST010203AB1", req.receptor.rfc)
        assertEquals("612", req.receptor.regimenFiscal)
        assertEquals("juana@example.com", req.receptor.email)
        assertEquals(InvoiceStatus.STAMPED, vm.state.value.result?.status)
        assertFalse(vm.state.value.canSubmit)
    }

    @Test
    fun clientValidationMarksFieldsWithoutCalling() {
        val vm = form()
        vm.onRfc("BAD")
        vm.onEmail("nope")
        vm.submit()
        assertTrue(api.creates.isEmpty())
        assertEquals(
            setOf(ReceptorField.RFC, ReceptorField.NOMBRE, ReceptorField.REGIMEN, ReceptorField.USO_CFDI, ReceptorField.CODIGO_POSTAL, ReceptorField.EMAIL),
            vm.state.value.invalidFields
        )
    }

    @Test
    fun keyKeptOnUnknownOutcomeDroppedOn4xxAndOnChange() {
        val vm = form()
        vm.fillValid()
        api.createResults += ApiResult.Error(502, "PAC_UNAVAILABLE", """{"error":"PAC_UNAVAILABLE"}""")
        api.createResults += ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, "")
        api.createResults += ApiResult.Error(409, "SALE_ALREADY_INVOICED", """{"error":"SALE_ALREADY_INVOICED"}""")
        vm.submit()
        assertEquals(InvoiceError.PROVIDER_UNAVAILABLE, vm.state.value.error)
        vm.submit()
        assertEquals(InvoiceError.NETWORK, vm.state.value.error)
        vm.submit()
        assertEquals(InvoiceError.ALREADY_INVOICED, vm.state.value.error)
        assertEquals(listOf("ik_1", "ik_1", "ik_1"), api.creates.map { it.first }, "same key while the outcome is unknown")
        assertFalse(vm.hasPendingKey, "a 4xx releases the key")

        api.createResults += ApiResult.Error(500, "HTTP_500", "")
        vm.submit()
        assertEquals("ik_2", api.creates.last().first)
        vm.onCodigoPostal("06700") // different body = different request
        vm.submit()
        assertNotEquals("ik_2", api.creates.last().first)
    }

    @Test
    fun receptorInvalidFromBackendMarksFields() {
        val vm = form()
        vm.fillValid()
        api.createResults += ApiResult.Error(
            422, "RECEPTOR_INVALID", """{"error":"RECEPTOR_INVALID","fields":{"receptor.nombre":"REQUIRED","codigo_postal":"INVALID_FORMAT","other":"X"}}"""
        )
        vm.submit()
        assertEquals(InvoiceError.RECEPTOR_INVALID, vm.state.value.error)
        assertEquals(setOf(ReceptorField.NOMBRE, ReceptorField.CODIGO_POSTAL), vm.state.value.invalidFields)
        vm.onNombre("JUANA PEREZ")
        assertEquals(setOf(ReceptorField.CODIGO_POSTAL), vm.state.value.invalidFields)
    }

    @Test
    fun missingCsdBlocksAndRoutes() {
        csd.current = ApiResult.Error(404, "CSD_NOT_FOUND", """{"error":"CSD_NOT_FOUND"}""")
        val vm = form()
        assertEquals(CsdGate.MISSING, vm.state.value.csd)
        vm.fillValid()
        assertFalse(vm.state.value.canSubmit)
        vm.submit()
        assertTrue(api.creates.isEmpty())

        csd.current = ApiResult.Success(CsdInfo(status = "ACTIVE"), 200)
        vm.load() // back from the CSD screen
        assertEquals(CsdGate.ACTIVE, vm.state.value.csd)
        assertEquals(1, api.catalogEtags.size, "catalogs not refetched")
        assertTrue(vm.state.value.canSubmit)

        csd.current = ApiResult.Success(CsdInfo(status = "EXPIRED"), 200)
        vm.load()
        assertEquals(CsdGate.MISSING, vm.state.value.csd)
    }

    @Test
    fun listPaginatesAndDropsDuplicates() {
        api.pages[null] = ApiResult.Success(InvoicePage(listOf(stamped("a"), stamped("b")), "c1"), 200)
        api.pages["c1"] = ApiResult.Success(InvoicePage(listOf(stamped("b"), stamped("c")), null), 200)
        val vm = InvoicesViewModel(api, { "mer_1" }, dispatcher)
        vm.load()
        assertTrue(vm.state.value.canLoadMore)
        vm.loadMore()
        assertEquals(listOf("a", "b", "c"), vm.state.value.invoices.map { it.invoiceId })
        assertNull(vm.state.value.nextCursor)
        assertFalse(vm.state.value.canLoadMore)
    }

    private fun detail(role: String = "owner", store: InvoiceFileStore = InvoiceFileStore { id, kind, _ -> SharedFile("content://x/$id.${kind.extension}", kind.mimeType) }) =
        InvoiceDetailViewModel("inv_1", api, { "mer_1" }, { role }, store, dispatcher).also { it.load() }

    @Test
    fun shareDownloadsSavesAndZeroesBuffer() {
        var saved: ByteArray? = null
        val vm = detail(store = { id, kind, bytes -> saved = bytes.copyOf(); SharedFile("content://x/$id", kind.mimeType) })
        vm.share(InvoiceFileKind.PDF)
        assertEquals(SharedFile("content://x/inv_1", "application/pdf"), vm.state.value.share)
        assertEquals(listOf<Byte>(1, 2, 3), saved!!.toList())
        assertTrue(api.lastFile!!.all { it == 0.toByte() }, "download buffer zeroed after writing it")
        vm.consumeShare()
        assertNull(vm.state.value.share)

        api.file = ApiResult.Error(404, "INVOICE_NOT_FOUND", "")
        vm.share(InvoiceFileKind.XML)
        assertTrue(vm.state.value.downloadFailed)
    }

    @Test
    fun ownerCancelsWithMotivoAndFolioRule() {
        val vm = detail()
        assertTrue(vm.state.value.canCancel)
        vm.onMotivo(CancelMotivo.WITH_RELATION)
        vm.cancel()
        assertEquals(CancelError.FOLIO_REQUIRED, vm.state.value.cancelError)
        assertTrue(api.cancels.isEmpty())
        vm.onFolio("6f1b2c3d-0000-4a5b-8c9d-112233445599")
        vm.cancel()
        assertEquals("01", api.cancels.single().motivo)
        assertEquals("6F1B2C3D-0000-4A5B-8C9D-112233445599", api.cancels.single().folioSustitucion)
        assertEquals(InvoiceStatus.CANCEL_REQUESTED, vm.state.value.invoice?.status)
        assertFalse(vm.state.value.canCancel)
    }

    @Test
    fun motivo02SendsNoFolioAndCashierCannotCancel() {
        val vm = detail()
        vm.onFolio("ignored")
        vm.onMotivo("99")
        assertEquals(CancelMotivo.WITHOUT_RELATION, vm.state.value.motivo)
        vm.cancel()
        assertNull(api.cancels.single().folioSustitucion)

        val cashier = detail(role = "cashier")
        assertFalse(cashier.state.value.canCancel)
        cashier.cancel()
        assertEquals(1, api.cancels.size)
    }

    @Test
    fun motivosFollowInvoiceKind() {
        val individual = detail()
        individual.onMotivo(CancelMotivo.NOMINATIVE_GLOBAL)
        assertEquals(CancelMotivo.WITHOUT_RELATION, individual.state.value.motivo, "04 is only for a factura global")
        assertEquals(listOf("01", "02", "03"), individual.state.value.motivoOptions)

        api.invoice = ApiResult.Success(stamped().copy(kind = InvoiceKind.GLOBAL, globalPeriod = "M2026-10"), 200)
        val global = detail()
        assertEquals(listOf("02", "03", "04"), global.state.value.motivoOptions)
        global.onMotivo(CancelMotivo.WITH_RELATION)
        assertEquals(CancelMotivo.WITHOUT_RELATION, global.state.value.motivo, "01 needs an individual substitute")
        global.onMotivo(CancelMotivo.NOMINATIVE_GLOBAL)
        global.cancel()
        assertEquals("04", api.cancels.single().motivo)
    }

    @Test
    fun salesMultiSelectOnlyInvoiceable() {
        val sales = object : SalesApi {
            override fun listSales(merchantId: String, limit: Int, before: String?) = ApiResult.Success(
                SalesPage(listOf(Sale("s1", "card", "CAPTURED", 100), Sale("s2", "card", "DECLINED", 100), Sale("s3", "spei", "RECEIVED", 100))),
                200
            )
        }
        val vm = SalesViewModel(sales, { "mer_1" }, dispatcher)
        vm.load()
        vm.toggle("s1")
        assertNull(vm.state.value.selection, "not selecting yet")
        vm.startSelection()
        vm.toggle("s1"); vm.toggle("s2"); vm.toggle("s3")
        assertEquals(setOf("s1", "s3"), vm.state.value.selection)
        vm.toggle("s1")
        assertEquals(setOf("s3"), vm.state.value.selection)
        vm.load()
        assertEquals(setOf("s3"), vm.state.value.selection, "reload keeps the selection")
        vm.cancelSelection()
        assertFalse(vm.state.value.isSelecting)
    }
}
