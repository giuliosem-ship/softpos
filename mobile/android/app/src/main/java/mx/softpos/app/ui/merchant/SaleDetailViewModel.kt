package mx.softpos.app.ui.merchant

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
import mx.softpos.app.core.money.MoneyParser
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.payments.CardPayment
import mx.softpos.app.payments.CardPaymentsApi
import mx.softpos.app.payments.CardRefund
import mx.softpos.app.payments.DevCardSimulator
import mx.softpos.app.payments.Sale
import java.util.UUID

enum class RefundError {
    INVALID_AMOUNT, EXCEEDS_REMAINING, NOT_ALLOWED, ACQUIRER_UNAVAILABLE, ACQUIRER_REJECTED,
    IDEMPOTENCY_CONFLICT, NETWORK, UNKNOWN
}

data class SaleDetailUiState(
    val sale: Sale? = null,
    val payment: CardPayment? = null,
    val isLoading: Boolean = false,
    val error: LoadError? = null,
    /** Only owners see the refund controls (the backend enforces it too). */
    val isOwner: Boolean = false,
    /** What the merchant typed for a partial refund, e.g. "50.25". */
    val refundInput: String = "",
    val isRefunding: Boolean = false,
    val refundError: RefundError? = null,
    /** Last accepted request; its money moves when the processor confirms. */
    val lastRefund: CardRefund? = null
) {
    val refundableCents: Long get() = payment?.refundableCents ?: 0L
    val canRefund: Boolean get() = isOwner && refundableCents > 0L
}

/**
 * Sale detail: reads the card payment (captured / refunded / chargeback
 * amounts, settlement) and lets the owner refund it in full or in part via
 * `POST /card-payments/{id}/refunds`. The `Idempotency-Key` is stable per
 * refund attempt and amount: kept while the outcome is unknown (network,
 * `502 ACQUIRER_UNAVAILABLE`) so a retry replays the same request, dropped
 * once the backend answered.
 */
class SaleDetailViewModel(
    sale: Sale?,
    private val cardApi: CardPaymentsApi,
    role: () -> String,
    /** Debug only: plays the processor's `payment.refunded` (DEV_MODE backend). */
    private val devSimulator: DevCardSimulator? = null,
    private val io: CoroutineDispatcher = Dispatchers.IO,
    private val newIdempotencyKey: () -> String = { UUID.randomUUID().toString() }
) : ViewModel() {
    private val _state = MutableStateFlow(SaleDetailUiState(sale = sale, isOwner = role() == ROLE_OWNER))
    val state: StateFlow<SaleDetailUiState> = _state.asStateFlow()

    private var refundKey: Pair<Long?, String>? = null

    fun load() {
        val id = _state.value.sale?.cardPaymentId ?: return
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            val r = withContext(io) { cardApi.getPayment(id) }
            _state.update {
                when (r) {
                    is ApiResult.Success -> it.copy(isLoading = false, payment = r.data)
                    is ApiResult.Error -> it.copy(isLoading = false, error = r.toLoadError())
                }
            }
        }
    }

    fun onRefundInput(value: String) =
        _state.update { it.copy(refundInput = value.filter { c -> c.isDigit() || c == '.' || c == ',' }.take(16), refundError = null) }

    /** Whole refundable remainder (`amount_cents` omitted). */
    fun refundAll() = refund(null)

    /** Partial refund of what was typed in [SaleDetailUiState.refundInput]. */
    fun refundPartial() {
        val cents = MoneyParser.parseMxnToCents(_state.value.refundInput)
        when {
            cents == null || cents <= 0L -> _state.update { it.copy(refundError = RefundError.INVALID_AMOUNT) }
            cents > _state.value.refundableCents -> _state.update { it.copy(refundError = RefundError.EXCEEDS_REMAINING) }
            else -> refund(cents)
        }
    }

    private fun refund(amountCents: Long?) {
        val s = _state.value
        val paymentId = s.payment?.cardPaymentId ?: return
        if (!s.canRefund || s.isRefunding) return
        val key = refundKey?.takeIf { it.first == amountCents }?.second
            ?: newIdempotencyKey().also { refundKey = amountCents to it }
        _state.update { it.copy(isRefunding = true, refundError = null) }
        viewModelScope.launch {
            when (val r = withContext(io) { cardApi.refund(paymentId, key, amountCents) }) {
                is ApiResult.Success -> {
                    refundKey = null
                    devSimulator?.let { sim -> withContext(io) { sim.simulate(paymentId, DevCardSimulator.REFUND, r.data.refundId) } }
                    val updated = (withContext(io) { cardApi.getPayment(paymentId) } as? ApiResult.Success)?.data
                    _state.update {
                        it.copy(isRefunding = false, lastRefund = r.data, refundInput = "", payment = updated ?: it.payment)
                    }
                }
                is ApiResult.Error -> {
                    val unknownOutcome = r.statusCode == NETWORK_ERROR_STATUS ||
                        (r.statusCode == 502 && r.errorCode == "ACQUIRER_UNAVAILABLE")
                    if (!unknownOutcome) refundKey = null
                    _state.update { it.copy(isRefunding = false, refundError = r.toRefundError()) }
                }
            }
        }
    }

    private fun ApiResult.Error.toRefundError(): RefundError = when {
        statusCode == NETWORK_ERROR_STATUS -> RefundError.NETWORK
        errorCode == "REFUND_EXCEEDS_REMAINING" -> RefundError.EXCEEDS_REMAINING
        errorCode == "ACQUIRER_UNAVAILABLE" -> RefundError.ACQUIRER_UNAVAILABLE
        errorCode == "ACQUIRER_REJECTED" -> RefundError.ACQUIRER_REJECTED
        errorCode == "IDEMPOTENCY_KEY_REUSED" -> RefundError.IDEMPOTENCY_CONFLICT
        statusCode == 403 -> RefundError.NOT_ALLOWED
        else -> RefundError.UNKNOWN
    }

    companion object {
        const val ROLE_OWNER = "owner"
    }
}
