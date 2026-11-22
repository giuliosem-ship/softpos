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
import mx.softpos.app.core.network.isUnknownOutcome
import mx.softpos.app.payments.CardPayment
import mx.softpos.app.payments.CardPaymentsApi
import mx.softpos.app.payments.CardRefund
import mx.softpos.app.payments.DevCardSimulator
import mx.softpos.app.payments.Sale
import mx.softpos.app.payments.SalesApi
import mx.softpos.app.payments.SendReceiptRequest
import mx.softpos.app.payments.invalidReceiptFields
import mx.softpos.app.payments.receiptSendResult
import mx.softpos.app.ui.login.PhoneNumbers
import java.util.UUID

enum class RefundError {
    INVALID_AMOUNT, EXCEEDS_REMAINING, NOT_ALLOWED, ACQUIRER_UNAVAILABLE, ACQUIRER_REJECTED,
    IDEMPOTENCY_CONFLICT, NETWORK, UNKNOWN
}

enum class ReceiptError {
    NO_DESTINATION, INVALID_EMAIL, INVALID_PHONE,
    /** `409 SALE_NOT_CONFIRMED`: no money collected yet. */
    NOT_CONFIRMED,
    NOT_ALLOWED,
    /** `503 RECEIPTS_NOT_CONFIGURED` (backend without `RECEIPT_TOKEN_SECRET`). */
    NOT_CONFIGURED,
    /** `502`: a channel (email / SMS) failed; [SaleDetailUiState.receiptSent] says which. */
    SEND_FAILED,
    NETWORK, UNKNOWN
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
    val lastRefund: CardRefund? = null,
    val role: String = "",
    val receiptEmail: String = "",
    /** Mexican mobile as typed (10 digits); sent as E.164 `+52…`. */
    val receiptPhone: String = "",
    val isSendingReceipt: Boolean = false,
    val receiptError: ReceiptError? = null,
    /** Channels of the last send (`email`, `sms`) and whether each went out. */
    val receiptSent: Map<String, Boolean>? = null
) {
    val refundableCents: Long get() = payment?.refundableCents ?: 0L
    val canRefund: Boolean get() = isOwner && refundableCents > 0L
    /** Absolute link of the public receipt; null before the sale is confirmed or without receipts. */
    val receiptUrl: String? get() = sale?.receiptUrl?.takeIf { it.isNotBlank() }
    /** *Facturar*: invoiceable with the payment's current status (a refund may have changed it). */
    val canInvoice: Boolean get() = sale?.let { it.copy(status = payment?.status ?: it.status).isInvoiceable() } == true
    /** Owners and cashiers send receipts (`requireRole(owner, cashier)` in the backend). */
    val canSendReceipt: Boolean get() = receiptUrl != null && role in RECEIPT_ROLES

    private companion object {
        val RECEIPT_ROLES = setOf(SaleDetailViewModel.ROLE_OWNER, SaleDetailViewModel.ROLE_CASHIER)
    }
}

/**
 * Sale detail: reads the sale (`GET …/sales/{sale_id}`: card brand, last 4,
 * auth code, receipt link to share or send) and the card payment (captured /
 * refunded / chargeback amounts, settlement), and lets the owner refund it in full or in part via
 * `POST /card-payments/{id}/refunds`. The `Idempotency-Key` is stable per
 * refund attempt and amount: kept while the outcome is unknown (network, 5xx
 * other than `ACQUIRER_REJECTED`) so a retry replays the same request,
 * dropped once the backend answered definitively.
 */
