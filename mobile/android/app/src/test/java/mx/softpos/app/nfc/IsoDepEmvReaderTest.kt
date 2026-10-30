package mx.softpos.app.nfc

import kotlinx.coroutines.test.runTest
import org.junit.jupiter.api.Assertions.assertArrayEquals
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class IsoDepEmvReaderTest {

    private fun hexToBytes(hex: String): ByteArray {
        val clean = hex.trim().uppercase()
        return ByteArray(clean.length / 2) { i ->
            clean.substring(i * 2, i * 2 + 2).toInt(16).toByte()
        }
    }

    @Test
    fun `SELECT_PPSE_APDU matches standard EMV 2PAY SYS DDF01 command`() {
        val expected = hexToBytes("00A404000E325041592E5359532E444446303100")
        assertArrayEquals(expected, IsoDepEmvReader.SELECT_PPSE_APDU)
    }

    @Test
    fun `collectContactlessPayment approves valid EMV response masks PAN and zeroizes raw buffer`() = runTest {
        val cardTlvHex = EmvTlvCodec.encode(
            linkedMapOf(
                "5A" to "4152313899999012",
                "9F26" to "AABBCCDDEEFF0011",
                "9F27" to "80"
            )
        )
        val rawResponseBuffer = hexToBytes(cardTlvHex + "9000")
        var sentCommand: ByteArray? = null

        val reader = IsoDepEmvReader { commandApdu ->
            sentCommand = commandApdu
            rawResponseBuffer
        }

        val result = reader.collectContactlessPayment(25_000L, "merch_mx_99")

        assertArrayEquals(IsoDepEmvReader.SELECT_PPSE_APDU, sentCommand)
        assertTrue(result is TapPaymentResult.Approved)
        val approved = result as TapPaymentResult.Approved
        assertEquals("emv_merch_mx_99_25000", approved.paymentId)
        assertEquals("415231******9012", approved.maskedPan)
        assertEquals("AABBCCDDEEFF0011", approved.cryptogramHex)

        val outboundTags = EmvTlvCodec.decode(approved.tlvPayloadHex)
        assertEquals("000000025000", outboundTags["9F02"])
        assertEquals("0484", outboundTags["5F2A"])
        assertEquals("AABBCCDDEEFF0011", outboundTags["9F26"])
        assertEquals("80", outboundTags["9F27"])

        // Verify sensitive buffer was zeroized in finally block
        assertTrue(rawResponseBuffer.all { it == 0.toByte() })
    }

    @Test
    fun `collectContactlessPayment declines on non-positive amount non-9000 status or missing tags and zeroizes buffer`() = runTest {
        val dummyReader = IsoDepEmvReader { ByteArray(0) }
        val invalidAmount = dummyReader.collectContactlessPayment(0L, "merch_1")
        assertEquals(TapPaymentResult.Declined("INVALID_AMOUNT", "Amount must be positive"), invalidAmount)

        // Non-9000 status word
        val errorApduBuffer = hexToBytes("6A82")
        val errorReader = IsoDepEmvReader { errorApduBuffer }
        val apduDeclined = errorReader.collectContactlessPayment(10_000L, "merch_1")
        assertEquals(TapPaymentResult.Declined("APDU_ERROR", "Card rejected APDU"), apduDeclined)
        assertTrue(errorApduBuffer.all { it == 0.toByte() })

        // Missing 9F26 tag
        val incompleteTlvHex = EmvTlvCodec.encode(mapOf("5A" to "4152313899999012"))
        val incompleteBuffer = hexToBytes(incompleteTlvHex + "9000")
        val incompleteReader = IsoDepEmvReader { incompleteBuffer }
        val missingTagDeclined = incompleteReader.collectContactlessPayment(10_000L, "merch_1")
        assertEquals(
            TapPaymentResult.Declined("MISSING_EMV_TAGS", "Required EMV tags 5A or 9F26 missing"),
            missingTagDeclined
        )
        assertTrue(incompleteBuffer.all { it == 0.toByte() })
    }
}
