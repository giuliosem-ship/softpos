package mx.softpos.app.fiscal

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.SerializationException
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.KernoJson
import mx.softpos.app.core.network.NETWORK_ERROR_CODE
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.core.network.executeJsonRequired
import mx.softpos.app.core.network.parseErrorCode
import mx.softpos.app.core.network.toJsonBody
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request
import java.io.IOException

object InvoiceStatus {
    const val STAMPED = "STAMPED"
    const val PENDING = "PENDING"
    const val FAILED = "FAILED"
    const val CANCEL_REQUESTED = "CANCEL_REQUESTED"
    const val CANCELED = "CANCELED"
}

/** CFDI receiver. Personal data of the merchant's customer: never printed. */
@Serializable
data class Receptor(
    @SerialName("rfc") val rfc: String,
    @SerialName("nombre") val nombre: String,
    @SerialName("regimen_fiscal") val regimenFiscal: String,
    @SerialName("codigo_postal") val codigoPostal: String,
    @SerialName("uso_cfdi") val usoCfdi: String,
    @SerialName("email") val email: String? = null
) {
    override fun toString(): String = "Receptor(<redacted>)"
}

@Serializable
data class CreateInvoiceRequest(
    @SerialName("sale_ids") val saleIds: List<String>,
    @SerialName("receptor") val receptor: Receptor
)

/** Invoice as returned by create / get / list / cancel. Money is Long cents. */
@Serializable
data class Invoice(
    @SerialName("invoice_id") val invoiceId: String,
    @SerialName("status") val status: String,
    @SerialName("uuid") val uuid: String? = null,
    @SerialName("serie") val serie: String? = null,
    @SerialName("folio") val folio: String? = null,
    @SerialName("total_cents") val totalCents: Long = 0L,
    @SerialName("subtotal_cents") val subtotalCents: Long = 0L,
    @SerialName("iva_cents") val ivaCents: Long = 0L,
    @SerialName("receptor") val receptor: Receptor? = null,
    @SerialName("sale_ids") val saleIds: List<String> = emptyList(),
    @SerialName("created_at") val createdAt: String = "",
    @SerialName("stamped_at") val stampedAt: String? = null,
    @SerialName("error") val error: String? = null
) {
    val isStamped: Boolean get() = status == InvoiceStatus.STAMPED
}

@Serializable
data class InvoicePage(
    @SerialName("items") val items: List<Invoice> = emptyList(),
    @SerialName("next_cursor") val nextCursor: String? = null
)

/** SAT cancellation reasons (Anexo 20, c_MotivoCancelacion). */
object CancelMotivo {
    /** Comprobante emitido con errores con relación: requires `folio_sustitucion`. */
    const val WITH_RELATION = "01"
    const val WITHOUT_RELATION = "02"
    const val NOT_EXECUTED = "03"
    const val NOMINATIVE_GLOBAL = "04"
    val ALL = listOf(WITH_RELATION, WITHOUT_RELATION, NOT_EXECUTED, NOMINATIVE_GLOBAL)
}

@Serializable
data class CancelInvoiceRequest(
    @SerialName("motivo") val motivo: String,
    @SerialName("folio_sustitucion") val folioSustitucion: String? = null
)

@Serializable
data class RegimenFiscal(
    @SerialName("code") val code: String,
    @SerialName("description") val description: String = "",
    @SerialName("persona_fisica") val personaFisica: Boolean = false,
    @SerialName("persona_moral") val personaMoral: Boolean = false
)

@Serializable
data class UsoCfdi(
    @SerialName("code") val code: String,
    @SerialName("description") val description: String = "",
    /** c_RegimenFiscal codes this use is valid for. */
    @SerialName("regimenes") val regimenes: List<String> = emptyList()
)

@Serializable
data class SatCatalogs(
    @SerialName("regimen_fiscal") val regimenFiscal: List<RegimenFiscal> = emptyList(),
    @SerialName("uso_cfdi") val usoCfdi: List<UsoCfdi> = emptyList()
)

/** Catalog response: [catalogs] null = `304 Not Modified` (keep the cached copy). */
data class CatalogResponse(val catalogs: SatCatalogs?, val etag: String?)

enum class InvoiceFileKind(val pathSegment: String, val mimeType: String, val extension: String) {
    PDF("pdf", "application/pdf", "pdf"),
    XML("xml", "application/xml", "xml")
}

interface InvoiceApi {
    fun create(merchantId: String, idempotencyKey: String, request: CreateInvoiceRequest): ApiResult<Invoice>
    fun list(merchantId: String, limit: Int = PAGE_SIZE, before: String? = null): ApiResult<InvoicePage>
    fun get(merchantId: String, invoiceId: String): ApiResult<Invoice>
    fun download(merchantId: String, invoiceId: String, kind: InvoiceFileKind): ApiResult<ByteArray>
    fun cancel(merchantId: String, invoiceId: String, request: CancelInvoiceRequest): ApiResult<Invoice>
    fun catalogs(etag: String?): ApiResult<CatalogResponse>

