package mx.softpos.app.ui.codi

import com.google.zxing.BarcodeFormat
import com.google.zxing.EncodeHintType
import com.google.zxing.qrcode.QRCodeWriter
import com.google.zxing.qrcode.decoder.ErrorCorrectionLevel

data class CodiQrMatrix(
    val width: Int,
    val height: Int,
    val pixels: BooleanArray,
    val payloadJson: String,
    val expiresAtEpochSec: Long
) {
    fun isExpired(nowEpochSec: Long): Boolean = nowEpochSec >= expiresAtEpochSec

    fun getModule(x: Int, y: Int): Boolean {
        require(x in 0 until width && y in 0 until height) {
            "QR coordinates ($x, $y) out of bounds (${width}x$height)"
        }
        return pixels[y * width + x]
    }

    override fun equals(other: Any?): Boolean {
        if (this === other) return true
        if (other !is CodiQrMatrix) return false
        return width == other.width &&
            height == other.height &&
            pixels.contentEquals(other.pixels) &&
            payloadJson == other.payloadJson &&
            expiresAtEpochSec == other.expiresAtEpochSec
    }

    override fun hashCode(): Int {
        var result = width
        result = 31 * result + height
        result = 31 * result + pixels.contentHashCode()
        result = 31 * result + payloadJson.hashCode()
        result = 31 * result + expiresAtEpochSec.hashCode()
        return result
    }
}

object CodiQrGenerator {
    private const val MIN_QR_SIZE_PX = 64

    fun generateQr(
        payloadJson: String,
        sizePx: Int = 256,
        expiresAtEpochSec: Long
    ): CodiQrMatrix {
        require(payloadJson.isNotBlank()) { "CoDi QR payloadJson must not be blank" }
        require(sizePx >= MIN_QR_SIZE_PX) { "CoDi QR sizePx must be at least $MIN_QR_SIZE_PX" }
        require(expiresAtEpochSec > 0L) { "expiresAtEpochSec must be positive" }

        val hints = mapOf(
            EncodeHintType.ERROR_CORRECTION to ErrorCorrectionLevel.M,
            EncodeHintType.CHARACTER_SET to "UTF-8",
            EncodeHintType.MARGIN to 1
        )

        val bitMatrix = QRCodeWriter().encode(
            payloadJson,
            BarcodeFormat.QR_CODE,
            sizePx,
            sizePx,
            hints
        )

        val w = bitMatrix.width
        val h = bitMatrix.height
        val pixels = BooleanArray(w * h)
        for (y in 0 until h) {
            val rowOffset = y * w
            for (x in 0 until w) {
                pixels[rowOffset + x] = bitMatrix.get(x, y)
            }
        }

        return CodiQrMatrix(
            width = w,
            height = h,
            pixels = pixels,
            payloadJson = payloadJson,
            expiresAtEpochSec = expiresAtEpochSec
        )
    }
}
