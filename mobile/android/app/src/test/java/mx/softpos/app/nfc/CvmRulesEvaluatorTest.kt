package mx.softpos.app.nfc

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test

class CvmRulesEvaluatorTest {

    @Test
    fun `amounts up to 1000 MXN require no CVM for physical card`() {
        val result = CvmRulesEvaluator.evaluate(
            amountCents = 100_000L, // Exactly $1,000.00 MXN
            deviceType = CardholderDeviceType.PHYSICAL_CARD
        )
        assertEquals(CvmResult.NoCvmRequired, result)

        val smallResult = CvmRulesEvaluator.evaluate(
            amountCents = 45_000L, // $450.00 MXN
            deviceType = CardholderDeviceType.PHYSICAL_CARD
        )
        assertEquals(CvmResult.NoCvmRequired, smallResult)
    }

    @Test
    fun `amounts up to 1000 MXN require no CVM for digital wallet`() {
        val result = CvmRulesEvaluator.evaluate(
            amountCents = 100_000L,
            deviceType = CardholderDeviceType.DIGITAL_WALLET
        )
        assertEquals(CvmResult.NoCvmRequired, result)
    }

    @Test
    fun `amounts above 1000 MXN with physical card require PIN-on-Glass`() {
        val result = CvmRulesEvaluator.evaluate(
            amountCents = 100_001L, // $1,000.01 MXN
            deviceType = CardholderDeviceType.PHYSICAL_CARD
        )
        assertEquals(CvmResult.PinRequired, result)

        val largeResult = CvmRulesEvaluator.evaluate(
            amountCents = 500_000L, // $5,000.00 MXN
            deviceType = CardholderDeviceType.PHYSICAL_CARD
        )
        assertEquals(CvmResult.PinRequired, largeResult)
    }

    @Test
    fun `amounts above 1000 MXN with Apple Pay or Google Pay CDCVM pass without PIN`() {
        // High amount ($15,000 MXN) paid with biometric CDCVM on phone
        val result = CvmRulesEvaluator.evaluate(
            amountCents = 1_500_000L,
            deviceType = CardholderDeviceType.DIGITAL_WALLET,
            cdcvmSupportedAndPerformed = true
        )
        assertEquals(CvmResult.CdcvmVerified, result)
    }

    @Test
    fun `zero or negative amounts throw IllegalArgumentException`() {
        assertThrows(IllegalArgumentException::class.java) {
            CvmRulesEvaluator.evaluate(0L, CardholderDeviceType.PHYSICAL_CARD)
        }
        assertThrows(IllegalArgumentException::class.java) {
            CvmRulesEvaluator.evaluate(-500L, CardholderDeviceType.DIGITAL_WALLET)
        }
    }
}
