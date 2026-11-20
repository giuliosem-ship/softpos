package mx.softpos.app.payments

import mx.softpos.app.core.network.ApiResult

/**
 * Pure in-memory stand-in for the backend card contract, used only by unit
 * tests. Mirrors docs/ACQUIRER_INTEGRATION.md: intents idempotent per key,
 * mock-acquirer scenarios by the last two digits (`51` declined), status
 * moved only by [simulate] (the DEV webhook player), refunds held then
 * applied on the simulated `payment.refunded`.
 */
class FakeCardBackend : CardPaymentsApi, DevCardSimulator {
    val payments = LinkedHashMap<String, CardPayment>()
    private val byKey = HashMap<String, Pair<String, Long>>()
    private val refunds = LinkedHashMap<String, CardRefund>()
    private val refundKeys = HashMap<String, String>()
    val intentKeys = mutableListOf<String>()
    val simulated = mutableListOf<String>()
    val cancels = mutableListOf<String>()
    var getCalls = 0

    /** Next createIntent errors (consumed one per call). */
    val intentErrors = ArrayDeque<ApiResult.Error>()
    /** Next refund errors (consumed one per call). */
    val refundErrors = ArrayDeque<ApiResult.Error>()
    /** When false, [simulate] fails like a non-DEV backend (404). */
    var devMode = true
    /** Rewrites the intent response (e.g. a replay of a tapped intent has no client_secret). */
    var intentResponse: (CardPaymentIntent) -> CardPaymentIntent = { it }
    /** Called on every [getPayment] (e.g. to advance a fake clock). */
    var onGet: () -> Unit = {}

    @Synchronized
    override fun createIntent(idempotencyKey: String, amountCents: Long, reference: String?): ApiResult<CardPaymentIntent> {
        intentKeys += idempotencyKey
        intentErrors.removeFirstOrNull()?.let { return it }
        byKey[idempotencyKey]?.let { (id, amount) ->
            if (amount != amountCents) return ApiResult.Error(409, "IDEMPOTENCY_KEY_REUSED", "")
            return ApiResult.Success(intentResponse(intentOf(payments.getValue(id))), 200)
        }
        val id = "cp_${payments.size + 1}"
        payments[id] = CardPayment(id, CardPaymentStatus.CREATED, amountCents, providerPaymentId = "mock_pi_$id")
        byKey[idempotencyKey] = id to amountCents
        return ApiResult.Success(intentResponse(intentOf(payments.getValue(id))), 201)
    }

    private fun intentOf(p: CardPayment) = CardPaymentIntent(
        cardPaymentId = p.cardPaymentId, status = p.status, amountCents = p.amountCents,
        provider = "mock", providerPaymentId = p.providerPaymentId, clientSecret = "mock_cs_${p.cardPaymentId}"
    )

    @Synchronized
    override fun getPayment(id: String): ApiResult<CardPayment> {
        getCalls++
        onGet()
        return payments[id]?.let { ApiResult.Success(it, 200) } ?: ApiResult.Error(404, "CARD_PAYMENT_NOT_FOUND", "")
    }

    @Synchronized
    override fun cancel(id: String): ApiResult<CancelResult> {
        cancels += id
        val p = payments[id] ?: return ApiResult.Error(404, "CARD_PAYMENT_NOT_FOUND", "")
        if (p.status != CardPaymentStatus.CREATED && p.status != CardPaymentStatus.AUTHORIZED) {
            return ApiResult.Error(409, "INVALID_STATE", "")
        }
        return ApiResult.Success(CancelResult(canceled = false, requested = true), 202)
    }

    @Synchronized
    override fun refund(id: String, idempotencyKey: String, amountCents: Long?): ApiResult<CardRefund> {
        refundErrors.removeFirstOrNull()?.let { return it }
        refundKeys[idempotencyKey]?.let { return ApiResult.Success(refunds.getValue(it), 202) }
        val p = payments[id] ?: return ApiResult.Error(404, "CARD_PAYMENT_NOT_FOUND", "")
        val held = refunds.values.filter { it.cardPaymentId == id && it.status == "PENDING" }.sumOf { it.amountCents }
        val remaining = p.refundableCents - held
        val amount = amountCents ?: remaining
        if (amount <= 0L || amount > remaining) return ApiResult.Error(400, "REFUND_EXCEEDS_REMAINING", "")
        val refund = CardRefund("cr_${refunds.size + 1}", id, amount, "PENDING", "mock_re_${refunds.size + 1}")
        refunds[refund.refundId] = refund
        refundKeys[idempotencyKey] = refund.refundId
        return ApiResult.Success(refund, 202)
    }

    @Synchronized
    override fun simulate(cardPaymentId: String, outcome: String, refundId: String?): ApiResult<Unit> {
        if (!devMode) return ApiResult.Error(404, "HTTP_404", "")
        simulated += "$outcome:$cardPaymentId"
        val p = payments[cardPaymentId] ?: return ApiResult.Error(404, "HTTP_404", "")
        payments[cardPaymentId] = when (outcome) {
            DevCardSimulator.TAP ->
                if (p.amountCents % 100L == 51L) p.copy(status = CardPaymentStatus.DECLINED, declineCode = "do_not_honor", final = true)
                else p.copy(status = CardPaymentStatus.CAPTURED, capturedCents = p.amountCents, final = true)
            DevCardSimulator.CANCEL -> p.copy(status = CardPaymentStatus.CANCELED, final = true)
            DevCardSimulator.REFUND -> {
                val r = refunds[refundId] ?: return ApiResult.Error(404, "REFUND_NOT_FOUND", "")
                refunds[r.refundId] = r.copy(status = "SUCCEEDED")
                val refunded = p.refundedCents + r.amountCents
                p.copy(
                    refundedCents = refunded,
                    status = if (refunded >= p.capturedCents) CardPaymentStatus.REFUNDED else CardPaymentStatus.PARTIALLY_REFUNDED
                )
            }
            else -> return ApiResult.Error(400, "INVALID_OUTCOME", "")
        }
        return ApiResult.Success(Unit, 200)
    }

    /** Marks a payment captured without going through a tap (fixtures). */
    @Synchronized
    fun seedCaptured(id: String, amountCents: Long, refunded: Long = 0L, chargeback: Long = 0L, status: String = CardPaymentStatus.CAPTURED) {
        payments[id] = CardPayment(
            id, status, amountCents, capturedCents = amountCents, refundedCents = refunded,
            chargebackCents = chargeback, final = true
        )
    }
}
