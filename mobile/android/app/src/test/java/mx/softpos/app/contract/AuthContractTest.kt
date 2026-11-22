package mx.softpos.app.contract

import mx.softpos.app.auth.EncryptedTokenStore
import mx.softpos.app.auth.FakeAuthApi
import mx.softpos.app.auth.HttpAuthApi
import mx.softpos.app.auth.KeyValueStore
import mx.softpos.app.auth.MembershipInfo
import mx.softpos.app.auth.SecretCipher
import mx.softpos.app.auth.SessionManager
import mx.softpos.app.auth.StoredSession
import mx.softpos.app.auth.authenticatedClient
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.KernoJson
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

/**
 * Memberships with merchant_name and the 401 of the auth gate (AND-41),
 * against bodies captured from pkg/api by TestCaptureAndroidFixtures.
 */
class AuthContractTest {
    private lateinit var server: MockWebServer
    private val plain = OkHttpClient()

    @BeforeEach fun setUp() { server = MockWebServer().apply { start() } }
    @AfterEach fun tearDown() { server.shutdown() }

    @Test
    fun refreshCarriesMerchantNamesForThePicker() {
        server.enqueue(MockResponse().setBody(Fixtures.read("auth_refresh_memberships_200.json")))
        val pair = (HttpAuthApi(server.url("/").toString(), plain).refresh("rt", null) as ApiResult.Success).data
        assertEquals(setOf("Tacos Don Memo", "Tortas La Güera"), pair.memberships.map { it.merchantName }.toSet())
        assertEquals(pair.memberships.map { it.merchantName }.toSet(), pair.memberships.map { it.displayName }.toSet())
        assertEquals("", pair.merchantId, "no tenant chosen yet: the picker is shown")
        // The names survive the encrypted session round trip.
        val kv = object : KeyValueStore {
            val m = HashMap<String, String>()
            override fun getString(key: String) = m[key]
            override fun putString(key: String, value: String) { m[key] = value }
            override fun remove(key: String) { m.remove(key) }
        }
        val identity = object : SecretCipher {
            override fun encrypt(plaintext: ByteArray) = plaintext.copyOf()
            override fun decrypt(ciphertext: ByteArray) = ciphertext.copyOf()
        }
        val store = EncryptedTokenStore(kv, identity)
        SessionManager(store, FakeAuthApi()).onLogin(pair)
        assertEquals(pair.memberships, store.load()?.memberships)
    }

    @Test
    fun blankOrMissingNameFallsBackToTheId() {
        assertNull(MembershipInfo("mer_1", "owner", merchantName = "  ").displayName)
        // Sessions saved before AND-41 have no merchant_name.
        val old = KernoJson.decodeFromString(
            StoredSession.serializer(),
            """{"access_token":"a","refresh_token":"r","access_expires_at_ms":1,"memberships":[{"merchant_id":"mer_1","role":"owner"}]}"""
        )
        assertNull(old.memberships.single().displayName)
    }

    /**
     * Since MVP-31 access tokens carry `aud`; a token minted before the deploy
     * (or for another audience) gets the gate's 401. The app refreshes once
     * (refresh tokens are opaque), retries with the new token and keeps the
     * refreshed memberships.
     */
    @Test
    fun rejectedAccessTokenRecoversThroughTheExistingRefresh() {
        val unauthorized = Fixtures.read("auth_unauthorized_401.json")
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/api/v1/auth/refresh" -> MockResponse().setBody(Fixtures.read("auth_refresh_memberships_200.json"))
                "/api/v1/merchants/mer_1/sales?limit=50" ->
                    if (request.getHeader("Authorization") == "Bearer access_token_captured") MockResponse().setBody("""{"items":[],"next_cursor":null}""")
                    else MockResponse().setResponseCode(401)
                        .setHeader("WWW-Authenticate", """Bearer realm="kerno", error="invalid_token"""")
                        .setHeader("Content-Type", "text/plain; charset=utf-8")
                        .setBody(unauthorized + "\n")
                else -> MockResponse().setResponseCode(404)
            }
        }
        val sessions = SessionManager(mx.softpos.app.auth.InMemoryTokenStore(), HttpAuthApi(server.url("/").toString(), plain))
        sessions.onLogin(FakeAuthApi.pair("pre_aud_token", "rt_1"))
        val sales = mx.softpos.app.payments.HttpSalesApi(server.url("/").toString(), authenticatedClient(plain, sessions))

        val r = sales.listSales("mer_1")

        assertEquals(200, (r as ApiResult.Success).statusCode)
        val calls = (1..server.requestCount).map { server.takeRequest().let { q -> q.path to q.getHeader("Authorization") } }
        assertEquals(
            listOf(
                "/api/v1/merchants/mer_1/sales?limit=50" to "Bearer pre_aud_token",
                "/api/v1/auth/refresh" to null,
                "/api/v1/merchants/mer_1/sales?limit=50" to "Bearer access_token_captured"
            ),
            calls
        )
        assertEquals("refresh_token_captured", sessions.current()?.refreshToken)
        assertEquals(2, sessions.current()?.memberships?.count { it.displayName != null })
    }
}
