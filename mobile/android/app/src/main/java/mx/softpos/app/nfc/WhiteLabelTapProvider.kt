package mx.softpos.app.nfc

import kotlinx.coroutines.flow.StateFlow
import java.util.*

enum class ContactlessInstrument {
    PHYSICAL_CARD,
    DIGITAL_WALLET_CDCVM,
    CARNET_VOUCHER
}

data class SdkTapReadOutcome(
    val approved: Boolean,
    val instrument: ContactlessInstrument,
    val pan: String,
    val cryptogramHex: String,
    val cdcvmVerified: Boolean = false,
    val errorCode: String? = null
)

fun interface ExternalTapSdkBridge {
    suspend fun invokeSdkTap(amountCents: Long, merchantId: String): SdkTapReadOutcome
}

class WhiteLabelTapProvider(
    private val providerName: String,
    private val bridge: ExternalTapSdkBridge
) : TapPaymentProvider {

    override suspend fun collectContactlessPayment(amountCents: Long, merchantId: String): TapPaymentResult {
        if (amountCents <= 0L) {
            return TapPaymentResult.Declined("INVALID_AMOUNT", "Amount must be positive")
        }

        val outcome = bridge.invokeSdkTap(amountCents, merchantId)

        if (!outcome.approved) {
            return TapPaymentResult.Declined(outcome.errorCode ?: "SDK_DECLINED", "Declined by $providerName")
        }

        if (outcome.instrument == ContactlessInstrument.DIGITAL_WALLET_CDCVM && !outcome.cdcvmVerified) {
            return TapPaymentResult.Declined("CDCVM_REQUIRED", "Biometric CDCVM verification required for digital wallet")
        }

        val maskedPan = EmvTlvCodec.maskPan(outcome.pan)
        val tlvHex = EmvTlvCodec.encode(linkedMapOf(
            "9F02" to amountCents.toString().padStart(12, '0'),
            "5F2A" to "0484",
            "9F26" to outcome.cryptogramHex,
            "9F27" to "80"
        ))

        return TapPaymentResult.Approved(
            paymentId = "${providerName.lowercase()}_${merchantId}_${amountCents}",
            maskedPan = maskedPan,
            cryptogramHex = outcome.cryptogramHex,
            tlvPayloadHex = tlvHex
        )
    }
}
