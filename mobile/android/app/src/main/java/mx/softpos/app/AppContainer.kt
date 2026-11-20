package mx.softpos.app

import android.content.Context
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import mx.softpos.app.auth.AuthApi
import mx.softpos.app.auth.HttpAuthApi
import mx.softpos.app.auth.SessionManager
import mx.softpos.app.auth.authenticatedClient
import mx.softpos.app.auth.keystoreTokenStore
import mx.softpos.app.merchant.HttpMerchantApi
import mx.softpos.app.merchant.MerchantApi
import mx.softpos.app.payments.CardPaymentsApi
import mx.softpos.app.payments.HttpCardPaymentsApi
import mx.softpos.app.payments.HttpSalesApi
import mx.softpos.app.payments.InMemoryPaymentsBackend
import mx.softpos.app.payments.MockTapToPayProvider
import mx.softpos.app.payments.SalesApi
import mx.softpos.app.payments.TapToPayProvider
import mx.softpos.app.payments.UnavailableTapToPayProvider
import okhttp3.OkHttpClient
import java.util.concurrent.TimeUnit

/**
 * Manual dependency injection: one instance per process, built lazily by
 * [KernoApplication]. Swap implementations here (fakes, real SDKs).
 */
class AppContainer(context: Context, baseUrl: String = BuildConfig.API_BASE_URL) {
    private val appContext = context.applicationContext

    /** Plain client: auth endpoints (no bearer, no authenticator). */
    private val baseClient: OkHttpClient = OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .callTimeout(45, TimeUnit.SECONDS)
        .build()

    val authApi: AuthApi = HttpAuthApi(baseUrl, baseClient)

    val sessionManager: SessionManager by lazy { SessionManager(keystoreTokenStore(appContext), authApi) }

    /** Client for tenant routes: bearer + single-flight refresh on 401. */
    val apiClient: OkHttpClient by lazy { authenticatedClient(baseClient, sessionManager) }

    val merchantApi: MerchantApi by lazy { HttpMerchantApi(baseUrl, apiClient) }

    private val mockPayments by lazy { InMemoryPaymentsBackend() }

    /** PROVISIONAL endpoints (docs/ANDROID_APP.md); in-memory in debug. */
    val cardPaymentsApi: CardPaymentsApi by lazy {
        if (BuildConfig.MOCK_PAYMENTS) mockPayments else HttpCardPaymentsApi(baseUrl, apiClient)
    }

    val salesApi: SalesApi by lazy {
        if (BuildConfig.MOCK_PAYMENTS) mockPayments else HttpSalesApi(baseUrl, apiClient)
    }

    /** Mock until the processor SDK is integrated; never fakes approvals in release. */
    val tapToPay: TapToPayProvider by lazy {
        if (BuildConfig.MOCK_PAYMENTS) MockTapToPayProvider(onOutcome = mockPayments::onTapOutcome)
        else UnavailableTapToPayProvider
    }

    /** Selected tenant of the current session (null if none yet). */
    fun currentMerchantId(): String? = sessionManager.current()?.merchantId?.ifBlank { null }

    fun currentRole(): String = sessionManager.current()?.role.orEmpty()

    /** Process-wide scope for work that must outlive a screen (logout). */
    val appScope: CoroutineScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    /** Local wipe is immediate; the server revocation runs in [appScope]. */
    fun logout() {
        appScope.launch { sessionManager.logout() }
    }
}
