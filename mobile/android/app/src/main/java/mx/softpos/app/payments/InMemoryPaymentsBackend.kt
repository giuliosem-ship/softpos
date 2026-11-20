package mx.softpos.app.payments

import mx.softpos.app.core.network.ApiResult
import java.time.Instant
import java.util.UUID

/**
 * Debug-only stand-in for the provisional card-intent and sales endpoints,
 * so the charge → history flow can be demoed before the backend ships them.
 * Idempotent on the `Idempotency-Key`, like the real API must be.
 */
class InMemoryPaymentsBackend(
    private val clock: () -> Instant = Instant::now
) : CardPaymentsApi, SalesApi {
    private val intents = LinkedHashMap<String, CardPaymentIntent>()
    private val byKey = HashMap<String, String>()
    private val createdAt = HashMap<String, Instant>()

    @Synchronized
    override fun createIntent(idempotencyKey: String, amountCents: Long): ApiResult<CardPaymentIntent> {
        byKey[idempotencyKey]?.let { return ApiResult.Success(intents.getValue(it), 200) }
        if (amountCents <= 0L) return ApiResult.Error(400, "INVALID_AMOUNT", "")
        val id = "cpi_" + UUID.randomUUID().toString().replace("-", "").take(16)
        val intent = CardPaymentIntent(id, "secret_$id", CardPaymentStatus.REQUIRES_TAP, amountCents)
        intents[id] = intent
        byKey[idempotencyKey] = id
        createdAt[id] = clock()
        return ApiResult.Success(intent, 201)
    }

    @Synchronized
    override fun getPayment(id: String): ApiResult<CardPaymentIntent> =
        intents[id]?.let { ApiResult.Success(it, 200) } ?: ApiResult.Error(404, "NOT_FOUND", "")

    /** Plays the processor webhook after the (mock) tap. */
    @Synchronized
    fun onTapOutcome(intentId: String, outcome: TapToPayOutcome) {
        val intent = intents[intentId] ?: return
        val status = when (outcome) {
            is TapToPayOutcome.Approved -> CardPaymentStatus.SUCCEEDED
            else -> CardPaymentStatus.FAILED
        }
        intents[intentId] = intent.copy(status = status)
    }

    @Synchronized
    override fun listSales(merchantId: String, limit: Int): ApiResult<SalesPage> {
        val sales = intents.values.reversed().take(limit).map {
            Sale(
                id = it.id,
                amountCents = it.amountCents,
                method = SaleMethod.CARD,
                status = it.status,
                createdAt = createdAt[it.id]?.toString().orEmpty()
            )
        }
        return ApiResult.Success(SalesPage(sales), 200)
    }
}
