package mx.softpos.app.ui.login

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
import mx.softpos.app.auth.AuthApi
import mx.softpos.app.auth.SessionManager
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS

enum class LoginStep { PHONE, CODE }

enum class LoginError { INVALID_PHONE, INVALID_CODE_FORMAT, RATE_LIMITED, OTP_INVALID, OTP_LOCKED, NETWORK, UNKNOWN }

data class LoginUiState(
    val phoneInput: String = "",
    val codeInput: String = "",
    val step: LoginStep = LoginStep.PHONE,
    val phoneE164: String? = null,
    val isLoading: Boolean = false,
    val error: LoginError? = null
)

object PhoneNumbers {
    /** Mexican mobile numbers: 10 national digits, optionally with +52 / 52. */
    fun normalizeMx(input: String): String? {
        val digits = input.filter { it.isDigit() }
        val national = when {
            digits.length == 10 -> digits
            digits.length == 12 && digits.startsWith("52") -> digits.substring(2)
            else -> return null
        }
        if (national.first() == '0') return null
        return "+52$national"
    }
}

class LoginViewModel(
    private val authApi: AuthApi,
    private val sessions: SessionManager,
    private val io: CoroutineDispatcher = Dispatchers.IO
) : ViewModel() {
    private val _state = MutableStateFlow(LoginUiState())
    val state: StateFlow<LoginUiState> = _state.asStateFlow()

    fun onPhoneChange(value: String) =
        _state.update { it.copy(phoneInput = value.filter { c -> c.isDigit() || c == '+' }.take(13), error = null) }

    fun onCodeChange(value: String) =
        _state.update { it.copy(codeInput = value.filter { c -> c.isDigit() }.take(OTP_LENGTH), error = null) }

    fun requestOtp() {
        val phone = PhoneNumbers.normalizeMx(_state.value.phoneInput)
        if (phone == null) {
            _state.update { it.copy(error = LoginError.INVALID_PHONE) }
            return
        }
        if (_state.value.isLoading) return
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            val result = withContext(io) { authApi.requestOtp(phone) }
            _state.update {
                when (result) {
                    is ApiResult.Success -> it.copy(isLoading = false, step = LoginStep.CODE, phoneE164 = phone, codeInput = "")
                    is ApiResult.Error -> it.copy(isLoading = false, error = mapError(result))
                }
            }
        }
    }

    fun verifyOtp() {
        val current = _state.value
        val phone = current.phoneE164 ?: return
        if (current.codeInput.length != OTP_LENGTH) {
            _state.update { it.copy(error = LoginError.INVALID_CODE_FORMAT) }
            return
        }
        if (current.isLoading) return
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            val result = withContext(io) { authApi.verifyOtp(phone, current.codeInput) }
            when (result) {
                is ApiResult.Success -> {
                    sessions.onLogin(result.data)
                    _state.value = LoginUiState()
                }
                is ApiResult.Error -> _state.update {
                    it.copy(isLoading = false, codeInput = "", error = mapError(result))
                }
            }
        }
    }

    fun backToPhone() = _state.update { it.copy(step = LoginStep.PHONE, codeInput = "", error = null) }

    private fun mapError(error: ApiResult.Error): LoginError = when {
        error.statusCode == NETWORK_ERROR_STATUS -> LoginError.NETWORK
        error.errorCode == "INVALID_PHONE" -> LoginError.INVALID_PHONE
        error.errorCode == "OTP_INVALID" -> LoginError.OTP_INVALID
        error.errorCode == "OTP_LOCKED" -> LoginError.OTP_LOCKED
        error.statusCode == 429 -> LoginError.RATE_LIMITED
        else -> LoginError.UNKNOWN
    }

    companion object {
        const val OTP_LENGTH = 6
    }
}
