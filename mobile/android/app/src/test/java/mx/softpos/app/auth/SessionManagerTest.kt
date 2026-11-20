package mx.softpos.app.auth

import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_CODE
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit

class SessionManagerTest {
    private var now = 1_000_000L
    private val api = FakeAuthApi()
    private val store = InMemoryTokenStore()
    private val sessions = SessionManager(store, api) { now }

    @Test
    fun loginPersistsSessionWithAbsoluteExpiry() {
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        val s = store.load()
        assertNotNull(s)
        s!!
        assertEquals(now + 900_000L, s.accessExpiresAtMs)
        assertEquals("mer_1", sessions.current()?.merchantId)
    }

    @Test
    fun expiringDetection() {
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        assertTrue(!sessions.isAccessTokenExpiring())
        now += 880_000L
        assertTrue(sessions.isAccessTokenExpiring())
    }

    @Test
    fun refreshRotatesTokens() {
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        assertEquals("at_new", sessions.refreshAfterUnauthorized("at_1"))
        assertEquals("rt_new", store.load()?.refreshToken)
    }

    @Test
    fun concurrentRefreshesAreSingleFlight() {
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        api.refreshDelayMs = 50
        val pool = Executors.newFixedThreadPool(8)
        val start = CountDownLatch(1)
        val results = (1..8).map {
            pool.submit<String?> {
                start.await()
                sessions.refreshAfterUnauthorized("at_1")
            }
        }
        start.countDown()
        val tokens = results.map { it.get(5, TimeUnit.SECONDS) }
        pool.shutdown()

        assertEquals(1, api.refreshCalls.get(), "the refresh token must be rotated exactly once")
        assertTrue(tokens.all { it == "at_new" })
    }

    @Test
    fun rejectedRefreshEndsSession() {
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        api.refreshResult = { ApiResult.Error(401, "REFRESH_TOKEN_REUSED", "") }
        assertNull(sessions.refreshAfterUnauthorized("at_1"))
        assertNull(sessions.session.value)
        assertNull(store.load())
    }

    @Test
    fun networkFailureDuringRefreshKeepsSession() {
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        api.refreshResult = { ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, "") }
        assertNull(sessions.refreshAfterUnauthorized("at_1"))
        assertEquals("at_1", sessions.current()?.accessToken)
    }

    @Test
    fun logoutRevokesFamilyAndWipesDevice() {
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        sessions.logout()
        assertEquals(listOf("rt_1"), api.logoutTokens)
        assertNull(store.load())
        assertNull(sessions.session.value)
    }

    @Test
    fun sessionIsRestoredFromStoreAtStartup() {
        store.save(StoredSession("at_saved", "rt_saved", now + 1000, merchantId = "mer_9"))
        assertEquals("mer_9", SessionManager(store, api) { now }.current()?.merchantId)
    }
}
