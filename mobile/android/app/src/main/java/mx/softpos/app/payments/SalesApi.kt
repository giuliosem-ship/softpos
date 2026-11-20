package mx.softpos.app.payments

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.executeJsonRequired
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request

/** One sale (card, SPEI or CoDi) as listed by the backend, newest first. */
@Serializable
data class Sale(
    @SerialName("id") val id: String,
    @SerialName("kind") val kind: String,
    @SerialName("status") val status: String,
    @SerialName("amount_cents") val amountCents: Long,
    @SerialName("created_at") val createdAt: String = "",
    @SerialName("card_payment_id") val cardPaymentId: String? = null
)

@Serializable
data class SalesPage(
    @SerialName("items") val items: List<Sale> = emptyList(),
    @SerialName("next_cursor") val nextCursor: String? = null
)

object SaleKind {
    const val CARD = "card"
    const val SPEI = "spei"
    const val CODI = "codi"
}

/** Coarse status groups for labels (card statuses plus SPEI/CoDi ones). */
enum class SaleStatusGroup { APPROVED, PENDING, FAILED, PARTIALLY_REFUNDED, REFUNDED, CHARGEBACK }

object SaleStatus {
    /**
     * Card: card_payments.status. SPEI and CoDi come from merchant_sales, which
     * only holds money already credited: spei `RECEIVED`, codi `CONFIRMED`.
     */
    private val approved = setOf(
        CardPaymentStatus.CAPTURED, CardPaymentStatus.CHARGEBACK_REVERSED, SPEI_RECEIVED, CODI_CONFIRMED
    )
    const val SPEI_RECEIVED = "RECEIVED"
    const val CODI_CONFIRMED = "CONFIRMED"
    private val failed = setOf(
        CardPaymentStatus.DECLINED, CardPaymentStatus.CANCELED
    )

    fun group(status: String): SaleStatusGroup = when (status.uppercase()) {
        CardPaymentStatus.PARTIALLY_REFUNDED -> SaleStatusGroup.PARTIALLY_REFUNDED
        CardPaymentStatus.REFUNDED -> SaleStatusGroup.REFUNDED
        CardPaymentStatus.CHARGEBACK -> SaleStatusGroup.CHARGEBACK
        in approved -> SaleStatusGroup.APPROVED
        in failed -> SaleStatusGroup.FAILED
        else -> SaleStatusGroup.PENDING
    }
}

/**
 * `GET /api/v1/merchants/{id}/sales?limit=50&before=<cursor>` →
 * `{"items":[Sale…],"next_cursor":string|null}` (null = last page).
 */
interface SalesApi {
    fun listSales(merchantId: String, limit: Int = PAGE_SIZE, before: String? = null): ApiResult<SalesPage>

    companion object {
        const val PAGE_SIZE = 50
    }
}

class HttpSalesApi(baseUrl: String, private val client: OkHttpClient) : SalesApi {
    private val base = baseUrl.trimEnd('/').toHttpUrl()

    override fun listSales(merchantId: String, limit: Int, before: String?): ApiResult<SalesPage> {
        val url = base.newBuilder()
            .addPathSegments("api/v1/merchants")
            .addPathSegment(merchantId)
            .addPathSegment("sales")
            .addQueryParameter("limit", limit.toString())
            .apply { if (!before.isNullOrBlank()) addQueryParameter("before", before) }
            .build()
        return client.executeJsonRequired(Request.Builder().url(url).get().build(), SalesPage.serializer())
    }
}
