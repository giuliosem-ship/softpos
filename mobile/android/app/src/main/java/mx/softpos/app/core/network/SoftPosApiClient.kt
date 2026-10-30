package mx.softpos.app.core.network

import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.util.UUID

sealed interface ApiResult<out T> {
    data class Success<T>(val data: T, val statusCode: Int) : ApiResult<T>
    data class Error(val statusCode: Int, val errorCode: String, val message: String) : ApiResult<Nothing>
}

class SoftPosApiClient(
    private val baseUrl: String,
    private val integrityTokenProvider: () -> String,
    private val httpClient: OkHttpClient = OkHttpClient()
) {
    private val jsonMediaType = "application/json; charset=utf-8".toMediaType()
    private val normalizedBaseUrl = baseUrl.trimEnd('/')

    fun authorizeEmv(
        idempotencyKey: String,
        merchantId: String,
        amountCents: Long,
        tlvHex: String
    ): ApiResult<String> {
        require(merchantId.isNotBlank()) { "merchantId must not be blank" }
        require(amountCents > 0L) { "amountCents must be positive" }
        require(tlvHex.isNotBlank()) { "tlvHex must not be blank" }

        val payload = buildJsonObject {
            put("merchant_id", merchantId)
            put("amount_cents", amountCents)
            put("tlv_hex", tlvHex)
        }.toString()

        return executePost(
            path = "/api/v1/payments/emv/authorize",
            idempotencyKey = idempotencyKey,
            jsonBody = payload
        )
    }

    fun generateCodiQr(
        idempotencyKey: String,
        merchantId: String,
        amountCents: Long,
        concept: String
    ): ApiResult<String> {
        require(merchantId.isNotBlank()) { "merchantId must not be blank" }
        require(amountCents > 0L) { "amountCents must be positive" }
        require(concept.isNotBlank()) { "concept must not be blank" }

        val payload = buildJsonObject {
            put("merchant_id", merchantId)
            put("amount_cents", amountCents)
            put("concept", concept)
        }.toString()

        return executePost(
            path = "/api/v1/payments/codi/qr",
            idempotencyKey = idempotencyKey,
            jsonBody = payload
        )
    }

    fun stampCfdi(
        idempotencyKey: String,
        paymentId: String,
        receptorRfc: String,
        receptorCp: String,
        regimenFiscal: String
    ): ApiResult<String> {
        require(paymentId.isNotBlank()) { "paymentId must not be blank" }
        require(receptorRfc.isNotBlank()) { "receptorRfc must not be blank" }
        require(receptorCp.isNotBlank()) { "receptorCp must not be blank" }
        require(regimenFiscal.isNotBlank()) { "regimenFiscal must not be blank" }

        val payload = buildJsonObject {
            put("payment_id", paymentId)
            put("receptor_rfc", receptorRfc)
            put("receptor_cp", receptorCp)
            put("regimen_fiscal", regimenFiscal)
        }.toString()

        return executePost(
            path = "/api/v1/invoices/cfdi",
            idempotencyKey = idempotencyKey,
            jsonBody = payload
        )
    }

    private fun executePost(
        path: String,
        idempotencyKey: String,
        jsonBody: String
    ): ApiResult<String> {
        val effectiveIdempotencyKey = idempotencyKey.ifBlank { UUID.randomUUID().toString() }
        val integrityToken = integrityTokenProvider()

        val request = Request.Builder()
            .url("$normalizedBaseUrl$path")
            .header("Idempotency-Key", effectiveIdempotencyKey)
            .header("X-Play-Integrity-Token", integrityToken)
            .post(jsonBody.toRequestBody(jsonMediaType))
            .build()

        httpClient.newCall(request).execute().use { response ->
            val bodyString = response.body?.string().orEmpty()
            if (response.isSuccessful) {
                return ApiResult.Success(data = bodyString, statusCode = response.code)
            }
            val mappedErrorCode = when (response.code) {
                409 -> "IDEMPOTENCY_CONFLICT"
                422 -> "SAT_COMPLIANCE_REJECTED"
                else -> "HTTP_ERROR"
            }
            return ApiResult.Error(
                statusCode = response.code,
                errorCode = mappedErrorCode,
                message = bodyString
            )
        }
    }
}
