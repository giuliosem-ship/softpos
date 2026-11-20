package mx.softpos.app.ui.login

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import mx.softpos.app.auth.FakeAuthApi
import mx.softpos.app.auth.InMemoryTokenStore
import mx.softpos.app.auth.SessionManager
import mx.softpos.app.core.network.ApiResult
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

@OptIn(ExperimentalCoroutinesApi::class)
class LoginViewModelTest {
    private val dispatcher = UnconfinedTestDispatcher()
    private val api = FakeAuthApi()
    private val sessions = SessionManager(InMemoryTokenStore(), api)
    private lateinit var vm: LoginViewModel

    @BeforeEach
    fun setUp() {
        Dispatchers.setMain(dispatcher)
        vm = LoginViewModel(api, sessions, dispatcher)
    }

    @AfterEach
    fun tearDown() = Dispatchers.resetMain()

    @Test
    fun normalizesMexicanNumbers() {
        assertEquals("+525512345678", PhoneNumbers.normalizeMx("55 1234 5678"))
        assertEquals("+525512345678", PhoneNumbers.normalizeMx("+52 55 1234 5678"))
        assertNull(PhoneNumbers.normalizeMx("12345"))
        assertNull(PhoneNumbers.normalizeMx("0512345678"))
    }

    @Test
    fun invalidPhoneNeverCallsBackend() {
        vm.onPhoneChange("123")
        vm.requestOtp()
        assertEquals(LoginError.INVALID_PHONE, vm.state.value.error)
        assertEquals(emptyList<String>(), api.otpPhones)
    }

    @Test
    fun fullOtpFlowCreatesSession() {
        vm.onPhoneChange("5512345678")
        vm.requestOtp()
        assertEquals(LoginStep.CODE, vm.state.value.step)
        assertEquals(listOf("+525512345678"), api.otpPhones)

        vm.onCodeChange("12a3456789")
        assertEquals("123456", vm.state.value.codeInput)
        vm.verifyOtp()

        assertEquals("+525512345678" to "123456", api.lastVerify)
        assertEquals("at_1", sessions.current()?.accessToken)
        assertEquals(LoginStep.PHONE, vm.state.value.step)
    }

    @Test
    fun shortCodeIsRejectedLocally() {
        vm.onPhoneChange("5512345678")
        vm.requestOtp()
        vm.onCodeChange("123")
        vm.verifyOtp()
        assertEquals(LoginError.INVALID_CODE_FORMAT, vm.state.value.error)
        assertNull(api.lastVerify)
    }

    @Test
    fun backendErrorsMapToUiErrors() {
        api.requestOtpResult = ApiResult.Error(429, "OTP_RATE_LIMITED", "")
        vm.onPhoneChange("5512345678")
        vm.requestOtp()
        assertEquals(LoginError.RATE_LIMITED, vm.state.value.error)

        api.requestOtpResult = ApiResult.Success(Unit, 202)
        vm.requestOtp()
        api.verifyResult = ApiResult.Error(401, "OTP_INVALID", "")
        vm.onCodeChange("000000")
        vm.verifyOtp()
        assertEquals(LoginError.OTP_INVALID, vm.state.value.error)
        assertEquals("", vm.state.value.codeInput)
        assertNull(sessions.current())

        api.verifyResult = ApiResult.Error(429, "OTP_LOCKED", "")
        vm.onCodeChange("000000")
        vm.verifyOtp()
        assertEquals(LoginError.OTP_LOCKED, vm.state.value.error)
    }
}
