package mx.softpos.app.auth

import mx.softpos.app.core.network.ApiResult
import java.util.concurrent.atomic.AtomicInteger

/** Scriptable [AuthApi] for unit tests. */
class FakeAuthApi : AuthApi {
    var requestOtpResult: ApiResult<Unit> = ApiResult.Success(Unit, 202)
    var verifyResult: ApiResult<TokenPair> = ApiResult.Success(pair("at_1", "rt_1"), 200)
    var refreshResult: (String) -> ApiResult<TokenPair> = { ApiResult.Success(pair("at_new", "rt_new"), 200) }
    var refreshDelayMs: Long = 0L

    val refreshCalls = AtomicInteger(0)
    val logoutTokens = mutableListOf<String>()
    val otpPhones = mutableListOf<String>()
    var lastVerify: Pair<String, String>? = null

    override fun requestOtp(phoneE164: String): ApiResult<Unit> {
        otpPhones += phoneE164
        return requestOtpResult
    }

    override fun verifyOtp(phoneE164: String, code: String): ApiResult<TokenPair> {
        lastVerify = phoneE164 to code
        return verifyResult
    }

    override fun refresh(refreshToken: String, merchantId: String?): ApiResult<TokenPair> {
        refreshCalls.incrementAndGet()
        if (refreshDelayMs > 0) Thread.sleep(refreshDelayMs)
        return refreshResult(refreshToken)
    }

    override fun logout(refreshToken: String): ApiResult<Unit> {
        synchronized(logoutTokens) { logoutTokens += refreshToken }
        return ApiResult.Success(Unit, 204)
    }

    companion object {
        fun pair(access: String, refresh: String, merchant: String = "mer_1") = TokenPair(
            accessToken = access,
            expiresInSeconds = 900,
            refreshToken = refresh,
            refreshExpiresInSeconds = 2_592_000,
            userId = "usr_1",
            merchantId = merchant,
            role = "owner"
        )
    }
}
