package mx.softpos.app.ui.onboarding

import android.content.Context
import android.graphics.Bitmap
import android.graphics.ImageDecoder
import android.net.Uri
import androidx.core.content.FileProvider
import mx.softpos.app.kyb.PickedDocument
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.IOException

/** Pure sizing math for document photos (unit-tested). */
object DocumentImageSizing {
    /** Longest edge kept: enough for INE text and a selfie, ~0.3–1 MB as JPEG. */
    const val MAX_EDGE_PX = 2048
    const val JPEG_QUALITY = 85

    /** Target size preserving aspect ratio; never upscales. */
    fun targetSize(width: Int, height: Int, maxEdge: Int = MAX_EDGE_PX): Pair<Int, Int> {
        require(width > 0 && height > 0) { "invalid size" }
        val longest = maxOf(width, height)
        if (longest <= maxEdge) return width to height
        // Integer math: round half up, at least 1 px.
        val w = ((width.toLong() * maxEdge + longest / 2) / longest).toInt().coerceAtLeast(1)
        val h = ((height.toLong() * maxEdge + longest / 2) / longest).toInt().coerceAtLeast(1)
        return w to h
    }
}

/** A [ByteArrayOutputStream] whose internal buffer can be zeroed after copying it out. */
private class WipeableOutputStream : ByteArrayOutputStream() {
    fun wipe() = buf.fill(0)
}

/**
 * Reads what the system photo picker, the camera intent or the document
 * picker returned. Photos are decoded with [ImageDecoder] (applies EXIF
 * rotation), downscaled to [DocumentImageSizing.MAX_EDGE_PX] and re-encoded
 * as JPEG, which also drops EXIF metadata such as GPS. PDFs pass through.
 * Intermediate buffers are zeroed; the camera temp file is overwritten and
 * deleted.
 */
class DocumentReader(private val context: Context) {
    private val resolver get() = context.contentResolver

    /** Fresh content Uri for `ActivityResultContracts.TakePicture` (app cache, FileProvider). */
    fun newCaptureUri(): Uri {
        val dir = File(context.cacheDir, CAPTURE_DIR).apply { mkdirs() }
        val file = File(dir, CAPTURE_FILE)
        return FileProvider.getUriForFile(context, context.packageName + AUTHORITY_SUFFIX, file)
    }

    /** Overwrites and deletes the camera temp file once its photo was read. */
    fun discardCapture() {
        val file = File(File(context.cacheDir, CAPTURE_DIR), CAPTURE_FILE)
        if (file.exists()) {
            try {
                file.outputStream().use { out -> out.write(ByteArray(file.length().toInt().coerceAtMost(MAX_BYTES))) }
            } catch (_: IOException) {
                // best effort; the file is deleted below anyway
            }
            file.delete()
        }
    }

    fun read(uri: Uri, baseName: String): PickedDocument? = try {
        if (resolver.getType(uri) == PDF) readPdf(uri, baseName) else readImage(uri, baseName)
    } catch (_: IOException) {
        null
    } catch (_: SecurityException) {
        null
    }

    private fun readImage(uri: Uri, baseName: String): PickedDocument? {
        val source = ImageDecoder.createSource(resolver, uri)
        val bitmap = ImageDecoder.decodeBitmap(source) { decoder, info, _ ->
            val (w, h) = DocumentImageSizing.targetSize(info.size.width, info.size.height)
            decoder.setTargetSize(w, h)
            decoder.allocator = ImageDecoder.ALLOCATOR_SOFTWARE
        }
        val out = WipeableOutputStream()
        return try {
            if (!bitmap.compress(Bitmap.CompressFormat.JPEG, DocumentImageSizing.JPEG_QUALITY, out)) return null
            PickedDocument(out.toByteArray(), JPEG, "$baseName.jpg")
        } finally {
            bitmap.eraseColor(0)
            bitmap.recycle()
            out.wipe()
        }
    }

    private fun readPdf(uri: Uri, baseName: String): PickedDocument? {
        val out = WipeableOutputStream()
        return try {
            resolver.openInputStream(uri)?.use { input ->
                val chunk = ByteArray(16 * 1024)
                while (true) {
                    val n = input.read(chunk)
                    if (n < 0) break
                    out.write(chunk, 0, n)
                    if (out.size() > MAX_BYTES) return null
                }
                chunk.fill(0)
            } ?: return null
            PickedDocument(out.toByteArray(), PDF, "$baseName.pdf")
        } finally {
            out.wipe()
        }
    }

    companion object {
        const val AUTHORITY_SUFFIX = ".kybfiles"
        private const val CAPTURE_DIR = "kyb"
        private const val CAPTURE_FILE = "capture.jpg"
        private const val JPEG = "image/jpeg"
        private const val PDF = "application/pdf"
        private const val MAX_BYTES = 10 * 1024 * 1024
    }
}
