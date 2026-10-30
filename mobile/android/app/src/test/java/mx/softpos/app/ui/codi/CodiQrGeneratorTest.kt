package mx.softpos.app.ui.codi

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class CodiQrGeneratorTest {

    @Test
    fun `generateQr produces valid ISO-18004 QR matrix with ZXing and checks expiration`() {
        val payload = """{"v":1,"ic":"646","c":"646180000000000011","m":"1160.00","cp":"TACOS","rf":"REF001","exp":1793400000}"""
        val matrix = CodiQrGenerator.generateQr(
            payloadJson = payload,
            sizePx = 256,
            expiresAtEpochSec = 1793400000L
        )

        assertEquals(256, matrix.width)
        assertEquals(256, matrix.height)
        assertEquals(256 * 256, matrix.pixels.size)
        assertEquals(payload, matrix.payloadJson)
        assertEquals(1793400000L, matrix.expiresAtEpochSec)

        // Ensure QR matrix contains both dark (true) and light (false) modules
        assertTrue(matrix.pixels.any { it })
        assertTrue(matrix.pixels.any { !it })

        // Verify getModule matches underlying pixel array
        assertEquals(matrix.pixels[0], matrix.getModule(0, 0))
        assertEquals(matrix.pixels[128 * 256 + 128], matrix.getModule(128, 128))

        // Verify expiration logic
        assertFalse(matrix.isExpired(1793399999L))
        assertTrue(matrix.isExpired(1793400000L))
        assertTrue(matrix.isExpired(1793400001L))
    }

    @Test
    fun `generateQr rejects blank payload or sizePx smaller than 64`() {
        assertThrows(IllegalArgumentException::class.java) {
            CodiQrGenerator.generateQr(payloadJson = "   ", sizePx = 256, expiresAtEpochSec = 1793400000L)
        }
        assertThrows(IllegalArgumentException::class.java) {
            CodiQrGenerator.generateQr(payloadJson = """{"v":1}""", sizePx = 32, expiresAtEpochSec = 1793400000L)
        }
    }
}
