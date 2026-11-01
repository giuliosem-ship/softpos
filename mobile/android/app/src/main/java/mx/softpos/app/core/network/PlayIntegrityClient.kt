package mx.softpos.app.core.network

import kotlinx.coroutines.delay
import java.io.IOException

class ExpiredNonceException(message: String) : RuntimeException(message)

data class AttestationNonce(
    val nonce: String,
    val issuedAtTimestampMs: Long,
    val ttlSeconds: Long = 300L
) {
    fun isFresh(currentTimeMs: Long = System.currentTimeMillis()): Boolean {
        val ageSeconds = (currentTimeMs - issuedAtTimestampMs) / 1000L
        return ageSeconds in 0..ttlSeconds
    }
}

fun interface NonceProvider {
    suspend fun fetchNonce(): AttestationNonce
}

fun interface PlayIntegrityTokenProvider {
    suspend fun requestIntegrityToken(nonce: String): String
}

class PlayIntegrityClient(
    private val nonceProvider: NonceProvider,
    private val integrityProvider: PlayIntegrityTokenProvider,
    private val maxRetries: Int = 3,
    private val initialBackoffMs: Long = 500L
) {
    suspend fun obtainIntegrityToken(): String {
        val attestationNonce = nonceProvider.fetchNonce()
        if (!attestationNonce.isFresh()) {
            throw ExpiredNonceException("Attestation nonce is expired or invalid (age > ${attestationNonce.ttlSeconds}s)")
        }

        var attempt = 0
        var currentBackoff = initialBackoffMs
        var lastException: Exception? = null

        while (attempt < maxRetries) {
            attempt++
            try {
                return integrityProvider.requestIntegrityToken(attestationNonce.nonce)
            } catch (e: Exception) {
                lastException = e
                if (attempt >= maxRetries) {
                    break
                }
                delay(currentBackoff)
                currentBackoff *= 2
            }
        }

        throw lastException ?: IOException("Failed to obtain Google Play Integrity token after $maxRetries attempts")
    }

    fun addPlayIntegrityHeaders(headers: MutableMap<String, String>, token: String) {
        headers["X-Play-Integrity-Token"] = token
    }
}