    companion object {
        const val PAGE_SIZE = 50
    }
}

/**
 * `422 RECEPTOR_INVALID {"fields":{"rfc":"INVALID_FORMAT",...}}`: the backend
 * sends an object of field name -> reason (`REQUIRED`, `INVALID_FORMAT`,
 * `NOT_ALLOWED_FOR_PERSONA_TYPE`...). A plain list is accepted too.
 */
fun ApiResult.Error.invalidReceptorFields(): List<String> =
    if (statusCode != 422 || errorCode != "RECEPTOR_INVALID") emptyList()
    else runCatching {
        when (val fields = KernoJson.parseToJsonElement(message).jsonObject["fields"]) {
            is JsonObject -> fields.keys.toList()
            is JsonArray -> fields.map { it.jsonPrimitive.content }
            else -> emptyList()
        }
    }.getOrNull().orEmpty()

class HttpInvoiceApi(baseUrl: String, private val client: OkHttpClient) : InvoiceApi {
    private val base = baseUrl.trimEnd('/').toHttpUrl()

    override fun create(merchantId: String, idempotencyKey: String, request: CreateInvoiceRequest): ApiResult<Invoice> {
        val body = KernoJson.encodeToString(CreateInvoiceRequest.serializer(), request)
        val req = Request.Builder().url(invoices(merchantId).build())
            .header("Idempotency-Key", idempotencyKey)
            .post(body.toJsonBody())
            .build()
        return client.executeJsonRequired(req, Invoice.serializer())
    }

    override fun list(merchantId: String, limit: Int, before: String?): ApiResult<InvoicePage> {
        val url = invoices(merchantId)
            .addQueryParameter("limit", limit.toString())
            .apply { if (!before.isNullOrBlank()) addQueryParameter("before", before) }
            .build()
        return client.executeJsonRequired(Request.Builder().url(url).get().build(), InvoicePage.serializer())
    }

    override fun get(merchantId: String, invoiceId: String): ApiResult<Invoice> =
        client.executeJsonRequired(
            Request.Builder().url(invoices(merchantId).addPathSegment(invoiceId).build()).get().build(),
            Invoice.serializer()
        )

    override fun download(merchantId: String, invoiceId: String, kind: InvoiceFileKind): ApiResult<ByteArray> {
        val url = invoices(merchantId).addPathSegment(invoiceId).addPathSegment(kind.pathSegment).build()
        return try {
            client.newCall(Request.Builder().url(url).get().build()).execute().use { response ->
                if (response.isSuccessful) {
                    val bytes = response.body?.bytes() ?: ByteArray(0)
                    if (bytes.isEmpty()) ApiResult.Error(response.code, "INVALID_RESPONSE", "empty body")
                    else ApiResult.Success(bytes, response.code)
                } else {
                    val text = response.body?.string().orEmpty()
                    ApiResult.Error(response.code, parseErrorCode(text, response.code), text)
                }
            }
        } catch (e: IOException) {
            ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, e.message.orEmpty())
        }
    }

    override fun cancel(merchantId: String, invoiceId: String, request: CancelInvoiceRequest): ApiResult<Invoice> {
        val body = KernoJson.encodeToString(CancelInvoiceRequest.serializer(), request)
        val url = invoices(merchantId).addPathSegment(invoiceId).addPathSegment("cancel").build()
        return client.executeJsonRequired(Request.Builder().url(url).post(body.toJsonBody()).build(), Invoice.serializer())
    }

    override fun catalogs(etag: String?): ApiResult<CatalogResponse> {
        val url = base.newBuilder().addPathSegments("api/v1/catalogs/sat")
            .addQueryParameter("names", "regimen_fiscal,uso_cfdi")
            .build()
        val req = Request.Builder().url(url).get().apply { if (etag != null) header("If-None-Match", etag) }.build()
        return try {
            client.newCall(req).execute().use { response ->
                val text = response.body?.string().orEmpty()
                when {
                    response.code == 304 -> ApiResult.Success(CatalogResponse(null, response.header("ETag") ?: etag), 304)
                    response.isSuccessful -> ApiResult.Success(
                        CatalogResponse(KernoJson.decodeFromString(SatCatalogs.serializer(), text), response.header("ETag")),
                        response.code
                    )
                    else -> ApiResult.Error(response.code, parseErrorCode(text, response.code), text)
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

    private fun invoices(merchantId: String): HttpUrl.Builder =
        base.newBuilder().addPathSegments("api/v1/merchants").addPathSegment(merchantId).addPathSegment("invoices")
}
