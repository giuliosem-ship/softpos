package mx.softpos.app.fiscal

import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_CODE
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class SatCatalogCacheTest {
    private val api = FakeInvoiceApi()
    private var now = 0L
    private val cache = SatCatalogCache(api, ttlMillis = 1_000) { now }

    @Test
    fun freshCopyIsServedWithoutCallingTheBackend() {
        assertEquals(TEST_CATALOGS, (cache.get() as ApiResult.Success).data)
        now = 999
        cache.get()
        assertEquals(listOf<String?>(null), api.catalogEtags)
    }

    @Test
    fun staleCopyRevalidatesWithEtagAnd304KeepsIt() {
        cache.get()
        now = 1_000
        api.catalogResult = ApiResult.Success(CatalogResponse(null, "\"v1\""), 304)
        assertEquals(TEST_CATALOGS, (cache.get() as ApiResult.Success).data)
        assertEquals(listOf(null, "\"v1\""), api.catalogEtags)
        now = 1_500
        cache.get()
        assertEquals(2, api.catalogEtags.size, "the 304 renewed the TTL")
    }

    @Test
    fun failedRefreshServesStaleButNoCacheIsAnError() {
        api.catalogResult = ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, "")
        assertTrue(cache.get() is ApiResult.Error)
        api.catalogResult = ApiResult.Success(CatalogResponse(TEST_CATALOGS, null), 200)
        cache.get()
        now = 5_000
        api.catalogResult = ApiResult.Error(503, "HTTP_503", "")
        assertEquals(TEST_CATALOGS, (cache.get() as ApiResult.Success).data)
    }

    @Test
    fun receptorRulesFilterByPersonaAndRegimen() {
        assertEquals(Persona.FISICA, ReceptorValidator.persona(" test010203ab1 "))
        assertEquals(Persona.MORAL, ReceptorValidator.persona("ABC010203AB1"))
        assertNull(ReceptorValidator.persona("ABC0102"))
        assertEquals(listOf("612", "626"), ReceptorValidator.regimenesFor(TEST_CATALOGS, Persona.FISICA).map { it.code })
        assertEquals(listOf("601", "626"), ReceptorValidator.regimenesFor(TEST_CATALOGS, Persona.MORAL).map { it.code })
        assertEquals(listOf("G01", "G03"), ReceptorValidator.usosFor(TEST_CATALOGS, "626").map { it.code })
        assertEquals(listOf("G01", "G03", "D01"), ReceptorValidator.usosFor(TEST_CATALOGS, "612").map { it.code })
        assertTrue(ReceptorValidator.isValidUuid("6f1b2c3d-0000-4a5b-8c9d-112233445566"))
    }
}
