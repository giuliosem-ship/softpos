package mx.softpos.app.fiscal

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.KernoJson
import mx.softpos.app.core.network.executeJsonRequired
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MultipartBody
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.nio.CharBuffer
import java.nio.charset.StandardCharsets

object CsdStatusValue {
    const val ACTIVE = "ACTIVE"
}

/**
 * `GET|PUT /api/v1/merchants/{id}/csd`: the merchant's SAT digital seal
 * certificate as stored by the backend (the private key never comes back).
 * `valid_from` / `valid_to` are RFC 3339 timestamps.
 */
@Serializable
data class CsdInfo(
    @SerialName("serial") val serial: String = "",
    @SerialName("rfc") val rfc: String = "",
    @SerialName("valid_from") val validFrom: String = "",
    @SerialName("valid_to") val validTo: String = "",
    @SerialName("status") val status: String = ""
) {
    val isActive: Boolean get() = status == CsdStatusValue.ACTIVE
}

/** A `.cer` or `.key` read from the document picker. The owner zeroes [bytes] once done. */
class CsdFile(val name: String, val bytes: ByteArray) {
    override fun toString(): String = "CsdFile($name, ${bytes.size} bytes)"
}

interface CsdApi {
    /** `404 CSD_NOT_FOUND` when the merchant never uploaded one. */
    fun getCsd(merchantId: String): ApiResult<CsdInfo>

    /**
     * Owner only. Multipart `cer`, `key`, `password`. The caller zeroes
     * [password] and the key bytes; this method zeroes its own UTF-8 copy.
     */
    fun uploadCsd(merchantId: String, cer: CsdFile, key: CsdFile, password: CharArray): ApiResult<CsdInfo>
}

/** `422 CSD_INVALID {"reason":...}`: why the backend rejected the pair (wrong password, key mismatch, FIEL...). */
fun ApiResult.Error.csdInvalidReason(): String? =
    if (statusCode != 422 || errorCode != "CSD_INVALID") null
    else runCatching { KernoJson.parseToJsonElement(message).jsonObject["reason"]?.jsonPrimitive?.content }.getOrNull()
        ?: ""

fun ApiResult.Error.isCsdNotFound(): Boolean = statusCode == 404 && errorCode == "CSD_NOT_FOUND"

class HttpCsdApi(baseUrl: String, private val client: OkHttpClient) : CsdApi {
    private val base = baseUrl.trimEnd('/').toHttpUrl()

    override fun getCsd(merchantId: String): ApiResult<CsdInfo> =
        client.executeJsonRequired(Request.Builder().url(csdUrl(merchantId)).get().build(), CsdInfo.serializer())

    override fun uploadCsd(merchantId: String, cer: CsdFile, key: CsdFile, password: CharArray): ApiResult<CsdInfo> {
        val passwordBytes = utf8(password)
        try {
            // The bodies reference the arrays (no copy), so zeroing them after the call
            // also clears what OkHttp would replay on an authenticator retry.
            val multipart = MultipartBody.Builder()
                .setType(MultipartBody.FORM)
                .addFormDataPart("cer", cer.name.ifBlank { "certificado.cer" }, cer.bytes.toRequestBody(BINARY))
                .addFormDataPart("key", key.name.ifBlank { "llave.key" }, key.bytes.toRequestBody(BINARY))
                .addFormDataPart("password", null, passwordBytes.toRequestBody(null))
                .build()
            val request = Request.Builder().url(csdUrl(merchantId)).put(multipart).build()
            return client.executeJsonRequired(request, CsdInfo.serializer())
        } finally {
            passwordBytes.fill(0)
        }
    }

    private fun csdUrl(merchantId: String): HttpUrl =
        base.newBuilder().addPathSegments("api/v1/merchants").addPathSegment(merchantId).addPathSegment("csd").build()

    private companion object {
        val BINARY = "application/octet-stream".toMediaType()
    }
}

/** UTF-8 bytes of [chars] without an intermediate String; the encoder buffer is zeroed. */
internal fun utf8(chars: CharArray): ByteArray {
    val buffer = StandardCharsets.UTF_8.newEncoder().encode(CharBuffer.wrap(chars))
    return try {
        ByteArray(buffer.remaining()).also { buffer.get(it) }
    } finally {
        if (buffer.hasArray()) buffer.array().fill(0)
    }
}
