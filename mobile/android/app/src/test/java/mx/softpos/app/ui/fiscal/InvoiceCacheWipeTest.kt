package mx.softpos.app.ui.fiscal

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.io.File

class InvoiceCacheWipeTest {
    @Test
    fun wipeRemovesOnlySharedInvoices(@TempDir cache: File) {
        val invoices = File(cache, CacheInvoiceFileStore.DIR).apply { mkdirs() }
        File(invoices, "CFDI_inv_1.pdf").writeText("%PDF")
        File(invoices, "CFDI_inv_1.xml").writeText("<cfdi/>")
        val other = File(cache, "kyb.jpg").apply { writeText("x") }

        wipeInvoiceCache(cache)

        assertEquals(0, invoices.listFiles()!!.size)
        assertTrue(other.exists())
        wipeInvoiceCache(File(cache, "missing")) // no directory: no-op
    }
}
