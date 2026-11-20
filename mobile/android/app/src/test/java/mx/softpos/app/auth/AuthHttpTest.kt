package mx.softpos.app.auth

import mx.softpos.app.core.network.ApiResult
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class AuthHttpTest {
    private lateinit var server: MockWebServer
    private val plain = OkHttpClient()

    private val pairJson = """{"access_token":"at_2","token_type":"Bearer","expires_in":900,
        |"refresh_token":"rt_2","refresh_expires_in":2592000,"user_id":"usr_1",
        |"merchant_id":"mer_1","role":"owner","memberships":[]}""".trimMargin()

    @BeforeEach fun setUp() { server = MockWebServer().apply { start() } }
    @AfterEach fun tearDown() { server.shutdown() }

    private fun baseUrl() = server.url("/").toString()

    @Test
    fun otpRequestAndVerifyFollowTheContract() {
        server.enqueue(MockResponse().setResponseCode(202).setBody("""{"status":"OTP_SENT"}"""))
        server.enqueue(MockResponse().setBody(pairJson))
        val api = HttpAuthApi(baseUrl(), plain)

        assertTrue(api.requestOtp("+525512345678") is ApiResult.Success)
        val verify = api.verifyOtp("+525512345678", "123456") as ApiResult.Success

        assertEquals("at_2", verify.data.accessToken)
        assertEquals(900L, verify.data.expiresInSeconds)
        val r1 = server.takeRequest()
        assertEquals("/api/v1/auth/otp/request", r1.path)
        assertEquals("""{"phone":"+525512345678"}""", r1.body.readUtf8())
        val r2 = server.takeRequest()
        assertEquals("/api/v1/auth/otp/verify", r2.path)
        assertEquals("""{"phone":"+525512345678","code":"123456"}""", r2.body.readUtf8())
    }

    @Test
    fun refreshSwitchesMerchantAndReadsMemberships() {
        server.enqueue(
            MockResponse().setBody(
                """{"access_token":"at_3","expires_in":900,"refresh_token":"rt_3","user_id":"usr_1",
                   "merchant_id":"mer_2","role":"cashier","memberships":[
                   {"user_id":"usr_1","merchant_id":"mer_1","role":"owner","created_at":"2026-01-01T00:00:00Z"},
                   {"user_id":"usr_1","merchant_id":"mer_2","role":"cashier","created_at":"2026-01-01T00:00:00Z"}]}"""
            )
        )
        val pair = (HttpAuthApi(baseUrl(), plain).refresh("rt_2", "mer_2") as ApiResult.Success).data
        assertEquals(listOf(MembershipInfo("mer_1", "owner"), MembershipInfo("mer_2", "cashier")), pair.memberships)
        assertEquals(2, pair.toStoredSession(0L).memberships.size)
        assertEquals("""{"refresh_token":"rt_2","merchant_id":"mer_2"}""", server.takeRequest().body.readUtf8())
    }

    @Test
    fun backendErrorCodeIsSurfaced() {
        server.enqueue(MockResponse().setResponseCode(429).setBody("""{"error":"OTP_RATE_LIMITED"}"""))
        val r = HttpAuthApi(baseUrl(), plain).requestOtp("+525512345678") as ApiResult.Error
        assertEquals(429, r.statusCode)
        assertEquals("OTP_RATE_LIMITED", r.errorCode)
    }

    @Test
    fun unauthorizedTriggersOneRefreshAndRetryWithNewBearer() {
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/api/v1/auth/refresh" -> MockResponse().setBody(pairJson)
                "/api/v1/merchants/mer_1/balances" ->
                    if (request.getHeader("Authorization") == "Bearer at_2") MockResponse().setBody("{}")
                    else MockResponse().setResponseCode(401).setBody("""{"error":"UNAUTHORIZED"}""")
                else -> MockResponse().setResponseCode(404)
            }
        }
        val sessions = SessionManager(InMemoryTokenStore(), HttpAuthApi(baseUrl(), plain))
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        val client = authenticatedClient(plain, sessions)

        val response = client.newCall(Request.Builder().url(server.url("/api/v1/merchants/mer_1/balances")).build()).execute()

        assertEquals(200, response.code)
        response.close()
        val paths = (1..server.requestCount).map { server.takeRequest().let { r -> r.path to r.getHeader("Authorization") } }
        assertEquals(
            listOf(
                "/api/v1/merchants/mer_1/balances" to "Bearer at_1",
                "/api/v1/auth/refresh" to null,
                "/api/v1/merchants/mer_1/balances" to "Bearer at_2"
            ),
            paths
        )
        assertEquals("rt_2", sessions.current()?.refreshToken)
    }

    @Test
    fun persistentUnauthorizedDoesNotLoop() {
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/api/v1/auth/refresh" -> MockResponse().setBody(pairJson)
                else -> MockResponse().setResponseCode(401)
            }
        }
        val sessions = SessionManager(InMemoryTokenStore(), HttpAuthApi(baseUrl(), plain))
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        val response = authenticatedClient(plain, sessions)
            .newCall(Request.Builder().url(server.url("/x")).build()).execute()
        assertEquals(401, response.code)
        response.close()
        assertEquals(3, server.requestCount)
    }

    @Test
    fun expiringTokenIsRefreshedProactively() {
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/api/v1/auth/refresh" -> MockResponse().setBody(pairJson)
                else -> MockResponse().setBody("{}")
            }
        }
        var now = 0L
        val sessions = SessionManager(InMemoryTokenStore(), HttpAuthApi(baseUrl(), plain)) { now }
        sessions.onLogin(FakeAuthApi.pair("at_1", "rt_1"))
        now = 899_000L
        authenticatedClient(plain, sessions)
            .newCall(Request.Builder().url(server.url("/x")).build()).execute().close()

        assertEquals("/api/v1/auth/refresh", server.takeRequest().path)
        assertEquals("Bearer at_2", server.takeRequest().getHeader("Authorization"))
    }
}
