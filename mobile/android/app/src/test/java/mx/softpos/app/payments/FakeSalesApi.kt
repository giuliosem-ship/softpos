package mx.softpos.app.payments

import mx.softpos.app.core.network.ApiResult

/** In-memory [SalesApi] for ViewModel tests: details by id, scripted receipt answers. */
class FakeSalesApi(
    val details: MutableMap<String, Sale> = mutableMapOf(),
    var page: SalesPage = SalesPage()
) : SalesApi {
    val detailCalls = mutableListOf<Pair<String, String>>()
    val sends = mutableListOf<Pair<String, SendReceiptRequest>>()
    /** Next sendReceipt answers (consumed one per call); empty = 200 with every channel sent. */
    val sendAnswers = ArrayDeque<ApiResult<SendReceiptResponse>>()

    override fun listSales(merchantId: String, limit: Int, before: String?): ApiResult<SalesPage> = ApiResult.Success(page, 200)

    override fun getSale(merchantId: String, saleId: String): ApiResult<Sale> {
        detailCalls += merchantId to saleId
        return details[saleId]?.let { ApiResult.Success(it, 200) } ?: ApiResult.Error(404, "SALE_NOT_FOUND", """{"error":"SALE_NOT_FOUND"}""")
    }

    override fun sendReceipt(merchantId: String, saleId: String, request: SendReceiptRequest): ApiResult<SendReceiptResponse> {
        sends += saleId to request
        return sendAnswers.removeFirstOrNull() ?: ApiResult.Success(
            SendReceiptResponse(
                receiptUrl = details[saleId]?.receiptUrl.orEmpty(),
                expiresAt = "2027-02-20T04:28:54Z",
                sent = buildMap {
                    if (request.email != null) put("email", true)
                    if (request.phone != null) put("sms", true)
                }
            ),
            200
        )
    }
}
