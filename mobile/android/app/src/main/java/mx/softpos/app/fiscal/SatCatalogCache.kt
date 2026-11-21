package mx.softpos.app.fiscal

import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.ui.onboarding.KybOnboardingPresenter

/**
 * In-memory cache of the SAT catalogs (one per process, nothing on disk).
 * Fresh for [ttlMillis]; after that it revalidates with `If-None-Match` and a
 * `304` just renews the copy. If the refresh fails, a stale copy is still
 * served (catalogs change a few times a year).
 */
class SatCatalogCache(
    private val api: InvoiceApi,
    private val ttlMillis: Long = DEFAULT_TTL_MILLIS,
    private val clock: () -> Long = System::currentTimeMillis
) {
    private var cached: SatCatalogs? = null
    private var etag: String? = null
    private var fetchedAt: Long = 0L

    @Synchronized
    fun get(): ApiResult<SatCatalogs> {
        val now = clock()
        cached?.let { if (now - fetchedAt < ttlMillis) return ApiResult.Success(it, 200) }
        return when (val r = api.catalogs(etag.takeIf { cached != null })) {
            is ApiResult.Success -> {
                val fresh = r.data.catalogs ?: cached
                if (fresh == null) {
                    // 304 without a copy (should not happen: no If-None-Match was sent).
                    ApiResult.Error(r.statusCode, "INVALID_RESPONSE", "not modified without cache")
                } else {
                    cached = fresh
                    etag = r.data.etag ?: etag
                    fetchedAt = now
                    ApiResult.Success(fresh, r.statusCode)
                }
            }
            is ApiResult.Error -> cached?.let { ApiResult.Success(it, 200) } ?: r
        }
    }

    companion object {
        const val DEFAULT_TTL_MILLIS = 12L * 60 * 60 * 1000
    }
}

enum class Persona { FISICA, MORAL }

/** Client-side CFDI 4.0 receptor checks (the backend and the PAC re-validate). */
object ReceptorValidator {
    private val POSTAL_CODE = Regex("^\\d{5}$")
    private val EMAIL = Regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]{2,}$")
    private val UUID = Regex("^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$")

    fun normalizeRfc(rfc: String): String = rfc.trim().uppercase()

    fun isValidRfc(rfc: String): Boolean = KybOnboardingPresenter.isValidRfc(rfc)

    /** 13 characters = persona física, 12 = persona moral; null while the RFC is not valid. */
    fun persona(rfc: String): Persona? {
        val n = normalizeRfc(rfc)
        if (!isValidRfc(n)) return null
        return if (n.length == 13) Persona.FISICA else Persona.MORAL
    }

    fun isValidPostalCode(cp: String) = POSTAL_CODE.matches(cp.trim())
    fun isValidEmail(email: String) = EMAIL.matches(email.trim())
    fun isValidUuid(value: String) = UUID.matches(value.trim())

    fun regimenesFor(catalogs: SatCatalogs, persona: Persona?): List<RegimenFiscal> = when (persona) {
        null -> emptyList()
        Persona.FISICA -> catalogs.regimenFiscal.filter { it.personaFisica }
        Persona.MORAL -> catalogs.regimenFiscal.filter { it.personaMoral }
    }

    fun usosFor(catalogs: SatCatalogs, regimen: String?): List<UsoCfdi> =
        if (regimen.isNullOrBlank()) emptyList() else catalogs.usoCfdi.filter { regimen in it.regimenes }
}
