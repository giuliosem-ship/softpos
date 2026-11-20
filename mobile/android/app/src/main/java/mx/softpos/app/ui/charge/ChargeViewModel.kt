package mx.softpos.app.ui.charge

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
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
import mx.softpos.app.merchant.MerchantApi
import mx.softpos.app.merchant.MerchantProfile
import mx.softpos.app.payments.CardPaymentStatus
import mx.softpos.app.payments.CardPaymentsApi
import mx.softpos.app.payments.TapToPayOutcome
import mx.softpos.app.payments.TapToPayProvider
import mx.softpos.app.ui.codi.CodiQrGenerator
import mx.softpos.app.ui.codi.CodiQrMatrix
import mx.softpos.app.ui.merchant.MerchantIdProvider
import mx.softpos.app.ui.pos.PaymentMethod
import mx.softpos.app.ui.pos.PosTerminalViewModel
import java.security.SecureRandom
import java.util.UUID

enum class CardStage { CREATING_INTENT, WAITING_FOR_CARD, CONFIRMING }

enum class ChargeError {
    NO_AMOUNT, NO_MERCHANT, TAP_UNAVAILABLE, NO_CLABE, DECLINED, CANCELLED, NETWORK, NOT_AVAILABLE, UNKNOWN
}

sealed interface ChargeStep {
    data object Entering : ChargeStep
    data class ProcessingCard(val stage: CardStage) : ChargeStep
    data class CardApproved(val paymentId: String, val amountCents: Long) : ChargeStep
    /** Tap succeeded but the backend has not confirmed yet (webhook pending). */
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
    val tapAvailable: Boolean = true
)

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

class ChargeViewModel(
    private val cardApi: CardPaymentsApi,
    private val tapToPay: TapToPayProvider,
    private val merchantApi: MerchantApi,
    private val merchantId: MerchantIdProvider,
    private val io: CoroutineDispatcher = Dispatchers.IO,
    private val newIdempotencyKey: () -> String = { UUID.randomUUID().toString() },
    private val newReference: () -> String = { SpeiQrPayload.newReference() },
    private val nowEpochSec: () -> Long = { System.currentTimeMillis() / 1000L }
) : ViewModel() {
    /** Keypad + money preview logic reused from the pure-logic module. */
    private val pos = PosTerminalViewModel()

    private val _state = MutableStateFlow(ChargeUiState(tapAvailable = tapToPay.isAvailable))
    val state: StateFlow<ChargeUiState> = _state.asStateFlow()

    /** Stable per charge attempt so a retry never creates a second intent. */
    private var attemptKey: String? = null
    private var cachedProfile: MerchantProfile? = null

    fun onDigit(digit: Int) = editAmount { pos.onDigit(digit) }
    fun onBackspace() = editAmount { pos.onBackspace() }
    fun onClear() = editAmount { pos.onClear() }

    private fun editAmount(action: () -> Unit) {
        if (_state.value.step != ChargeStep.Entering) return
        action()
        attemptKey = null
        _state.update { it.copy(amountCents = pos.uiState.value.amountCents, error = null) }
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

        viewModelScope.launch {
            val intent = when (val r = withContext(io) { cardApi.createIntent(key, amount) }) {
                is ApiResult.Success -> r.data
                is ApiResult.Error -> return@launch backToEntering(r.toChargeError())
            }
            setStep(ChargeStep.ProcessingCard(CardStage.WAITING_FOR_CARD))
            when (val outcome = tapToPay.collect(intent, amount)) {
                is TapToPayOutcome.Approved -> {
                    setStep(ChargeStep.ProcessingCard(CardStage.CONFIRMING))
                    attemptKey = null
                    val status = (withContext(io) { cardApi.getPayment(intent.id) } as? ApiResult.Success)?.data?.status
                    setStep(
                        if (status != null && CardPaymentStatus.isApproved(status)) ChargeStep.CardApproved(intent.id, amount)
                        else ChargeStep.CardPending(intent.id, amount)
                    )
                }
                is TapToPayOutcome.Declined -> { attemptKey = null; backToEntering(ChargeError.DECLINED) }
                TapToPayOutcome.Cancelled -> { attemptKey = null; backToEntering(ChargeError.CANCELLED) }
                is TapToPayOutcome.Failed -> { attemptKey = null; backToEntering(ChargeError.UNKNOWN) }
            }
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

    private fun setStep(step: ChargeStep) = _state.update { it.copy(step = step, error = null) }
    private fun fail(error: ChargeError) = _state.update { it.copy(error = error) }
    private fun backToEntering(error: ChargeError?) = _state.update { it.copy(step = ChargeStep.Entering, error = error) }

    private fun ApiResult.Error.toChargeError(): ChargeError = when {
        statusCode == NETWORK_ERROR_STATUS -> ChargeError.NETWORK
        statusCode == 403 && errorCode == "MERCHANT_SCOPE_REQUIRED" -> ChargeError.NO_MERCHANT
        statusCode == 404 || statusCode == 405 || statusCode == 501 -> ChargeError.NOT_AVAILABLE
        else -> ChargeError.UNKNOWN
    }

    companion object {
        /** Minimum size: about one pixel per module; Compose scales it up. */
        const val QR_SIZE_PX = 64
        const val QR_TTL_SEC = 15 * 60L
    }
}
