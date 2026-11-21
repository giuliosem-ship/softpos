package mx.softpos.app.ui.fiscal

import android.content.Context
import android.net.Uri
import android.provider.OpenableColumns
import mx.softpos.app.fiscal.CsdInspector
import java.io.ByteArrayOutputStream
import java.io.IOException

/**
 * Reads a `.cer` / `.key` returned by the Storage Access Framework picker
 * (`OpenDocument`: no storage permission, read grant for that Uri only).
 * Nothing is copied to disk; intermediate buffers are zeroed.
 */
class CsdFileReader(private val context: Context) {
    /** Display name and bytes; bytes are null when unreadable or larger than [CsdInspector.MAX_FILE_BYTES]. */
    fun read(uri: Uri): Pair<String, ByteArray?> {
        val name = displayName(uri) ?: uri.lastPathSegment.orEmpty()
        return name to readBytes(uri)
    }

    private fun displayName(uri: Uri): String? = try {
        context.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { c ->
            if (c.moveToFirst()) c.getString(0) else null
        }
    } catch (_: SecurityException) {
        null
    }

    private fun readBytes(uri: Uri): ByteArray? {
        val out = object : ByteArrayOutputStream() {
            fun wipe() = buf.fill(0)
        }
        val chunk = ByteArray(4 * 1024)
        return try {
            context.contentResolver.openInputStream(uri)?.use { input ->
                while (true) {
                    val n = input.read(chunk)
                    if (n < 0) break
                    out.write(chunk, 0, n)
                    if (out.size() > CsdInspector.MAX_FILE_BYTES) return null
                }
            } ?: return null
            out.toByteArray()
        } catch (_: IOException) {
            null
        } catch (_: SecurityException) {
            null
        } finally {
            chunk.fill(0)
            out.wipe()
        }
    }
}
