package mx.softpos.app.payments

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.executeJsonRequired
import mx.softpos.app.core.network.toJsonBody
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request

/**
 * PROVISIONAL contract (backend card-intent endpoints are being built in
 * parallel; see docs/ANDROID_APP.md). Adjust field names once they land.
 *
 * `POST /api/v1/card-payments/intents {amount_cents, currency:"MXN"}` with an
 * `Idempotency-Key` header → `{id, client_secret, status}`;
 * `GET /api/v1/card-payments/{id}` → same shape with the current status.
 */
@Serializable
data class CardPaymentIntent(
    @SerialName("id") val id: String,
    @SerialName("client_secret") val clientSecret: String = "",
    @SerialName("status") val status: String,
    @SerialName("amount_cents") val amountCents: Long = 0L
) {
    override fun toString(): String = "CardPaymentIntent(id=$id, status=$status, amountCents=$amountCents)"
}

object CardPaymentStatus {
    const val REQUIRES_TAP = "requires_tap"
    const val PROCESSING = "processing"
    const val SUCCEEDED = "succeeded"
    const val FAILED = "failed"

    private val approved = setOf(SUCCEEDED, "authorized", "captured", "settled")
    fun isApproved(status: String) = status.lowercase() in approved
    fun isFailed(status: String) = status.lowercase() in setOf(FAILED, "declined", "canceled", "cancelled")
}

interface CardPaymentsApi {
    fun createIntent(idempotencyKey: String, amountCents: Long): ApiResult<CardPaymentIntent>
    fun getPayment(id: String): ApiResult<CardPaymentIntent>
}

class HttpCardPaymentsApi(baseUrl: String, private val client: OkHttpClient) : CardPaymentsApi {
    private val base = baseUrl.trimEnd('/').toHttpUrl()

    override fun createIntent(idempotencyKey: String, amountCents: Long): ApiResult<CardPaymentIntent> {
        require(amountCents > 0L) { "amountCents must be positive" }
        require(idempotencyKey.isNotBlank()) { "idempotencyKey must not be blank" }
        val body = buildJsonObject {
            put("amount_cents", amountCents)
            put("currency", "MXN")
        }.toString()
        val request = Request.Builder()
            .url(base.newBuilder().addPathSegments("api/v1/card-payments/intents").build())
            .header("Idempotency-Key", idempotencyKey)
            .post(body.toJsonBody())
            .build()
        return client.executeJsonRequired(request, CardPaymentIntent.serializer())
    }

    override fun getPayment(id: String): ApiResult<CardPaymentIntent> {
        val request = Request.Builder()
            .url(base.newBuilder().addPathSegments("api/v1/card-payments").addPathSegment(id).build())
            .get()
            .build()
        return client.executeJsonRequired(request, CardPaymentIntent.serializer())
    }
}
