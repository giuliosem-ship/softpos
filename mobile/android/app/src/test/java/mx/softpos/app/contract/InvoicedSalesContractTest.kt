package mx.softpos.app.contract

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.KernoJson
import mx.softpos.app.payments.FakeCardBackend
import mx.softpos.app.payments.HttpSalesApi
import mx.softpos.app.payments.SaleInvoiceMark
import mx.softpos.app.payments.invoiceMark
import mx.softpos.app.fiscal.HttpInvoiceApi
import mx.softpos.app.ui.fiscal.CancelError
import mx.softpos.app.ui.fiscal.InvoiceDetailViewModel
import mx.softpos.app.ui.merchant.SaleDetailViewModel
import mx.softpos.app.ui.merchant.SalesViewModel
import mx.softpos.app.ui.merchant.isInvoiceable
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
 * Invoiced sales, credit notes and 409 INVOICE_HAS_CREDIT_NOTES (AND-40)
 * against bodies captured from pkg/api by TestCaptureAndroidFixtures.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class InvoicedSalesContractTest {
    private val dispatcher = UnconfinedTestDispatcher()
    private lateinit var server: MockWebServer

    @BeforeEach fun setUp() {
        Dispatchers.setMain(dispatcher)
        server = MockWebServer().apply { start() }
    }

    @AfterEach fun tearDown() {
        server.shutdown()
        Dispatchers.resetMain()
    }

    private fun respond(code: Int, fixture: String) = server.enqueue(MockResponse().setResponseCode(code).setBody(Fixtures.read(fixture)))
    private fun sales() = HttpSalesApi(server.url("/").toString(), OkHttpClient())
    private fun invoices() = HttpInvoiceApi(server.url("/").toString(), OkHttpClient())

    @Test
    fun listMarksInvoicedSalesAndOnlyFreeOnesAreSelectable() {
        respond(200, "sales_invoiced_page.json")
        val items = (sales().listSales("m") as ApiResult.Success).data.items
        val free = items.single { it.invoiceId == null }
        val global = items.single { it.invoiceKind == "GLOBAL" }
        val card = items.single { it.invoiceKind == "INDIVIDUAL" }
        assertEquals(SaleInvoiceMark.NONE, free.invoiceMark())
        assertEquals(SaleInvoiceMark.IN_GLOBAL, global.invoiceMark())
        assertEquals(SaleInvoiceMark.INVOICED, card.invoiceMark())
        assertTrue(free.isInvoiceable())
        assertFalse(global.isInvoiceable(), "a sale in the factura global cannot be invoiced again")
        assertFalse(card.isInvoiceable(), "already invoiced (the partial refund alone would allow it)")
        val cn = card.creditNotes.single()
        assertEquals("STAMPED", cn.status)
        assertEquals(card.invoiceId, cn.relatedInvoiceId)
        assertTrue(cn.totalCents > 0L && cn.totalCents < card.amountCents)
        assertTrue(!cn.uuid.isNullOrBlank())
        assertTrue(free.creditNotes.isEmpty())

        respond(200, "sales_invoiced_page.json")
        val vm = SalesViewModel(sales(), { "m" }, dispatcher)
        vm.load()
        vm.startSelection()
        items.forEach { vm.toggle(it.id) }
        assertEquals(setOf(free.id), vm.state.value.selection)
    }

    @Test
    fun detailOfInvoicedSalesDisablesFacturarAndShowsCreditNotes() {
        val raw = KernoJson.parseToJsonElement(Fixtures.read("sale_detail_invoiced_credit_note_200.json")).jsonObject
        val id = raw.getValue("id").jsonPrimitive.content
        val cardBackend = FakeCardBackend().apply { seedCaptured(raw.getValue("card_payment_id").jsonPrimitive.content, 11_600, refunded = 1_160) }
        respond(200, "sale_detail_invoiced_credit_note_200.json")
        val vm = SaleDetailViewModel(id, null, sales(), { "m" }, cardBackend, { "owner" }, null, dispatcher)
        vm.load()
        val s = vm.state.value
        assertEquals(SaleInvoiceMark.INVOICED, s.sale?.invoiceMark())
        assertFalse(s.canInvoice)
        assertEquals(1, s.sale?.creditNotes?.size)

        respond(200, "sale_detail_in_global_200.json")
        val inGlobal = SaleDetailViewModel("spei", null, sales(), { "m" }, cardBackend, { "owner" }, null, dispatcher).also { it.load() }
        assertEquals(SaleInvoiceMark.IN_GLOBAL, inGlobal.state.value.sale?.invoiceMark())
        assertFalse(inGlobal.state.value.canInvoice)

        respond(200, "sale_detail_spei_200.json")
        val free = SaleDetailViewModel("spei", null, sales(), { "m" }, cardBackend, { "owner" }, null, dispatcher).also { it.load() }
        assertTrue(free.state.value.canInvoice)
    }

    @Test
    fun cancelWithLiveCreditNotesPointsToTheCreditNote() {
        val creditNoteId = KernoJson.parseToJsonElement(Fixtures.read("invoice_cancel_has_credit_notes_409.json"))
            .jsonObject.getValue("credit_note_id").jsonPrimitive.content
        respond(200, "invoice_get_200.json")
        val vm = InvoiceDetailViewModel("inv", invoices(), { "m" }, { "owner" }, { _, _, _ -> null }, dispatcher)
        vm.load()
        server.takeRequest()
        respond(409, "invoice_cancel_has_credit_notes_409.json")
        vm.cancel()
        server.takeRequest()
        assertEquals(CancelError.HAS_CREDIT_NOTES, vm.state.value.cancelError)
        assertEquals(creditNoteId, vm.state.value.blockingCreditNoteId)
        assertEquals(2, server.requestCount, "not a NOT_CANCELABLE: no reload")

        vm.onMotivo("03")
        assertNull(vm.state.value.blockingCreditNoteId, "cleared with the error")
    }
}
