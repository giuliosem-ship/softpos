package mx.softpos.app.nfc

import kotlinx.coroutines.runBlocking
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class WhiteLabelTapProviderTest {

    @Test
    fun `collectContactlessPayment rejects non-positive amount`() = runBlocking {
        val provider = WhiteLabelTapProvider("Kushki") { _, _ ->
            SdkTapReadOutcome(
                approved = true,
                instrument = ContactlessInstrument.PHYSICAL_CARD,
                pan = "4152310000009012",
                cryptogramHex = "AABBCCDDEEFF0011"
            )
        }
        val result = provider.collectContactlessPayment(0L, "merch_01")
        assertTrue(result is TapPaymentResult.Declined)
        assertEquals("INVALID_AMOUNT", (result as TapPaymentResult.Declined).reasonCode)
    }

    @Test
    fun `collectContactlessPayment enforces CDCVM for digital wallets`() = runBlocking {
        val unverifiedWalletProvider = WhiteLabelTapProvider("Stripe") { _, _ ->
            SdkTapReadOutcome(
                approved = true,
                instrument = ContactlessInstrument.DIGITAL_WALLET_CDCVM,
                pan = "5204160000001234",
                cryptogramHex = "1122334455667788",
                cdcvmVerified = false
            )
        }
        val declined = unverifiedWalletProvider.collectContactlessPayment(25000L, "merch_01")
        assertTrue(declined is TapPaymentResult.Declined)
        assertEquals("CDCVM_REQUIRED", (declined as TapPaymentResult.Declined).reasonCode)

        val verifiedWalletProvider = WhiteLabelTapProvider("Stripe") { _, _ ->
            SdkTapReadOutcome(
                approved = true,
                instrument = ContactlessInstrument.DIGITAL_WALLET_CDCVM,
                pan = "5204160000001234",
                cryptogramHex = "1122334455667788",
                cdcvmVerified = true
            )
        }
        val approved = verifiedWalletProvider.collectContactlessPayment(25000L, "merch_01")
        assertTrue(approved is TapPaymentResult.Approved)
        val ok = approved as TapPaymentResult.Approved
        assertEquals("stripe_merch_01_25000", ok.paymentId)
        assertEquals("520416******1234", ok.maskedPan)
    }

    @Test
    fun `collectContactlessPayment approves Carnet voucher and encodes EMV TLV`() = runBlocking {
        val carnetProvider = WhiteLabelTapProvider("Blumon") { _, _ ->
            SdkTapReadOutcome(
                approved = true,
                instrument = ContactlessInstrument.CARNET_VOUCHER,
                pan = "5063290000007788",
                cryptogramHex = "ABCDEF0123456789",
                cdcvmVerified = false
            )
        }
        val result = carnetProvider.collectContactlessPayment(15000L, "merch_mx")
        assertTrue(result is TapPaymentResult.Approved)
        val approved = result as TapPaymentResult.Approved
        assertEquals("blumon_merch_mx_15000", approved.paymentId)
        assertEquals("506329******7788", approved.maskedPan)
        assertEquals("ABCDEF0123456789", approved.cryptogramHex)

        val decoded = EmvTlvCodec.decode(approved.tlvPayloadHex)
        assertEquals("000000015000", decoded["9F02"])
        assertEquals("0484", decoded["5F2A"])
        assertEquals("ABCDEF0123456789", decoded["9F26"])
        assertEquals("80", decoded["9F27"])
    }

    @Test
    fun `collectContactlessPayment propagates SDK decline reason`() = runBlocking {
        val decliningProvider = WhiteLabelTapProvider("Kushki") { _, _ ->
            SdkTapReadOutcome(
                approved = false,
                instrument = ContactlessInstrument.PHYSICAL_CARD,
                pan = "4152310000009012",
                cryptogramHex = "",
                errorCode = "INSUFFICIENT_FUNDS"
            )
        }
        val result = decliningProvider.collectContactlessPayment(5000L, "merch_01")
        assertTrue(result is TapPaymentResult.Declined)
        val declined = result as TapPaymentResult.Declined
        assertEquals("INSUFFICIENT_FUNDS", declined.reasonCode)
        assertEquals("Declined by Kushki", declined.message)
    }
}
