package mx.softpos.app

import android.content.Context
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import mx.softpos.app.auth.AuthApi
import mx.softpos.app.auth.HttpAuthApi
import mx.softpos.app.auth.MembershipInfo
import mx.softpos.app.auth.SessionManager
import mx.softpos.app.auth.authenticatedClient
import mx.softpos.app.auth.keystoreTokenStore
import mx.softpos.app.merchant.HttpMerchantApi
import mx.softpos.app.merchant.MerchantApi
import mx.softpos.app.payments.CardPaymentsApi
import mx.softpos.app.payments.DevCardSimulator
import mx.softpos.app.payments.HttpCardPaymentsApi
import mx.softpos.app.payments.HttpDevCardSimulator
import mx.softpos.app.payments.HttpSalesApi
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

    /** Real `/api/v1/card-payments/...` contract (docs/ACQUIRER_INTEGRATION.md) in every build. */
    val cardPaymentsApi: CardPaymentsApi by lazy { HttpCardPaymentsApi(baseUrl, apiClient) }

    val salesApi: SalesApi by lazy { HttpSalesApi(baseUrl, apiClient) }

    /**
     * Debug only: the backend DEV_MODE endpoint that plays the processor
     * webhooks. Null in release, so nothing can fake a processor event there.
     */
    val devCardSimulator: DevCardSimulator? by lazy {
        if (BuildConfig.DEV_TAP_SIMULATOR) HttpDevCardSimulator(baseUrl, apiClient) else null
    }

    /** Simulated tap until the processor SDK is integrated; card payments off in release. */
    val tapToPay: TapToPayProvider by lazy {
        devCardSimulator?.let { MockTapToPayProvider(it) } ?: UnavailableTapToPayProvider
    }

    /** Selected tenant of the current session (null if none yet). */
    fun currentMerchantId(): String? = sessionManager.current()?.merchantId?.ifBlank { null }

    fun currentRole(): String = sessionManager.current()?.role.orEmpty()

    fun memberships(): List<MembershipInfo> = sessionManager.current()?.memberships.orEmpty()

    /** Process-wide scope for work that must outlive a screen (logout). */
    val appScope: CoroutineScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    /** Local wipe is immediate; the server revocation runs in [appScope]. */
    fun logout() {
        appScope.launch { sessionManager.logout() }
    }
}
