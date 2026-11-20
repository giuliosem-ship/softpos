package mx.softpos.app.auth

import okhttp3.Authenticator
import okhttp3.Interceptor
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.Route

private const val AUTHORIZATION = "Authorization"
private const val BEARER = "Bearer "

/** Adds the access token, refreshing first if it is about to expire. */
class AuthInterceptor(private val sessions: SessionManager) : Interceptor {
    override fun intercept(chain: Interceptor.Chain): Response {
        var token = sessions.current()?.accessToken
        if (token != null && sessions.isAccessTokenExpiring()) {
            token = sessions.refreshAfterUnauthorized(token) ?: token
        }
        val request = if (token != null) {
            chain.request().newBuilder().header(AUTHORIZATION, BEARER + token).build()
        } else {
            chain.request()
        }
        return chain.proceed(request)
    }
}

/** On 401: one single-flight refresh, then one retry. Never loops. */
class TokenAuthenticator(private val sessions: SessionManager) : Authenticator {
    override fun authenticate(route: Route?, response: Response): Request? {
        val sent = response.request.header(AUTHORIZATION) ?: return null
        if (response.priorResponse != null) return null
        val fresh = sessions.refreshAfterUnauthorized(sent.removePrefix(BEARER)) ?: return null
        return response.request.newBuilder().header(AUTHORIZATION, BEARER + fresh).build()
    }
}

/** Client for protected routes, derived from the plain [base] client. */
fun authenticatedClient(base: OkHttpClient, sessions: SessionManager): OkHttpClient =
    base.newBuilder()
        .addInterceptor(AuthInterceptor(sessions))
        .authenticator(TokenAuthenticator(sessions))
        .build()
