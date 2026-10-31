package mx.softpos.app.nfc

sealed interface CvmResult {
    data object NoCvmRequired : CvmResult
    data object CdcvmVerified : CvmResult
    data object PinRequired : CvmResult
}

enum class CardholderDeviceType {
    PHYSICAL_CARD,
    DIGITAL_WALLET
}

object CvmRulesEvaluator {
    const val MEXICO_CVM_LIMIT_CENTS: Long = 100_000L // $1,000.00 MXN in integer cents

    /**
     * Evaluates CVM (Cardholder Verification Method) requirement per Mexican contactless rules:
     * - <= $1,000.00 MXN (100,000 cents): No CVM required regardless of device.
     * - > $1,000.00 MXN with digital wallet (CDCVM biometric verified on buyer device): Approved directly (no upper cap).
     * - > $1,000.00 MXN with physical plastic card: Requires PIN-on-Glass (PinRequired).
     */
    fun evaluate(
        amountCents: Long,
        deviceType: CardholderDeviceType,
        cdcvmSupportedAndPerformed: Boolean = (deviceType == CardholderDeviceType.DIGITAL_WALLET)
    ): CvmResult {
        require(amountCents > 0L) { "Amount in cents must be positive, got: $amountCents" }

        return when {
            amountCents <= MEXICO_CVM_LIMIT_CENTS -> CvmResult.NoCvmRequired
            deviceType == CardholderDeviceType.DIGITAL_WALLET && cdcvmSupportedAndPerformed -> CvmResult.CdcvmVerified
            else -> CvmResult.PinRequired
        }
    }
}
