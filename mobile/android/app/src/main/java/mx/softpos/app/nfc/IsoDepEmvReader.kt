package mx.softpos.app.nfc

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import java.nio.ByteBuffer

fun interface IsoDepTransceiver {
    fun transceive(commandApdu: ByteArray): ByteArray
}

class IsoDepEmvReader(private val transceiver: IsoDepTransceiver) : TapPaymentProvider {

    companion object {
        private fun hexToBytes(hex: String): ByteArray {
            val clean = hex.trim().uppercase()
            return ByteArray(clean.length / 2) { i -> clean.substring(i * 2, i * 2 + 2).toInt(16).toByte() }
        }

        val SELECT_PPSE_APDU: ByteArray = hexToBytes("00A404000E325041592E5359532E444446303100")
    }

    override suspend fun collectContactlessPayment(amountCents: Long, merchantId: String): TapPaymentResult {
        if (amountCents <= 0L) {
            return TapPaymentResult.Declined("INVALID_AMOUNT", "Amount must be positive")
        }

        val responseBytes = transceiver.transceive(SELECT_PPSE_APDU)
        try {
            if (responseBytes.size < 2 || responseBytes[responseBytes.size - 2] != 0x90.toByte() || responseBytes[responseBytes.size - 1] != 0x00.toByte()) {
                return TapPaymentResult.Declined("APDU_ERROR", "Card rejected APDU")
            }

            val payloadBytes = responseBytes.sliceArray(0 until responseBytes.size - 2)
            val payloadHex = payloadBytes.joinToString("") { "%02X".format(it.toInt() and 0xFF) }
            val decoded = EmvTlvCodec.decode(payloadHex)

            val pan = decoded["5A"]
            val cryptogram = decoded["9F26"]

            if (pan.isNullOrBlank() || cryptogram.isNullOrBlank()) {
                return TapPaymentResult.Declined("MISSING_EMV_TAGS", "Required EMV tags 5A or 9F26 missing")
            }

            val maskedPan = EmvTlvCodec.maskPan(pan)

            val outboundMap = linkedMapOf(
                "9F02" to amountCents.toString().padStart(12, '0'),
                "5F2A" to "0484",
                "9F26" to cryptogram,
                "9F27" to (decoded["9F27"] ?: "80")
            )

            val outboundTlvHex = EmvTlvCodec.encode(outboundMap)

            return TapPaymentResult.Approved(
                paymentId = "emv_${merchantId}_${amountCents}",
                maskedPan = maskedPan,
                cryptogramHex = cryptogram,
                tlvPayloadHex = outboundTlvHex
            )
        } finally {
            EmvTlvCodec.zeroize(responseBytes)
        }
    }
}
