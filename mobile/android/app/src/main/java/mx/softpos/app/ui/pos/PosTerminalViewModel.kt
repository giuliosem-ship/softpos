package mx.softpos.app.ui.pos

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import mx.softpos.app.core.money.MoneySplitCalculator
import mx.softpos.app.core.money.SplitPreview

enum class PaymentMethod {
    CARD_NFC,
    CODI_QR
}

data class PosUiState(
    val rawDigits: String = "",
    val amountCents: Long = 0L,
    val paymentMethod: PaymentMethod = PaymentMethod.CARD_NFC,
    val isPersonaMoral: Boolean = false,
    val isrRateBps: Long = 100L,
    val rbfHoldbackBps: Long = 1000L,
    val cardFeeBps: Long = 290L,
    val splitPreview: SplitPreview? = null
)

class PosTerminalViewModel(
    initialIsrRateBps: Long = 100L,
    initialRbfHoldbackBps: Long = 1000L,
    cardFeeBps: Long = 290L
) {
    private val _uiState = MutableStateFlow(
        PosUiState(
            isrRateBps = initialIsrRateBps,
            rbfHoldbackBps = initialRbfHoldbackBps,
            cardFeeBps = cardFeeBps
        )
    )

    val uiState: StateFlow<PosUiState> = _uiState.asStateFlow()

    fun onDigit(digit: Int) {
        require(digit in 0..9) { "Digit must be between 0 and 9" }
        val currentState = uiState.value
        if (currentState.rawDigits.isEmpty() && digit == 0) return
        if (currentState.rawDigits.length >= 9) return

        val newRawDigits = currentState.rawDigits + digit
        val newAmountCents = newRawDigits.toLong()
        updateUiState {
            it.copy(
                rawDigits = newRawDigits,
                amountCents = newAmountCents,
                splitPreview = calculateSplitPreview(newAmountCents, it)
            )
        }
    }

    fun onBackspace() {
        val currentState = uiState.value
        if (currentState.rawDigits.isEmpty()) return

        val newRawDigits = currentState.rawDigits.dropLast(1)
        val newAmountCents = if (newRawDigits.isEmpty()) 0L else newRawDigits.toLong()
        updateUiState {
            it.copy(
                rawDigits = newRawDigits,
                amountCents = newAmountCents,
                splitPreview = calculateSplitPreview(newAmountCents, it)
            )
        }
    }

    fun onClear() {
        updateUiState {
            it.copy(
                rawDigits = "",
                amountCents = 0L,
                splitPreview = null
            )
        }
    }

    fun setPaymentMethod(method: PaymentMethod) {
        updateUiState { currentState ->
            val newSplitPreview = if (currentState.amountCents > 0L) {
                calculateSplitPreview(currentState.amountCents, currentState.copy(paymentMethod = method))
            } else {
                null
            }
            currentState.copy(
                paymentMethod = method,
                splitPreview = newSplitPreview
            )
        }
    }

    fun togglePersonaMoral() {
        updateUiState { currentState ->
            val newSplitPreview = if (currentState.amountCents > 0L) {
                calculateSplitPreview(currentState.amountCents, currentState.copy(isPersonaMoral = !currentState.isPersonaMoral))
            } else {
                null
            }
            currentState.copy(
                isPersonaMoral = !currentState.isPersonaMoral,
                splitPreview = newSplitPreview
            )
        }
    }

    private fun calculateSplitPreview(amountCents: Long, state: PosUiState): SplitPreview? {
        if (amountCents <= 0L) return null

        val effectiveFeeBps = if (state.paymentMethod == PaymentMethod.CODI_QR) 0L else state.cardFeeBps
        return MoneySplitCalculator.calculatePreview(
            grossCents = amountCents,
            platformFeeBps = effectiveFeeBps,
            isrRateBps = state.isrRateBps,
            rbfHoldbackBps = state.rbfHoldbackBps,
            isPersonaMoral = state.isPersonaMoral
        )
    }

    private fun updateUiState(update: (PosUiState) -> PosUiState) {
        _uiState.value = update(uiState.value)
    }
}
