package mx.softpos.app.fiscal

import mx.softpos.app.core.network.ApiResult

val TEST_CATALOGS = SatCatalogs(
    regimenFiscal = listOf(
        RegimenFiscal("601", "General de Ley Personas Morales", personaFisica = false, personaMoral = true),
        RegimenFiscal("612", "Personas Físicas con Actividades Empresariales y Profesionales", personaFisica = true, personaMoral = false),
        RegimenFiscal("626", "Régimen Simplificado de Confianza", personaFisica = true, personaMoral = true)
    ),
    usoCfdi = listOf(
        UsoCfdi("G01", "Adquisición de mercancías", listOf("601", "612", "626")),
        UsoCfdi("G03", "Gastos en general", listOf("601", "612", "626")),
        UsoCfdi("D01", "Honorarios médicos", listOf("612"))
    )
)

fun stamped(id: String = "inv_1", saleIds: List<String> = listOf("s1")) = Invoice(
    invoiceId = id, status = InvoiceStatus.STAMPED, uuid = "6F1B2C3D-0000-4A5B-8C9D-112233445566",
    serie = "K", folio = "1", totalCents = 11_600, subtotalCents = 10_000, ivaCents = 1_600,
    saleIds = saleIds, createdAt = "2026-11-20T10:00:00Z", stampedAt = "2026-11-20T10:00:02Z"
)

/** Scripted invoice backend: queue results per call; records requests. */
class FakeInvoiceApi : InvoiceApi {
    val createResults = ArrayDeque<ApiResult<Invoice>>()
    val creates = mutableListOf<Pair<String, CreateInvoiceRequest>>()
    val pages = mutableMapOf<String?, ApiResult<InvoicePage>>()
    var invoice: ApiResult<Invoice> = ApiResult.Success(stamped(), 200)
    var file: ApiResult<ByteArray> = ApiResult.Success(byteArrayOf(1, 2, 3), 200)
    var lastFile: ByteArray? = null
    var cancelResult: ApiResult<Invoice>? = null
    val cancels = mutableListOf<CancelInvoiceRequest>()
    var catalogResult: ApiResult<CatalogResponse> = ApiResult.Success(CatalogResponse(TEST_CATALOGS, "\"v1\""), 200)
    val catalogEtags = mutableListOf<String?>()

    override fun create(merchantId: String, idempotencyKey: String, request: CreateInvoiceRequest): ApiResult<Invoice> {
        creates += idempotencyKey to request
        return createResults.removeFirstOrNull() ?: ApiResult.Success(stamped(saleIds = request.saleIds), 201)
    }

    override fun list(merchantId: String, limit: Int, before: String?) =
        pages[before] ?: ApiResult.Success(InvoicePage(), 200)

    override fun get(merchantId: String, invoiceId: String) = invoice

    override fun download(merchantId: String, invoiceId: String, kind: InvoiceFileKind): ApiResult<ByteArray> {
        val r = file
        if (r is ApiResult.Success) {
            val copy = r.data.copyOf()
            lastFile = copy
            return ApiResult.Success(copy, 200)
        }
        return r
    }

    override fun cancel(merchantId: String, invoiceId: String, request: CancelInvoiceRequest): ApiResult<Invoice> {
        cancels += request
        return cancelResult ?: ApiResult.Success(
            (invoice as ApiResult.Success).data.copy(status = InvoiceStatus.CANCEL_REQUESTED), 200
        )
    }

    override fun catalogs(etag: String?): ApiResult<CatalogResponse> {
        catalogEtags += etag
        return catalogResult
    }
}
