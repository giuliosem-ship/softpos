package mx.softpos.app.payments

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.executeJsonRequired
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request

/** One sale (card or SPEI) as shown in the history. */
@Serializable
data class Sale(
    @SerialName("id") val id: String,
    @SerialName("amount_cents") val amountCents: Long,
    @SerialName("method") val method: String,
    @SerialName("status") val status: String,
    @SerialName("created_at") val createdAt: String = "",
    @SerialName("reference") val reference: String = ""
)

@Serializable
data class SalesPage(@SerialName("sales") val sales: List<Sale> = emptyList())

object SaleMethod {
    const val CARD = "CARD"
    const val SPEI = "SPEI"
}

/**
 * PROVISIONAL: the backend has no sales listing yet. Expected contract:
 * `GET /api/v1/merchants/{id}/sales?limit=50` → `{"sales":[Sale…]}`, newest
 * first, tenant from the token.
 */
interface SalesApi {
    fun listSales(merchantId: String, limit: Int = 50): ApiResult<SalesPage>
}

class HttpSalesApi(baseUrl: String, private val client: OkHttpClient) : SalesApi {
    private val base = baseUrl.trimEnd('/').toHttpUrl()

    override fun listSales(merchantId: String, limit: Int): ApiResult<SalesPage> {
        val url = base.newBuilder()
            .addPathSegments("api/v1/merchants")
            .addPathSegment(merchantId)
            .addPathSegment("sales")
            .addQueryParameter("limit", limit.toString())
            .build()
        return client.executeJsonRequired(Request.Builder().url(url).get().build(), SalesPage.serializer())
    }
}
