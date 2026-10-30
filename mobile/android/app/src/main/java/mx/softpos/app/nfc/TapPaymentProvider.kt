package mx.softpos.app.nfc

/**
 * Agnostic contract for Contactless NFC Tap-to-Phone payment collection.
 * Implementations can swap between:
 *  - Stage 1: Stripe Terminal Tap-to-Pay SDK
 *  - Stage 2: PayFac White-Label SDK (Kushki / Blumon)
 *  - Stage 3: Direct MPoC SDK (Phos / MineSec) or IsoDep APDU Lab Reader
 */
interface TapPaymentProvider {
    suspend fun collectContactlessPayment(amountCents: Long, merchantId: String): TapPaymentResult
}

sealed interface TapPaymentResult {
    data class Approved(
        val paymentId: String,
        val maskedPan: String,
        val cryptogramHex: String,
        val tlvPayloadHex: String
    ) : TapPaymentResult

    data class Declined(val reasonCode: String, val message: String) : TapPaymentResult
}
