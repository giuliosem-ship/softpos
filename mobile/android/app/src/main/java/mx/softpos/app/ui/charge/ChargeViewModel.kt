package mx.softpos.app.ui.charge

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.core.network.isUnknownOutcome
import mx.softpos.app.merchant.MerchantApi
import mx.softpos.app.merchant.MerchantProfile
import mx.softpos.app.payments.CardPayment
import mx.softpos.app.payments.CardPaymentIntent
import mx.softpos.app.payments.CardPaymentStatus
import mx.softpos.app.payments.CardPaymentsApi
import mx.softpos.app.payments.TapToPayOutcome
import mx.softpos.app.payments.TapToPayProvider
import mx.softpos.app.ui.codi.CodiQrGenerator
import mx.softpos.app.ui.codi.CodiQrMatrix
import mx.softpos.app.ui.merchant.MerchantIdProvider
import mx.softpos.app.ui.merchant.isKybBlocked
import mx.softpos.app.ui.pos.PaymentMethod
import mx.softpos.app.ui.pos.PosTerminalViewModel
import java.security.SecureRandom
import java.util.UUID

enum class CardStage { CREATING_INTENT, WAITING_FOR_CARD, CONFIRMING }

enum class ChargeError {
    NO_AMOUNT, NO_MERCHANT, TAP_UNAVAILABLE, NO_CLABE, DECLINED, CANCELLED, NETWORK,
    ACQUIRER_UNAVAILABLE, CARD_NOT_CONFIGURED, IDEMPOTENCY_CONFLICT, NOT_APPROVED, TAP_FAILED,
    NOT_AVAILABLE, UNKNOWN
}

sealed interface ChargeStep {
    data object Entering : ChargeStep
    data class ProcessingCard(val stage: CardStage) : ChargeStep
    data class CardApproved(val paymentId: String, val amountCents: Long) : ChargeStep
    /** Tap done but the backend has no final status yet (webhook pending / poll timed out). */
    data class CardPending(val paymentId: String, val amountCents: Long) : ChargeStep
    data object LoadingSpei : ChargeStep
    data class SpeiQr(
        val qr: CodiQrMatrix,
        val clabe: String,
        val beneficiary: String,
        val reference: String,
        val amountCents: Long
    ) : ChargeStep
}

data class ChargeUiState(
    val amountCents: Long = 0L,
    val step: ChargeStep = ChargeStep.Entering,
    val error: ChargeError? = null,
    /** Processor decline code (e.g. `do_not_honor`) when [error] is DECLINED. */
    val declineCode: String? = null,
    val tapAvailable: Boolean = true
)

/**
 * Status polling after the tap: exponential backoff in integer milliseconds
 * ([initialDelayMs] × 3/2 per attempt, capped at [maxDelayMs]) until the
 * payment is `final` or [timeoutMs] elapsed.
 */
data class PollPolicy(
    val initialDelayMs: Long = 500L,
    val maxDelayMs: Long = 3_000L,
    val timeoutMs: Long = 60_000L
) {
    fun next(delayMs: Long): Long = (delayMs * 3L / 2L).coerceAtMost(maxDelayMs)
}

/** SPEI QR payload (Kerno format, not CoDi/DiMo): CLABE + amount + reference. */
object SpeiQrPayload {
    fun build(clabe: String, beneficiary: String, amountCents: Long, reference: String): String =
        buildJsonObject {
            put("v", 1)
            put("type", "SPEI")
            put("clabe", clabe)
            put("beneficiary", beneficiary)
            put("amount_cents", amountCents)
            put("reference", reference)
        }.toString()

    /** SPEI "referencia numérica": up to 7 digits; 1..9_999_999. */
    fun newReference(random: SecureRandom = SecureRandom()): String =
        (1 + random.nextInt(9_999_999)).toString().padStart(7, '0')
}

/**
 * Card flow (docs/ACQUIRER_INTEGRATION.md §2): intent with a per-attempt
 * `Idempotency-Key` → processor SDK with `provider_payment_id` +
 * `client_secret` → poll `GET /card-payments/{id}` until `final`. The key is
 * kept while the outcome of the intent call is unknown (network error, any
 * 5xx but `ACQUIRER_REJECTED` / `ACQUIRER_NOT_CONFIGURED`), so the retry
 * returns the same intent instead of a second one; it is dropped once the
 * attempt is settled one way or another.
 */
