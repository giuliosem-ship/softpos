package mx.softpos.app.auth

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import mx.softpos.app.core.network.ApiResult

/**
 * Owns the session: persistence, single-flight refresh and logout.
 *
 * Refresh is single-flight: concurrent callers that saw the same expired
 * access token block on one lock; the first rotates the refresh token, the
 * rest find a different access token already stored and reuse it. This
 * matters because the backend treats a second use of a rotated refresh token
 * as theft and revokes the whole family (docs/AUTH.md).
 */
class SessionManager(
    private val store: TokenStore,
    private val authApi: AuthApi,
    private val clock: () -> Long = System::currentTimeMillis
) {
    private val lock = Any()
    private val _session = MutableStateFlow(store.load())
    val session: StateFlow<StoredSession?> = _session.asStateFlow()

    fun current(): StoredSession? = _session.value

    fun onLogin(pair: TokenPair) {
        val stored = pair.toStoredSession(clock())
        store.save(stored)
        _session.value = stored
    }

    /** True when the access token expires within [skewMs]. */
    fun isAccessTokenExpiring(skewMs: Long = 30_000L): Boolean {
        val s = _session.value ?: return false
        return clock() + skewMs >= s.accessExpiresAtMs
    }

    /**
     * Returns a usable access token after [staleAccessToken] was rejected or
     * is about to expire, or null if the session is gone. Only a definitive
     * rejection of the refresh token ends the session: `401`
     * (`REFRESH_TOKEN_INVALID` / `REFRESH_TOKEN_REUSED`) or an OAuth-style
     * `invalid_grant`. Network errors, `429` (rate limit), `403` and 5xx keep
     * it, so a bad connection or a busy backend never logs the merchant out.
     */
    fun refreshAfterUnauthorized(staleAccessToken: String?): String? = synchronized(lock) {
        val current = _session.value ?: return null
        if (staleAccessToken != null && current.accessToken != staleAccessToken) {
            return current.accessToken
        }
        when (val result = authApi.refresh(current.refreshToken, current.merchantId.ifBlank { null })) {
            is ApiResult.Success -> {
                onLogin(result.data)
                result.data.accessToken
            }
            is ApiResult.Error -> {
                if (result.isDefinitiveRejection()) clearLocal()
                null
            }
        }
    }

    /**
     * Re-scopes the session to another membership through `/refresh
     * {refresh_token, merchant_id}` (docs/AUTH.md). Same single-flight lock as
     * the 401 refresh, so a concurrent refresh never reuses the rotated token.
     * Only a `401` (invalid/reused refresh token) ends the session; a `403
     * NOT_A_MERCHANT_MEMBER`, 429 or 5xx leaves the current session as it was.
     */
    fun switchMerchant(merchantId: String): ApiResult<Unit> = synchronized(lock) {
        val current = _session.value ?: return ApiResult.Error(401, "NO_SESSION", "")
        when (val result = authApi.refresh(current.refreshToken, merchantId)) {
            is ApiResult.Success -> {
                onLogin(result.data)
                ApiResult.Success(Unit, result.statusCode)
            }
            is ApiResult.Error -> {
                if (result.isDefinitiveRejection()) clearLocal()
                result
            }
        }
    }

    /** Revokes the refresh family on the server (best effort) and wipes the device. */
    fun logout() {
        val current = synchronized(lock) {
            val s = _session.value
            clearLocal()
            s
        } ?: return
        authApi.logout(current.refreshToken)
    }

    private fun ApiResult.Error.isDefinitiveRejection(): Boolean =
        statusCode == 401 || errorCode.equals("invalid_grant", ignoreCase = true)

    private fun clearLocal() {
        store.clear()
        _session.value = null
    }
}
