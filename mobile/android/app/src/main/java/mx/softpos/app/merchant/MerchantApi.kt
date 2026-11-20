package mx.softpos.app.merchant

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.executeJsonRequired
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request

/** `GET /api/v1/merchants/{id}/balances` (pkg/api/handlers_merchants.go). */
@Serializable
data class MerchantBalances(
    @SerialName("merchant_id") val merchantId: String,
    @SerialName("available_cents") val availableCents: Long,
    @SerialName("tax_escrow_cents") val taxEscrowCents: Long,
    @SerialName("rbf_debt_cents") val rbfDebtCents: Long = 0L,
    @SerialName("currency") val currency: String = "MXN"
)

/** `GET /api/v1/merchants/{id}`. */
@Serializable
data class MerchantProfile(
    @SerialName("id") val id: String = "",
    @SerialName("rfc") val rfc: String = "",
    @SerialName("name") val name: String = "",
    @SerialName("is_resico") val isResico: Boolean = false,
    // Backend Merchant JSON tag is "clabe" (pkg/api/server.go), not "virtual_clabe".
    @SerialName("clabe") val virtualClabe: String = ""
)

interface MerchantApi {
    fun getBalances(merchantId: String): ApiResult<MerchantBalances>
    fun getMerchant(merchantId: String): ApiResult<MerchantProfile>
}

/** Tenant routes; [client] must be the authenticated client. */
class HttpMerchantApi(baseUrl: String, private val client: OkHttpClient) : MerchantApi {
    private val base = baseUrl.trimEnd('/').toHttpUrl()

    override fun getBalances(merchantId: String): ApiResult<MerchantBalances> =
        client.executeJsonRequired(get(merchantId, "balances"), MerchantBalances.serializer())

    override fun getMerchant(merchantId: String): ApiResult<MerchantProfile> =
        client.executeJsonRequired(get(merchantId, null), MerchantProfile.serializer())

    private fun get(merchantId: String, suffix: String?): Request {
        val url = base.newBuilder()
            .addPathSegments("api/v1/merchants")
            .addPathSegment(merchantId)
            .apply { if (suffix != null) addPathSegment(suffix) }
            .build()
        return Request.Builder().url(url).get().build()
    }
}
