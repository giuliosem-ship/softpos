package mx.softpos.app.payments

import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.executeJson
import mx.softpos.app.core.network.toJsonBody
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request
import kotlin.coroutines.cancellation.CancellationException

/** What the SDK reports on the phone; informative only, the backend status is the truth. */
sealed interface TapToPayOutcome {
    data object Approved : TapToPayOutcome
    data class Declined(val reason: String) : TapToPayOutcome
    data object Cancelled : TapToPayOutcome
    data class Failed(val reason: String) : TapToPayOutcome
}

/**
 * Seam for the processor's Tap to Pay on Android SDK (gap F9 "Integrar SDK
 * del procesador"). The SDK is started with the intent's
 * `provider_payment_id` + `client_secret`, reads the card and talks to the
 * processor; the backend learns the result from the processor webhook. The
 * app never sees PAN or cryptograms on this path.
 */
interface TapToPayProvider {
    /** False when the device cannot take card payments (no NFC, SDK missing). */
    val isAvailable: Boolean
    suspend fun collect(intent: CardPaymentIntent): TapToPayOutcome
}

/** `POST /api/v1/dev/card-payments/{id}/simulate` (backend DEV_MODE + mock acquirer only). */
interface DevCardSimulator {
    fun simulate(cardPaymentId: String, outcome: String, refundId: String? = null): ApiResult<Unit>

    companion object {
        const val TAP = "tap"
        const val CANCEL = "cancel"
        const val REFUND = "refund"
    }
}

class HttpDevCardSimulator(baseUrl: String, private val client: OkHttpClient) : DevCardSimulator {
    private val base = baseUrl.trimEnd('/').toHttpUrl()

    override fun simulate(cardPaymentId: String, outcome: String, refundId: String?): ApiResult<Unit> {
        val url = base.newBuilder()
            .addPathSegments("api/v1/dev/card-payments")
            .addPathSegment(cardPaymentId)
            .addPathSegment("simulate")
            .build()
        val body = buildJsonObject {
            put("outcome", outcome)
            if (refundId != null) put("refund_id", refundId)
        }.toString()
        return when (val r = client.executeJson<Unit>(Request.Builder().url(url).post(body.toJsonBody()).build(), null)) {
            is ApiResult.Success -> ApiResult.Success(Unit, r.statusCode)
            is ApiResult.Error -> r
        }
    }
}

/**
 * Debug-only provider until the processor SDK is integrated: after [delayMs]
 * it asks the DEV_MODE backend to play the processor webhooks for the tap
 * ([DevCardSimulator.TAP]), so the real intent → webhook → ledger → polling
 * path runs end to end. The backend mock acquirer decides the outcome by the
 * amount's last two digits (`51` declined, `52` processor timeout). A
 * cancelled collection simulates the processor's cancel event.
 */
class MockTapToPayProvider(
    private val simulator: DevCardSimulator,
    private val delayMs: Long = 1_500L,
    private val io: CoroutineDispatcher = Dispatchers.IO
) : TapToPayProvider {
    override val isAvailable: Boolean = true

    override suspend fun collect(intent: CardPaymentIntent): TapToPayOutcome {
        try {
            delay(delayMs)
        } catch (e: CancellationException) {
            withContext(NonCancellable + io) { simulator.simulate(intent.cardPaymentId, DevCardSimulator.CANCEL) }
            throw e
        }
        return when (val r = withContext(io) { simulator.simulate(intent.cardPaymentId, DevCardSimulator.TAP) }) {
            is ApiResult.Error -> TapToPayOutcome.Failed("DEV_SIMULATE_" + r.errorCode)
            is ApiResult.Success ->
                if (intent.amountCents % 100L == DECLINE_CENTS) TapToPayOutcome.Declined("do_not_honor")
                else TapToPayOutcome.Approved
        }
    }

    companion object {
        /** Mirrors the backend mock acquirer scenario (docs/ACQUIRER_INTEGRATION.md §1). */
        const val DECLINE_CENTS = 51L
    }
}

/** Release builds until a real SDK exists: card payments are switched off. */
object UnavailableTapToPayProvider : TapToPayProvider {
    override val isAvailable: Boolean = false
    override suspend fun collect(intent: CardPaymentIntent): TapToPayOutcome =
        TapToPayOutcome.Failed("TAP_TO_PAY_UNAVAILABLE")
}
