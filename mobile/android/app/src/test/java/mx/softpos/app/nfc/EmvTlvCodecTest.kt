package mx.softpos.app.nfc

import org.junit.jupiter.api.Assertions.assertArrayEquals
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test

class EmvTlvCodecTest {

    @Test
    fun `encode and decode round-trips single-byte and multi-byte BER-TLV tags`() {
        val tags = linkedMapOf(
            "9F02" to "000000116000",
            "5F2A" to "0484",
            "82" to "1980",
            "9A" to "261030",
            "9C" to "00",
            "9F26" to "A1B2C3D4E5F60718",
            "9F27" to "80",
            "9F36" to "0042",
            "5F24" to "281231",
            "5A" to "4152313899999012"
        )

        val encoded = EmvTlvCodec.encode(tags)
        val decoded = EmvTlvCodec.decode(encoded)

        assertEquals(tags, decoded)
    }

    @Test
    fun `decode throws IllegalArgumentException on malformed or truncated hex`() {
        assertThrows(IllegalArgumentException::class.java) {
            EmvTlvCodec.decode("")
        }
        assertThrows(IllegalArgumentException::class.java) {
            EmvTlvCodec.decode("9F02060000") // Truncated value (claims 6 bytes, only 2 provided)
        }
        assertThrows(IllegalArgumentException::class.java) {
            EmvTlvCodec.decode("9F020") // Odd length hex
        }
        assertThrows(IllegalArgumentException::class.java) {
            EmvTlvCodec.decode("ZZZZ") // Non-hex characters
        }
    }

    @Test
    fun `maskPan keeps first 6 BIN and last 4 digits and masks middle digits`() {
        assertEquals("415231******9012", EmvTlvCodec.maskPan("4152313899999012"))
        assertEquals("541275*********1234", EmvTlvCodec.maskPan("5412751234567891234"))
    }

    @Test
    fun `maskPan throws IllegalArgumentException when PAN has fewer than 12 digits`() {
        assertThrows(IllegalArgumentException::class.java) {
            EmvTlvCodec.maskPan("4152319012")
        }
    }

    @Test
    fun `zeroize overwrites all bytes in sensitive buffer with zero`() {
        val sensitive = byteArrayOf(0x41, 0x52, 0x31, 0x38, 0x7F, -0x01)
        EmvTlvCodec.zeroize(sensitive)
        assertArrayEquals(ByteArray(6) { 0 }, sensitive)
    }
}
