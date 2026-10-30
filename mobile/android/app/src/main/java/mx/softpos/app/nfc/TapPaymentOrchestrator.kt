package mx.softpos.app.nfc

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

enum class TapStage {
    IDLE, READING_CARD, AUTHORIZING, SETTLING, COMPLETED, REVERSED, FAILED
}

data class TapSessionState(
    val stage: TapStage = TapStage.IDLE,
    val amountCents: Long = 0L,
    val paymentId: String? = null,
    val maskedPan: String? = null,
    val errorMessage: String? = null
)

interface EmvBackendGateway {
    suspend fun authorize(paymentId: String, amountCents: Long, tlvHex: String): Boolean
    suspend fun settle(paymentId: String): Boolean
    suspend fun reverse(paymentId: String): Boolean
}

class TapPaymentOrchestrator(
    private val tapProvider: TapPaymentProvider,
    private val gateway: EmvBackendGateway
) {
    private val _state = MutableStateFlow(TapSessionState())
    val state: StateFlow<TapSessionState> = _state.asStateFlow()

    fun reset() {
        _state.value = TapSessionState()
    }

    suspend fun processTap(amountCents: Long, merchantId: String): TapSessionState {
        if (amountCents <= 0L) {
            throw IllegalArgumentException("Amount in cents must be greater than zero")
        }

        _state.value = TapSessionState(stage = TapStage.READING_CARD, amountCents = amountCents)
        val result = tapProvider.collectContactlessPayment(amountCents, merchantId)

        return when (result) {
            is TapPaymentResult.Declined -> {
                _state.value = TapSessionState(
                    stage = TapStage.FAILED,
                    amountCents = amountCents,
                    errorMessage = result.message
                )
                _state.value
            }
            is TapPaymentResult.Approved -> {
                _state.value = TapSessionState(
                    stage = TapStage.AUTHORIZING,
                    amountCents = amountCents,
                    paymentId = result.paymentId,
                    maskedPan = result.maskedPan
                )
                if (!gateway.authorize(result.paymentId, amountCents, result.tlvPayloadHex)) {
                    _state.value = TapSessionState(
                        stage = TapStage.FAILED,
                        amountCents = amountCents,
                        paymentId = result.paymentId,
                        maskedPan = result.maskedPan,
                        errorMessage = "Authorization declined"
                    )
                    return _state.value
                }

                _state.value = TapSessionState(
                    stage = TapStage.SETTLING,
                    amountCents = amountCents,
                    paymentId = result.paymentId,
                    maskedPan = result.maskedPan
                )
                if (gateway.settle(result.paymentId)) {
                    _state.value = TapSessionState(
                        stage = TapStage.COMPLETED,
                        amountCents = amountCents,
                        paymentId = result.paymentId,
                        maskedPan = result.maskedPan
                    )
                } else {
                    gateway.reverse(result.paymentId)
                    _state.value = TapSessionState(
                        stage = TapStage.REVERSED,
                        amountCents = amountCents,
                        paymentId = result.paymentId,
                        maskedPan = result.maskedPan,
                        errorMessage = "Settlement failed; reversal executed"
                    )
                }
                _state.value
            }
        }
    }
}
