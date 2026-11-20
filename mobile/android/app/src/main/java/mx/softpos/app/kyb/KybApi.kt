package mx.softpos.app.kyb

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.KernoJson
import mx.softpos.app.core.network.executeJson
import mx.softpos.app.core.network.executeJsonRequired
import mx.softpos.app.core.network.toJsonBody
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MultipartBody
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okio.BufferedSink

object KybStatusValue {
    const val DRAFT = "DRAFT"
    const val SUBMITTED = "SUBMITTED"
    const val IN_REVIEW = "IN_REVIEW"
    const val APPROVED = "APPROVED"
    const val REJECTED = "REJECTED"
}

object KybDocumentType {
    const val INE_FRONT = "INE_FRONT"
    const val INE_BACK = "INE_BACK"
    const val SELFIE = "SELFIE"
    const val PROOF_OF_ADDRESS = "PROOF_OF_ADDRESS"
    const val BANK_STATEMENT = "BANK_STATEMENT"
    const val CSF = "CSF"

    val ALL = listOf(INE_FRONT, INE_BACK, SELFIE, PROOF_OF_ADDRESS, BANK_STATEMENT, CSF)
}

@Serializable
data class KybChecks(
    @SerialName("rfc_valid") val rfcValid: Boolean = false,
    /** SAT 69-B list: CLEAR | LISTED | UNKNOWN. */
    @SerialName("list_69b") val list69b: String = "UNKNOWN",
    @SerialName("documents_complete") val documentsComplete: Boolean = false
)

/** `GET /api/v1/merchants/{id}/kyb`. */
@Serializable
data class KybStatus(
    @SerialName("status") val status: String,
    @SerialName("reason") val reason: String? = null,
    @SerialName("checks") val checks: KybChecks = KybChecks(),
    @SerialName("required_documents") val requiredDocuments: List<String> = emptyList(),
    @SerialName("submitted_at") val submittedAt: String? = null,
    @SerialName("decided_at") val decidedAt: String? = null
) {
    val isApproved: Boolean get() = status == KybStatusValue.APPROVED
}

@Serializable
data class KybAddress(
    @SerialName("street") val street: String,
    @SerialName("ext_number") val extNumber: String,
    @SerialName("int_number") val intNumber: String? = null,
    @SerialName("neighborhood") val neighborhood: String,
    @SerialName("city") val city: String,
    @SerialName("state") val state: String,
    @SerialName("postal_code") val postalCode: String
)

/** `PUT /api/v1/merchants/{id}/kyb` body (owner). */
@Serializable
data class KybApplication(
    @SerialName("legal_name") val legalName: String,
    @SerialName("rfc") val rfc: String,
    @SerialName("curp") val curp: String? = null,
    @SerialName("tax_regime") val taxRegime: String,
    @SerialName("mcc") val mcc: String,
    @SerialName("address") val address: KybAddress,
    @SerialName("contact_email") val contactEmail: String,
    @SerialName("contact_phone") val contactPhone: String
)

@Serializable
data class KybDocumentReceipt(
    @SerialName("document_id") val documentId: String,
    @SerialName("type") val type: String,
    @SerialName("status") val status: String = "RECEIVED"
)

/** A captured document ready to upload. [bytes] are zeroed by the owner once done. */
class PickedDocument(val bytes: ByteArray, val mimeType: String, val fileName: String) {
    override fun toString(): String = "PickedDocument($fileName, $mimeType, ${bytes.size} bytes)"
}

interface KybApi {
    fun getKyb(merchantId: String): ApiResult<KybStatus>
    fun putKyb(merchantId: String, application: KybApplication): ApiResult<Unit>
    /** [onProgress] gets (bytes sent, total) from the uploading thread. */
    fun uploadDocument(
        merchantId: String,
        type: String,
        document: PickedDocument,
        onProgress: (Long, Long) -> Unit
    ): ApiResult<KybDocumentReceipt>
    fun submit(merchantId: String): ApiResult<Unit>
}

class HttpKybApi(baseUrl: String, private val client: OkHttpClient) : KybApi {
    private val base = baseUrl.trimEnd('/').toHttpUrl()

    override fun getKyb(merchantId: String): ApiResult<KybStatus> =
        client.executeJsonRequired(Request.Builder().url(kybUrl(merchantId).build()).get().build(), KybStatus.serializer())

    override fun putKyb(merchantId: String, application: KybApplication): ApiResult<Unit> {
        val body = KernoJson.encodeToString(KybApplication.serializer(), application)
        return unit(Request.Builder().url(kybUrl(merchantId).build()).put(body.toJsonBody()).build())
    }

    override fun uploadDocument(
        merchantId: String,
        type: String,
        document: PickedDocument,
        onProgress: (Long, Long) -> Unit
    ): ApiResult<KybDocumentReceipt> {
        val file = ProgressRequestBody(document.bytes, document.mimeType.toMediaType(), onProgress)
        val multipart = MultipartBody.Builder()
            .setType(MultipartBody.FORM)
            .addFormDataPart("type", type)
            .addFormDataPart("file", document.fileName, file)
            .build()
        val request = Request.Builder().url(kybUrl(merchantId).addPathSegment("documents").build()).post(multipart).build()
        return client.executeJsonRequired(request, KybDocumentReceipt.serializer())
    }

    override fun submit(merchantId: String): ApiResult<Unit> =
        unit(Request.Builder().url(kybUrl(merchantId).addPathSegment("submit").build()).post("{}".toJsonBody()).build())

    /** Success bodies of PUT/submit are not needed: the caller re-reads the status. */
    private fun unit(request: Request): ApiResult<Unit> = when (val r = client.executeJson<Unit>(request, null)) {
        is ApiResult.Success -> ApiResult.Success(Unit, r.statusCode)
        is ApiResult.Error -> r
    }

    private fun kybUrl(merchantId: String): HttpUrl.Builder =
        base.newBuilder().addPathSegments("api/v1/merchants").addPathSegment(merchantId).addPathSegment("kyb")
}

/** Streams [bytes] in chunks and reports progress (no copy of the document). */
internal class ProgressRequestBody(
    private val bytes: ByteArray,
    private val type: MediaType,
    private val onProgress: (Long, Long) -> Unit
) : RequestBody() {
    override fun contentType(): MediaType = type
    override fun contentLength(): Long = bytes.size.toLong()

    override fun writeTo(sink: BufferedSink) {
        val total = bytes.size.toLong()
        var offset = 0
        while (offset < bytes.size) {
            val n = minOf(CHUNK, bytes.size - offset)
            sink.write(bytes, offset, n)
            sink.flush()
            offset += n
            onProgress(offset.toLong(), total)
        }
        if (bytes.isEmpty()) onProgress(0L, 0L)
    }

    private companion object {
        const val CHUNK = 16 * 1024
    }
}
