package mx.softpos.app.payments

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.executeJson
import mx.softpos.app.core.network.executeJsonRequired
import mx.softpos.app.core.network.toJsonBody
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request

/**
 * `POST /api/v1/card-payments/intents` response (docs/ACQUIRER_INTEGRATION.md §2).
 * [clientSecret] is a short-lived single-intent token for the processor SDK;
 * it is never logged ([toString]) nor persisted.
 */
@Serializable
data class CardPaymentIntent(
    @SerialName("card_payment_id") val cardPaymentId: String,
    @SerialName("status") val status: String = CardPaymentStatus.CREATED,
    @SerialName("amount_cents") val amountCents: Long,
    @SerialName("currency") val currency: String = "MXN",
    @SerialName("provider") val provider: String = "",
    @SerialName("provider_payment_id") val providerPaymentId: String = "",
    @SerialName("client_secret") val clientSecret: String = "",
    @SerialName("expires_at") val expiresAt: String = "",
    @SerialName("created_at") val createdAt: String = ""
) {
    override fun toString(): String =
        "CardPaymentIntent(id=$cardPaymentId, status=$status, amountCents=$amountCents, provider=$provider)"
}

/** Split recorded when the capture settled (subset of the settle response). */
@Serializable
data class CardSettlement(
    @SerialName("gross_amount_cents") val grossAmountCents: Long = 0L,
    @SerialName("platform_fee_cents") val platformFeeCents: Long = 0L,
    @SerialName("tax_escrow_cents") val taxEscrowCents: Long = 0L,
    @SerialName("rbf_sweep_cents") val rbfSweepCents: Long = 0L,
    @SerialName("net_available_cents") val netAvailableCents: Long = 0L
)

/** `GET /api/v1/card-payments/{id}`: the server-side truth, moved only by processor webhooks. */
@Serializable
data class CardPayment(
    @SerialName("card_payment_id") val cardPaymentId: String,
    @SerialName("status") val status: String,
    @SerialName("amount_cents") val amountCents: Long,
    @SerialName("currency") val currency: String = "MXN",
    @SerialName("provider") val provider: String = "",
    @SerialName("provider_payment_id") val providerPaymentId: String = "",
    @SerialName("reference") val reference: String = "",
    @SerialName("captured_cents") val capturedCents: Long = 0L,
    @SerialName("refunded_cents") val refundedCents: Long = 0L,
    @SerialName("chargeback_cents") val chargebackCents: Long = 0L,
    @SerialName("decline_code") val declineCode: String = "",
    @SerialName("final") val final: Boolean = false,
    @SerialName("settlement") val settlement: CardSettlement? = null,
    @SerialName("created_at") val createdAt: String = "",
    @SerialName("updated_at") val updatedAt: String = ""
) {
    /** What can still be refunded: captured − refunded − chargeback (never negative). */
    val refundableCents: Long
        get() = if (status in CardPaymentStatus.REFUNDABLE) {
            (capturedCents - refundedCents - chargebackCents).coerceAtLeast(0L)
        } else 0L
}

/** `POST /api/v1/card-payments/{id}/refunds` → `202`. */
@Serializable
data class CardRefund(
    @SerialName("refund_id") val refundId: String,
    @SerialName("card_payment_id") val cardPaymentId: String = "",
    @SerialName("amount_cents") val amountCents: Long = 0L,
    @SerialName("status") val status: String = "PENDING",
    @SerialName("provider_refund_id") val providerRefundId: String = ""
)

/** Backend `card_payments.status` values (upper case, as sent). */
object CardPaymentStatus {
    const val CREATED = "CREATED"
    const val AUTHORIZED = "AUTHORIZED"
    const val DECLINED = "DECLINED"
    const val CANCELED = "CANCELED"
    const val CAPTURED = "CAPTURED"
    const val PARTIALLY_REFUNDED = "PARTIALLY_REFUNDED"
    const val REFUNDED = "REFUNDED"
    const val CHARGEBACK = "CHARGEBACK"
    const val CHARGEBACK_REVERSED = "CHARGEBACK_REVERSED"

