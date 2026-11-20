package mx.softpos.app.core.network

import kotlinx.serialization.KSerializer
import kotlinx.serialization.SerializationException
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import java.io.IOException

/** Shared JSON configuration: tolerant to new backend fields. */
val KernoJson: Json = Json {
    ignoreUnknownKeys = true
    explicitNulls = false
    encodeDefaults = true
}

/** `statusCode` used in [ApiResult.Error] when no HTTP response arrived. */
const val NETWORK_ERROR_STATUS = 0
const val NETWORK_ERROR_CODE = "NETWORK_ERROR"

private val JSON_MEDIA_TYPE = "application/json; charset=utf-8".toMediaType()

fun String.toJsonBody(): RequestBody = toRequestBody(JSON_MEDIA_TYPE)

/**
 * Executes a blocking call and decodes a JSON body. Errors keep the backend's
 * `{"error":"CODE"}` code so the UI can map it. Call it off the main thread.
 */
fun <T> OkHttpClient.executeJson(request: Request, deserializer: KSerializer<T>?): ApiResult<T?> {
    return try {
        newCall(request).execute().use { response ->
            val body = response.body?.string().orEmpty()
            if (response.isSuccessful) {
                val data = if (deserializer == null || body.isBlank()) null
                else KernoJson.decodeFromString(deserializer, body)
                ApiResult.Success(data, response.code)
            } else {
                ApiResult.Error(response.code, parseErrorCode(body, response.code), body)
            }
        }
    } catch (e: IOException) {
        ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, e.message.orEmpty())
    } catch (e: SerializationException) {
        ApiResult.Error(NETWORK_ERROR_STATUS, "INVALID_RESPONSE", e.message.orEmpty())
    } catch (e: IllegalArgumentException) {
        ApiResult.Error(NETWORK_ERROR_STATUS, "INVALID_RESPONSE", e.message.orEmpty())
    }
}

/** Same as [executeJson] for endpoints whose success body is required. */
fun <T> OkHttpClient.executeJsonRequired(request: Request, deserializer: KSerializer<T>): ApiResult<T> =
    when (val r = executeJson(request, deserializer)) {
        is ApiResult.Success -> r.data?.let { ApiResult.Success(it, r.statusCode) }
            ?: ApiResult.Error(r.statusCode, "INVALID_RESPONSE", "empty body")
        is ApiResult.Error -> r
    }

internal fun parseErrorCode(body: String, status: Int): String =
    try {
        KernoJson.parseToJsonElement(body).jsonObject["error"]?.jsonPrimitive?.content
    } catch (_: Exception) {
        null
    } ?: "HTTP_$status"

/**
 * True when a money request (`Idempotency-Key`) may have been applied even
 * though it failed: no response, an unparsable one, or any 5xx except the
 * definitive ones. The retry must reuse the same key so the backend replays
 * the stored request instead of creating a second intent / refund.
 */
fun ApiResult.Error.isUnknownOutcome(): Boolean =
    statusCode == NETWORK_ERROR_STATUS || (statusCode >= 500 && errorCode !in DEFINITIVE_5XX)

/** 5xx codes the backend returns before storing anything, or after a definitive provider rejection. */
private val DEFINITIVE_5XX = setOf("ACQUIRER_REJECTED", "ACQUIRER_NOT_CONFIGURED")
