package mx.softpos.app.ui.fiscal

import android.content.Context
import androidx.core.content.FileProvider
import mx.softpos.app.R
import mx.softpos.app.fiscal.InvoiceFileKind
import java.io.File
import java.io.IOException

/**
 * Own FileProvider subclass (a manifest can declare `androidx.core.content.FileProvider`
 * only once; the KYB camera already uses it). Not exported; it only maps
 * `cache/invoices/` (res/xml/invoice_file_paths.xml) and grants are per Uri.
 */
class InvoiceFileProvider : FileProvider(R.xml.invoice_file_paths)

/**
 * Keeps at most the file being shared: the directory is emptied before each
 * write, so old CFDIs (customer data) do not pile up in the cache.
 */
class CacheInvoiceFileStore(private val context: Context) : InvoiceFileStore {
    override fun save(invoiceId: String, kind: InvoiceFileKind, bytes: ByteArray): SharedFile? = try {
        val dir = File(context.cacheDir, DIR).apply { mkdirs() }
        dir.listFiles()?.forEach { it.delete() }
        val safeId = invoiceId.filter { it.isLetterOrDigit() || it == '-' || it == '_' }.take(64).ifBlank { "factura" }
        val file = File(dir, "CFDI_$safeId.${kind.extension}")
        file.outputStream().use { it.write(bytes) }
        val uri = FileProvider.getUriForFile(context, context.packageName + AUTHORITY_SUFFIX, file)
        SharedFile(uri.toString(), kind.mimeType)
    } catch (_: IOException) {
        null
    } catch (_: IllegalArgumentException) {
        null
    }

    companion object {
        const val DIR = "invoices"
        const val AUTHORITY_SUFFIX = ".invoicefiles"
    }
}