    val REFUNDABLE = setOf(CAPTURED, PARTIALLY_REFUNDED, CHARGEBACK_REVERSED)

    /** The money was captured at some point (later refunds/chargebacks may follow). */
    fun isCaptured(status: String) =
        status in setOf(CAPTURED, PARTIALLY_REFUNDED, REFUNDED, CHARGEBACK, CHARGEBACK_REVERSED)
}

/** Result of `POST /cancel`: `200` canceled at once, `202` asked to the processor. */
data class CancelResult(val canceled: Boolean, val requested: Boolean)

interface CardPaymentsApi {
    fun createIntent(idempotencyKey: String, amountCents: Long, reference: String? = null): ApiResult<CardPaymentIntent>
    fun getPayment(id: String): ApiResult<CardPayment>
    fun cancel(id: String): ApiResult<CancelResult>
    /** [amountCents] null = the whole refundable remainder. Owner only (backend). */
    fun refund(id: String, idempotencyKey: String, amountCents: Long?): ApiResult<CardRefund>
}

/** `/api/v1/card-payments/...` over the authenticated client. */
class HttpCardPaymentsApi(baseUrl: String, private val client: OkHttpClient) : CardPaymentsApi {
    private val base = baseUrl.trimEnd('/').toHttpUrl()

    override fun createIntent(idempotencyKey: String, amountCents: Long, reference: String?): ApiResult<CardPaymentIntent> {
        require(amountCents > 0L) { "amountCents must be positive" }
        require(idempotencyKey.isNotBlank()) { "idempotencyKey must not be blank" }
        // No is_persona_moral: the backend derives it from the merchant.
        val body = buildJsonObject {
            put("amount_cents", amountCents)
            put("currency", "MXN")
            if (!reference.isNullOrBlank()) put("reference", reference)
        }.toString()
        val request = Request.Builder()
            .url(paymentsUrl().addPathSegment("intents").build())
            .header("Idempotency-Key", idempotencyKey)
            .post(body.toJsonBody())
            .build()
        return client.executeJsonRequired(request, CardPaymentIntent.serializer())
    }

    override fun getPayment(id: String): ApiResult<CardPayment> {
        val request = Request.Builder().url(paymentsUrl().addPathSegment(id).build()).get().build()
        return client.executeJsonRequired(request, CardPayment.serializer())
    }

    override fun cancel(id: String): ApiResult<CancelResult> {
        val request = Request.Builder()
            .url(paymentsUrl().addPathSegment(id).addPathSegment("cancel").build())
            .post("{}".toJsonBody())
            .build()
        return when (val r = client.executeJson(request, JsonObject.serializer())) {
            is ApiResult.Success -> ApiResult.Success(
                CancelResult(canceled = r.statusCode == 200, requested = r.statusCode == 202),
                r.statusCode
            )
            is ApiResult.Error -> r
        }
    }

    override fun refund(id: String, idempotencyKey: String, amountCents: Long?): ApiResult<CardRefund> {
        require(idempotencyKey.isNotBlank()) { "idempotencyKey must not be blank" }
        require(amountCents == null || amountCents > 0L) { "amountCents must be positive" }
        val body = if (amountCents == null) "{}" else buildJsonObject { put("amount_cents", amountCents) }.toString()
        val request = Request.Builder()
            .url(paymentsUrl().addPathSegment(id).addPathSegment("refunds").build())
            .header("Idempotency-Key", idempotencyKey)
            .post(body.toJsonBody())
            .build()
        return client.executeJsonRequired(request, CardRefund.serializer())
    }

    private fun paymentsUrl(): HttpUrl.Builder = base.newBuilder().addPathSegments("api/v1/card-payments")
}