class ChargeViewModel(
    private val cardApi: CardPaymentsApi,
    private val tapToPay: TapToPayProvider,
    private val merchantApi: MerchantApi,
    private val merchantId: MerchantIdProvider,
    private val io: CoroutineDispatcher = Dispatchers.IO,
    private val newIdempotencyKey: () -> String = { UUID.randomUUID().toString() },
    private val newReference: () -> String = { SpeiQrPayload.newReference() },
    private val nowEpochSec: () -> Long = { System.currentTimeMillis() / 1000L },
    private val poll: PollPolicy = PollPolicy(),
    /** Monotonic clock for the poll timeout (requests count, not only the waits). */
    private val elapsedMs: () -> Long = { System.nanoTime() / 1_000_000L }
) : ViewModel() {
    /** Keypad + money preview logic reused from the pure-logic module. */
    private val pos = PosTerminalViewModel()

    private val _state = MutableStateFlow(ChargeUiState(tapAvailable = tapToPay.isAvailable))
    val state: StateFlow<ChargeUiState> = _state.asStateFlow()

    /** Stable per charge attempt so a retry never creates a second intent. */
    private var attemptKey: String? = null
    private var cachedProfile: MerchantProfile? = null
    private var cardJob: Job? = null
    private var activePaymentId: String? = null

    fun onDigit(digit: Int) = editAmount { pos.onDigit(digit) }
    fun onBackspace() = editAmount { pos.onBackspace() }
    fun onClear() = editAmount { pos.onClear() }

    private fun editAmount(action: () -> Unit) {
        if (_state.value.step != ChargeStep.Entering) return
        action()
        attemptKey = null
        _state.update { it.copy(amountCents = pos.uiState.value.amountCents, error = null, declineCode = null) }
    }

    fun chargeWithCard() {
        val amount = _state.value.amountCents
        val id = merchantId()
        when {
            amount <= 0L -> return fail(ChargeError.NO_AMOUNT)
            id == null -> return fail(ChargeError.NO_MERCHANT)
            !tapToPay.isAvailable -> return fail(ChargeError.TAP_UNAVAILABLE)
            _state.value.step != ChargeStep.Entering -> return
        }
        pos.setPaymentMethod(PaymentMethod.CARD_NFC)
        val key = attemptKey ?: newIdempotencyKey().also { attemptKey = it }
        setStep(ChargeStep.ProcessingCard(CardStage.CREATING_INTENT))

        cardJob = viewModelScope.launch {
            val intent = when (val r = withContext(io) { cardApi.createIntent(key, amount) }) {
                is ApiResult.Success -> r.data
                is ApiResult.Error -> {
                    if (!r.isUnknownOutcome()) attemptKey = null
                    return@launch backToEntering(r.toChargeError())
                }
            }
            activePaymentId = intent.cardPaymentId
            // A replay of an intent that was already tapped (any status but CREATED)
            // has no client_secret: no new SDK session, and never cancel it
            // (cancel would void an AUTHORIZED payment); just read the outcome.
            if (intent.status == CardPaymentStatus.CREATED && intent.clientSecret.isNotBlank()) {
                setStep(ChargeStep.ProcessingCard(CardStage.WAITING_FOR_CARD))
                when (val outcome = tapToPay.collect(intent)) {
                    TapToPayOutcome.Approved, is TapToPayOutcome.Declined -> Unit
                    TapToPayOutcome.Cancelled -> return@launch abandon(intent, ChargeError.CANCELLED)
                    is TapToPayOutcome.Failed -> return@launch abandon(intent, ChargeError.TAP_FAILED)
                }
            }
            setStep(ChargeStep.ProcessingCard(CardStage.CONFIRMING))
            val payment = pollUntilFinal(intent.cardPaymentId)
            attemptKey = null
            activePaymentId = null
            when {
                payment == null || !payment.final -> setStep(ChargeStep.CardPending(intent.cardPaymentId, amount))
                CardPaymentStatus.isCaptured(payment.status) -> setStep(ChargeStep.CardApproved(intent.cardPaymentId, amount))
                payment.status == CardPaymentStatus.DECLINED ->
                    _state.update {
                        it.copy(step = ChargeStep.Entering, error = ChargeError.DECLINED, declineCode = payment.declineCode.ifBlank { null })
                    }
                payment.status == CardPaymentStatus.CANCELED -> backToEntering(ChargeError.CANCELLED)
                else -> setStep(ChargeStep.CardPending(intent.cardPaymentId, amount))
            }
        }
    }

    /** Merchant gives up while waiting for the card: stop the SDK and cancel the intent. */
    fun cancelCardCharge() {
        val step = _state.value.step as? ChargeStep.ProcessingCard ?: return
        // Only while the SDK waits for the card: before, the intent id is unknown;
        // after, the processor already has the tap.
        if (step.stage != CardStage.WAITING_FOR_CARD) return
        cardJob?.cancel()
        cardJob = null
        val paymentId = activePaymentId
        activePaymentId = null
        attemptKey = null
        backToEntering(ChargeError.CANCELLED)
        if (paymentId != null) viewModelScope.launch { withContext(io) { cardApi.cancel(paymentId) } }
    }

    private suspend fun abandon(intent: CardPaymentIntent, error: ChargeError) {
        attemptKey = null
        activePaymentId = null
        withContext(io) { cardApi.cancel(intent.cardPaymentId) } // best effort; webhook moves the status
        backToEntering(error)
    }

    /** Last seen status, or null if nothing could be read before the timeout. */
    private suspend fun pollUntilFinal(paymentId: String): CardPayment? {
        var waited = 0L
        var wait = poll.initialDelayMs
        var last: CardPayment? = null
        val start = elapsedMs()
        while (true) {
            when (val r = withContext(io) { cardApi.getPayment(paymentId) }) {
                is ApiResult.Success -> {
                    last = r.data
                    if (r.data.final) return r.data
                }
                is ApiResult.Error -> if (r.statusCode == 404) return last
            }
            // Waits alone under-count: each GET can take up to the HTTP timeout.
            if (maxOf(waited, elapsedMs() - start) >= poll.timeoutMs) return last
            delay(wait)
            waited += wait
            wait = poll.next(wait)
        }
    }

    fun chargeWithSpei() {
        val amount = _state.value.amountCents
        val id = merchantId()
        when {
            amount <= 0L -> return fail(ChargeError.NO_AMOUNT)
            id == null -> return fail(ChargeError.NO_MERCHANT)
            _state.value.step != ChargeStep.Entering -> return
        }
        pos.setPaymentMethod(PaymentMethod.CODI_QR)
        setStep(ChargeStep.LoadingSpei)
        viewModelScope.launch {
            val profile = cachedProfile ?: when (val r = withContext(io) { merchantApi.getMerchant(id!!) }) {
                is ApiResult.Success -> r.data.also { cachedProfile = it }
                is ApiResult.Error -> return@launch backToEntering(r.toChargeError())
            }
            if (profile.virtualClabe.isBlank()) return@launch backToEntering(ChargeError.NO_CLABE)
            val reference = newReference()
            val payload = SpeiQrPayload.build(profile.virtualClabe, profile.name, amount, reference)
            val qr = withContext(io) {
                CodiQrGenerator.generateQr(payload, QR_SIZE_PX, nowEpochSec() + QR_TTL_SEC)
            }
            setStep(ChargeStep.SpeiQr(qr, profile.virtualClabe, profile.name, reference, amount))
        }
    }

    /** Back to an empty keypad for the next sale. */
    fun newCharge() {
        attemptKey = null
        pos.onClear()
        _state.value = ChargeUiState(tapAvailable = tapToPay.isAvailable)
    }

    /** Leave a QR / result without clearing the amount (e.g. switch method). */
    fun backToAmount() = backToEntering(null)

    private fun setStep(step: ChargeStep) = _state.update { it.copy(step = step, error = null, declineCode = null) }
    private fun fail(error: ChargeError) = _state.update { it.copy(error = error) }
    private fun backToEntering(error: ChargeError?) =
        _state.update { it.copy(step = ChargeStep.Entering, error = error, declineCode = null) }

    private fun ApiResult.Error.toChargeError(): ChargeError = when {
        statusCode == NETWORK_ERROR_STATUS -> ChargeError.NETWORK
        errorCode == "ACQUIRER_UNAVAILABLE" -> ChargeError.ACQUIRER_UNAVAILABLE
        errorCode == "ACQUIRER_NOT_CONFIGURED" -> ChargeError.CARD_NOT_CONFIGURED
        errorCode == "IDEMPOTENCY_KEY_REUSED" -> ChargeError.IDEMPOTENCY_CONFLICT
        statusCode == 403 && errorCode == "MERCHANT_SCOPE_REQUIRED" -> ChargeError.NO_MERCHANT
        isKybBlocked() -> ChargeError.NOT_APPROVED
        statusCode == 404 || statusCode == 405 || statusCode == 501 -> ChargeError.NOT_AVAILABLE
        else -> ChargeError.UNKNOWN
    }

    companion object {
        /** Minimum size: about one pixel per module; Compose scales it up. */
        const val QR_SIZE_PX = 64
        const val QR_TTL_SEC = 15 * 60L
    }
}
