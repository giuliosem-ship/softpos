package mx.softpos.app.ui.fiscal

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.w3c.dom.Element
import java.io.File
import javax.xml.parsers.DocumentBuilderFactory

/**
 * Static checks of the merged-source manifest and path files: every
 * FileProvider stays private (not exported, grants per Uri) and the invoice
 * one maps only `cache/invoices/`. Unit tests run with the module as working dir.
 */
class FileProviderHygieneTest {
    private val androidNs = "http://schemas.android.com/apk/res/android"

    private fun xml(path: String): Element {
        val factory = DocumentBuilderFactory.newInstance().apply { isNamespaceAware = true }
        return factory.newDocumentBuilder().parse(File(path)).documentElement
    }

    private fun Element.children(tag: String): List<Element> {
        val nodes = getElementsByTagName(tag)
        return (0 until nodes.length).map { nodes.item(it) as Element }
    }

    @Test
    fun providersAreNotExportedAndGrantPerUri() {
        val providers = xml("src/main/AndroidManifest.xml").children("provider")
        assertEquals(2, providers.size, "KYB camera + invoice share")
        providers.forEach {
            assertEquals("false", it.getAttributeNS(androidNs, "exported"))
            assertEquals("true", it.getAttributeNS(androidNs, "grantUriPermissions"))
        }
        val invoice = providers.single { it.getAttributeNS(androidNs, "name") == ".ui.fiscal.InvoiceFileProvider" }
        assertEquals("\${applicationId}" + CacheInvoiceFileStore.AUTHORITY_SUFFIX, invoice.getAttributeNS(androidNs, "authorities"))
        assertEquals("@xml/invoice_file_paths", invoice.children("meta-data").single().getAttributeNS(androidNs, "resource"))
    }

    @Test
    fun invoicePathsExposeOnlyTheInvoiceCacheSubdirectory() {
        val paths = xml("src/main/res/xml/invoice_file_paths.xml")
        val entries = (0 until paths.childNodes.length).map { paths.childNodes.item(it) }.filterIsInstance<Element>()
        assertEquals(1, entries.size)
        val only = entries.single()
        assertEquals("cache-path", only.tagName)
        assertEquals(CacheInvoiceFileStore.DIR + "/", only.getAttribute("path"))
        assertTrue(only.getAttribute("path").isNotBlank() && only.getAttribute("path") != "/" && only.getAttribute("path") != ".")
    }
}
