package mx.softpos.app.payments

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.KernoJson
import mx.softpos.app.core.network.executeJsonRequired
import mx.softpos.app.core.network.toJsonBody
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request

/**
 * One sale (card, SPEI or CoDi) as listed by the backend, newest first. The
 * detail (`GET …/sales/{sale_id}`) adds [card], [receiptUrl] and
 * [receiptExpiresAt]; they are null in the list.
 */
@Serializable
data class Sale(
    @SerialName("id") val id: String,
    @SerialName("kind") val kind: String,
    @SerialName("status") val status: String,
    @SerialName("amount_cents") val amountCents: Long,
    @SerialName("created_at") val createdAt: String = "",
    @SerialName("card_payment_id") val cardPaymentId: String? = null,
    /** Card sales only: brand, `**** 1234`, authorization code (never more than the last 4 digits). */
    @SerialName("card") val card: SaleCard? = null,
    /** Public receipt (autofactura) link of a confirmed sale; absolute once read through [HttpSalesApi]. */
    @SerialName("receipt_url") val receiptUrl: String? = null,
    @SerialName("receipt_expires_at") val receiptExpiresAt: String? = null
)

/**
 * Card data of a sale. The backend only stores the last 4 digits; [sanitized]
 * drops anything else so the app never holds more (PCI), whatever it is sent.
 */
@Serializable
data class SaleCard(
    @SerialName("brand") val brand: String = "",
    @SerialName("masked_pan") val maskedPan: String = "",
    @SerialName("auth_code") val authCode: String = ""
) {
    /** Last 4 digits, or null when fewer were sent (e.g. the empty card object of an untapped intent). */
    val last4: String? get() = maskedPan.filter { it.isDigit() }.takeIf { it.length >= 4 }?.takeLast(4)

    fun sanitized(): SaleCard = copy(maskedPan = last4?.let { "**** $it" }.orEmpty())

    override fun toString(): String = "SaleCard(brand=$brand, last4=$last4)"
}

/** `POST …/sales/{sale_id}/receipt`: at least one destination; phone in E.164 (`+52…`). */
@Serializable
data class SendReceiptRequest(
    @SerialName("email") val email: String? = null,
    @SerialName("phone") val phone: String? = null
)

/** `{"receipt_url","expires_at","sent":{"email":bool,"sms":bool}}` (also the body of a `502`). */
@Serializable
data class SendReceiptResponse(
    @SerialName("receipt_url") val receiptUrl: String = "",
    @SerialName("expires_at") val expiresAt: String = "",
    @SerialName("sent") val sent: Map<String, Boolean> = emptyMap()
)

/** The body of a `502` (a channel failed): which channels went out. Null for other errors. */
fun ApiResult.Error.receiptSendResult(): SendReceiptResponse? =
    if (statusCode != 502) null
    else runCatching { KernoJson.decodeFromString(SendReceiptResponse.serializer(), message) }.getOrNull()
        ?.takeIf { it.sent.isNotEmpty() }

/** `422 RECEIPT_DESTINATION_INVALID {"fields":{"email":"INVALID_FORMAT",…}}` → field names. */
fun ApiResult.Error.invalidReceiptFields(): Set<String> =
    if (statusCode != 422 || errorCode != "RECEIPT_DESTINATION_INVALID") emptySet()
    else runCatching { (KernoJson.parseToJsonElement(message).jsonObject["fields"] as? JsonObject)?.keys }.getOrNull().orEmpty()

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

    /** `GET …/sales/{sale_id}` (any member): the sale with its card data and receipt link. */
    fun getSale(merchantId: String, saleId: String): ApiResult<Sale>

    /** `POST …/sales/{sale_id}/receipt` (owner or cashier): email and/or SMS with the receipt link. */
    fun sendReceipt(merchantId: String, saleId: String, request: SendReceiptRequest): ApiResult<SendReceiptResponse>

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

    override fun getSale(merchantId: String, saleId: String): ApiResult<Sale> {
        val url = sale(merchantId, saleId).build()
        return when (val r = client.executeJsonRequired(Request.Builder().url(url).get().build(), Sale.serializer())) {
            is ApiResult.Success -> ApiResult.Success(
                r.data.copy(card = r.data.card?.sanitized(), receiptUrl = r.data.receiptUrl?.let(::absolute)),
                r.statusCode
            )
            is ApiResult.Error -> r
        }
    }

    override fun sendReceipt(merchantId: String, saleId: String, request: SendReceiptRequest): ApiResult<SendReceiptResponse> {
        val clean = SendReceiptRequest(email = request.email?.trim()?.ifBlank { null }, phone = request.phone?.trim()?.ifBlank { null })
        val body = KernoJson.encodeToString(SendReceiptRequest.serializer(), clean)
        val url = sale(merchantId, saleId).addPathSegment("receipt").build()
        return when (val r = client.executeJsonRequired(Request.Builder().url(url).post(body.toJsonBody()).build(), SendReceiptResponse.serializer())) {
            is ApiResult.Success -> ApiResult.Success(r.data.copy(receiptUrl = absolute(r.data.receiptUrl)), r.statusCode)
            is ApiResult.Error -> r
        }
    }

    private fun sale(merchantId: String, saleId: String): HttpUrl.Builder =
        base.newBuilder().addPathSegments("api/v1/merchants").addPathSegment(merchantId)
            .addPathSegment("sales").addPathSegment(saleId)

    /** `RECEIPT_BASE_URL` unset → the backend sends `/r/…`: resolve it against the API host. */
    private fun absolute(url: String): String = if (url.isBlank()) url else base.resolve(url)?.toString() ?: url
}