class SaleDetailViewModel(
    private val saleId: String,
    /** The list's copy, shown while the detail loads (null after process death / deep link). */
    sale: Sale?,
    private val salesApi: SalesApi,
    private val merchantId: MerchantIdProvider,
    private val cardApi: CardPaymentsApi,
    role: () -> String,
    /** Debug only: plays the processor's `payment.refunded` (DEV_MODE backend). */
    private val devSimulator: DevCardSimulator? = null,
    private val io: CoroutineDispatcher = Dispatchers.IO,
    private val newIdempotencyKey: () -> String = { UUID.randomUUID().toString() }
) : ViewModel() {
    private val _state = MutableStateFlow(role().let { r -> SaleDetailUiState(sale = sale, isOwner = r == ROLE_OWNER, role = r) })
    val state: StateFlow<SaleDetailUiState> = _state.asStateFlow()

    private var refundKey: Pair<Long?, String>? = null

    fun load() {
        val merchant = merchantId()
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            if (merchant != null) {
                val d = withContext(io) { salesApi.getSale(merchant, saleId) }
                _state.update {
                    when (d) {
                        is ApiResult.Success -> it.copy(sale = d.data)
                        // The list's copy stays usable (refunds, invoicing); only the receipt is missing.
                        is ApiResult.Error -> if (it.sale == null) it.copy(error = d.toLoadError()) else it
                    }
                }
            }
            val id = _state.value.sale?.cardPaymentId
            if (id == null) {
                _state.update { it.copy(isLoading = false) }
                return@launch
            }
            val r = withContext(io) { cardApi.getPayment(id) }
            _state.update {
                when (r) {
                    is ApiResult.Success -> it.copy(isLoading = false, payment = r.data)
                    is ApiResult.Error -> it.copy(isLoading = false, error = r.toLoadError())
                }
            }
        }
    }

    fun onReceiptEmail(value: String) = _state.update { it.copy(receiptEmail = value.take(254), receiptError = null) }

    fun onReceiptPhone(value: String) =
        _state.update { it.copy(receiptPhone = value.filter { c -> c.isDigit() || c == '+' || c == ' ' }.take(16), receiptError = null) }

    /**
     * Sends the receipt link by email and/or SMS. No Idempotency-Key: the
     * backend just sends again, and the button is disabled while sending.
     */
    fun sendReceipt() {
        val merchant = merchantId() ?: return
        val s = _state.value
        if (!s.canSendReceipt || s.isSendingReceipt) return
        val email = s.receiptEmail.trim().ifBlank { null }
        val phoneInput = s.receiptPhone.trim().ifBlank { null }
        val phone = phoneInput?.let { PhoneNumbers.normalizeMx(it) }
        val error = when {
            email == null && phoneInput == null -> ReceiptError.NO_DESTINATION
            email != null && !EMAIL.matches(email) -> ReceiptError.INVALID_EMAIL
            phoneInput != null && phone == null -> ReceiptError.INVALID_PHONE
            else -> null
        }
        if (error != null) {
            _state.update { it.copy(receiptError = error) }
            return
        }
        _state.update { it.copy(isSendingReceipt = true, receiptError = null, receiptSent = null) }
        viewModelScope.launch {
            val r = withContext(io) { salesApi.sendReceipt(merchant, saleId, SendReceiptRequest(email = email, phone = phone)) }
            _state.update {
                when (r) {
                    is ApiResult.Success -> it.copy(isSendingReceipt = false, receiptSent = r.data.sent, receiptEmail = "", receiptPhone = "")
                    is ApiResult.Error -> it.copy(isSendingReceipt = false, receiptError = r.toReceiptError(), receiptSent = r.receiptSendResult()?.sent)
                }
            }
        }
    }

    private fun ApiResult.Error.toReceiptError(): ReceiptError = when {
        statusCode == NETWORK_ERROR_STATUS -> ReceiptError.NETWORK
        errorCode == "RECEIPT_DESTINATION_INVALID" ->
            if ("phone" in invalidReceiptFields() && "email" !in invalidReceiptFields()) ReceiptError.INVALID_PHONE else ReceiptError.INVALID_EMAIL
        errorCode == "SALE_NOT_CONFIRMED" -> ReceiptError.NOT_CONFIRMED
        statusCode == 403 -> ReceiptError.NOT_ALLOWED
        errorCode == "RECEIPTS_NOT_CONFIGURED" -> ReceiptError.NOT_CONFIGURED
        statusCode == 502 -> ReceiptError.SEND_FAILED
        else -> ReceiptError.UNKNOWN
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
                    // 500 CARD_REFUND_STORE_FAILED / 504 may follow a refund the processor
                    // accepted: a new key would send a second refund.
                    if (!r.isUnknownOutcome()) refundKey = null
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
        const val ROLE_CASHIER = "cashier"
        private val EMAIL = Regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]{2,}$")
    }
}
