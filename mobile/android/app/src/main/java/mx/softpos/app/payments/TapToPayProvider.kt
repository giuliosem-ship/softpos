package mx.softpos.app.payments

import kotlinx.coroutines.delay

sealed interface TapToPayOutcome {
    data class Approved(val providerPaymentId: String) : TapToPayOutcome
    data class Declined(val reason: String) : TapToPayOutcome
    data object Cancelled : TapToPayOutcome
    data class Failed(val reason: String) : TapToPayOutcome
}

/**
 * Seam for the processor's Tap to Pay on Android SDK (gap F3/F9 "Integrar SDK
 * del procesador"). The SDK reads the card and talks to the processor with
 * the intent's client secret; the backend learns the result from the
 * processor (webhook). The app never sees PAN or cryptograms on this path.
 */
interface TapToPayProvider {
    /** False when the device cannot take card payments (no NFC, SDK missing). */
    val isAvailable: Boolean
    suspend fun collect(intent: CardPaymentIntent, amountCents: Long): TapToPayOutcome
}

/**
 * Demo provider until the processor SDK is integrated. Approves after
 * [delayMs]; amounts ending in 13 centavos are declined so the decline path
 * can be exercised by hand. [onOutcome] lets the in-memory backend play the
 * processor webhook.
 */
class MockTapToPayProvider(
    private val delayMs: Long = 1_500L,
    private val onOutcome: (intentId: String, outcome: TapToPayOutcome) -> Unit = { _, _ -> }
) : TapToPayProvider {
    override val isAvailable: Boolean = true

    override suspend fun collect(intent: CardPaymentIntent, amountCents: Long): TapToPayOutcome {
        delay(delayMs)
        val outcome = if (amountCents % 100L == DECLINE_CENTS) {
            TapToPayOutcome.Declined("MOCK_DECLINED")
        } else {
            TapToPayOutcome.Approved("mock_${intent.id}")
        }
        onOutcome(intent.id, outcome)
        return outcome
    }

    companion object {
        const val DECLINE_CENTS = 13L
    }
}

/** Release builds until a real SDK exists: card payments are switched off. */
object UnavailableTapToPayProvider : TapToPayProvider {
    override val isAvailable: Boolean = false
    override suspend fun collect(intent: CardPaymentIntent, amountCents: Long): TapToPayOutcome =
        TapToPayOutcome.Failed("TAP_TO_PAY_UNAVAILABLE")
}
