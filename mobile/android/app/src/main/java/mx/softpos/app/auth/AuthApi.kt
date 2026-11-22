package mx.softpos.app.auth

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.executeJson
import mx.softpos.app.core.network.executeJsonRequired
import mx.softpos.app.core.network.toJsonBody
import okhttp3.OkHttpClient
import okhttp3.Request

/**
 * One `(user, merchant, role)` link (`merchant_memberships`). [merchantName]
 * is the business name the backend adds on login/refresh (docs/AUTH.md);
 * empty when it could not be read and in sessions saved before AND-41.
 */
@Serializable
data class MembershipInfo(
    @SerialName("merchant_id") val merchantId: String,
    @SerialName("role") val role: String = "",
    @SerialName("merchant_name") val merchantName: String = ""
) {
    /** Name for the picker, or null to fall back to the merchant id. */
    val displayName: String? get() = merchantName.trim().ifBlank { null }
}

/** Token pair returned by `/otp/verify` and `/refresh` (docs/AUTH.md). */
@Serializable
data class TokenPair(
    @SerialName("access_token") val accessToken: String,
    @SerialName("token_type") val tokenType: String = "Bearer",
    @SerialName("expires_in") val expiresInSeconds: Long,
    @SerialName("refresh_token") val refreshToken: String,
    @SerialName("refresh_expires_in") val refreshExpiresInSeconds: Long = 0L,
    @SerialName("user_id") val userId: String = "",
    @SerialName("merchant_id") val merchantId: String = "",
    @SerialName("role") val role: String = "",
    @SerialName("memberships") val memberships: List<MembershipInfo> = emptyList()
) {
    /** Never prints the access or refresh token (logs, crash reports). */
    override fun toString(): String =
        "TokenPair(user=$userId, merchant=$merchantId, role=$role, expiresIn=$expiresInSeconds, memberships=${memberships.size})"
}

/**
 * Backend auth contract. Calls are blocking (OkHttp); ViewModels run them on
 * an IO dispatcher and the OkHttp authenticator runs them on its own thread.
 */
interface AuthApi {
    fun requestOtp(phoneE164: String): ApiResult<Unit>
    fun verifyOtp(phoneE164: String, code: String): ApiResult<TokenPair>
    fun refresh(refreshToken: String, merchantId: String?): ApiResult<TokenPair>
    fun logout(refreshToken: String): ApiResult<Unit>
}

/**
 * [AuthApi] over HTTP. Must use a client WITHOUT the auth interceptor and
 * authenticator, otherwise a 401 on `/refresh` would recurse.
 */
class HttpAuthApi(baseUrl: String, private val client: OkHttpClient) : AuthApi {
    private val base = baseUrl.trimEnd('/')

    override fun requestOtp(phoneE164: String): ApiResult<Unit> =
        post("/api/v1/auth/otp/request", buildJsonObject { put("phone", phoneE164) }.toString())

    override fun verifyOtp(phoneE164: String, code: String): ApiResult<TokenPair> {
        val body = buildJsonObject {
            put("phone", phoneE164)
            put("code", code)
        }.toString()
        return client.executeJsonRequired(request("/api/v1/auth/otp/verify", body), TokenPair.serializer())
    }

    override fun refresh(refreshToken: String, merchantId: String?): ApiResult<TokenPair> {
        val body = buildJsonObject {
            put("refresh_token", refreshToken)
            if (!merchantId.isNullOrBlank()) put("merchant_id", merchantId)
        }.toString()
        return client.executeJsonRequired(request("/api/v1/auth/refresh", body), TokenPair.serializer())
    }

    override fun logout(refreshToken: String): ApiResult<Unit> =
        post("/api/v1/auth/logout", buildJsonObject { put("refresh_token", refreshToken) }.toString())

    private fun post(path: String, json: String): ApiResult<Unit> =
        when (val r = client.executeJson<Unit>(request(path, json), null)) {
            is ApiResult.Success -> ApiResult.Success(Unit, r.statusCode)
            is ApiResult.Error -> r
        }

    private fun request(path: String, json: String): Request =
        Request.Builder().url(base + path).post(json.toJsonBody()).build()
}
