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
     * is about to expire, or null if the session is gone. A 4xx from
     * `/refresh` (expired, revoked or reused token) ends the session; network
     * or 5xx errors keep it so the user is not logged out by a bad connection.
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
                if (result.statusCode in 400..499) clearLocal()
                null
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

    private fun clearLocal() {
        store.clear()
        _session.value = null
    }
}
