package mx.softpos.app.core.security

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test

class PinBlockFormatterTest {

    private fun bytesToHex(bytes: ByteArray): String {
        val sb = StringBuilder(bytes.size * 2)
        for (b in bytes) {
            sb.append(String.format("%02X", b))
        }
        return sb.toString()
    }

    @Test
    fun `formatIso0 produces standard ISO 9564 Format 0 block with PAN XOR`() {
        val pin = "1234"
        val pan = "4152313899999012"

        // Plaintext PIN field: 04 12 34 FF FF FF FF FF
        // PAN field (12 rightmost digits excluding check digit): 00 00 23 13 89 99 99 01
        // Expected XOR: 04 12 17 EC 76 66 66 FE
        val block = PinBlockFormatter.formatIso0(pin.toCharArray(), pan)

        assertEquals(8, block.size)
        assertEquals("041217EC766666FE", bytesToHex(block))
    }

    @Test
    fun `formatIso0 supports 6-digit PIN`() {
        val pin = "123456"
        val pan = "5412751234567890"

        // Plaintext PIN field: 06 12 34 56 FF FF FF FF
        // PAN 12 digits excluding last ('0'): 275123456789 -> 00 00 27 51 23 45 67 89
        // Expected XOR:
        // 06 ^ 00 = 06
        // 12 ^ 00 = 12
        // 34 ^ 27 = 13
        // 56 ^ 51 = 07
        // FF ^ 23 = DC
        // FF ^ 45 = BA
        // FF ^ 67 = 98
        // FF ^ 89 = 76
        // Result: 06 12 13 07 DC BA 98 76
        val block = PinBlockFormatter.formatIso0(pin.toCharArray(), pan)

        assertEquals("06121307DCBA9876", bytesToHex(block))
    }

    @Test
    fun `formatIso0 rejects PINs shorter than 4 or longer than 12 digits`() {
        assertThrows(IllegalArgumentException::class.java) {
            PinBlockFormatter.formatIso0("123".toCharArray(), "4152313899999012")
        }
        assertThrows(IllegalArgumentException::class.java) {
            PinBlockFormatter.formatIso0("1234567890123".toCharArray(), "4152313899999012")
        }
    }

    @Test
    fun `formatIso0 rejects invalid PANs shorter than 13 digits`() {
        assertThrows(IllegalArgumentException::class.java) {
            PinBlockFormatter.formatIso0("1234".toCharArray(), "4152313899")
        }
    }

    @Test
    fun `formatIso0 rejects non-digit PIN characters`() {
        assertThrows(IllegalArgumentException::class.java) {
            PinBlockFormatter.formatIso0("12a4".toCharArray(), "4152313899999012")
        }
    }
}
