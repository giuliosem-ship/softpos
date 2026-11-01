package mx.softpos.app.core.network

import kotlinx.coroutines.runBlocking
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import java.io.IOException

class PlayIntegrityClientTest {

    @Test
    fun `nonce isFresh validates 300 second freshness window`() {
        val now = 1_000_000_000L
        val freshNonce = AttestationNonce(
            nonce = "BASE64_NONCE_VAL_01",
            issuedAtTimestampMs = now - 50_000L, // 50s old
            ttlSeconds = 300L
        )
        assertTrue(freshNonce.isFresh(currentTimeMs = now))

        val expiredNonce = AttestationNonce(
            nonce = "BASE64_NONCE_VAL_02",
            issuedAtTimestampMs = now - 350_000L, // 350s old
            ttlSeconds = 300L
        )
        assertFalse(expiredNonce.isFresh(currentTimeMs = now))
    }

    @Test
    fun `handshake obtains token and formats X-Play-Integrity-Token header`() = runBlocking {
        val now = System.currentTimeMillis()
        val mockNonceProvider = NonceProvider {
            AttestationNonce(nonce = "NONCE_ABC_123", issuedAtTimestampMs = now, ttlSeconds = 300L)
        }
        val mockIntegrityProvider = PlayIntegrityTokenProvider { nonce ->
            "JWT_INTEGRITY_TOKEN_FOR_$nonce"
        }

        val client = PlayIntegrityClient(
            nonceProvider = mockNonceProvider,
            integrityProvider = mockIntegrityProvider
        )

        val token = client.obtainIntegrityToken()
        assertEquals("JWT_INTEGRITY_TOKEN_FOR_NONCE_ABC_123", token)

        val headers = mutableMapOf<String, String>()
        client.addPlayIntegrityHeaders(headers, token)
        assertEquals("JWT_INTEGRITY_TOKEN_FOR_NONCE_ABC_123", headers["X-Play-Integrity-Token"])
    }

    @Test
    fun `handshake rejects expired nonce before calling Google Play provider`() {
        val now = System.currentTimeMillis()
        val expiredNonceProvider = NonceProvider {
            AttestationNonce(nonce = "EXPIRED_NONCE", issuedAtTimestampMs = now - 400_000L, ttlSeconds = 300L)
        }
        var integrityCalled = false
        val mockIntegrityProvider = PlayIntegrityTokenProvider { _ ->
            integrityCalled = true
            "TOKEN"
        }

        val client = PlayIntegrityClient(
            nonceProvider = expiredNonceProvider,
            integrityProvider = mockIntegrityProvider
        )

        assertThrows(ExpiredNonceException::class.java) {
            runBlocking { client.obtainIntegrityToken() }
        }
        assertFalse(integrityCalled)
    }

    @Test
    fun `handshake recovers with retry on transient network failure`() = runBlocking {
        val now = System.currentTimeMillis()
        val mockNonceProvider = NonceProvider {
            AttestationNonce(nonce = "RETRY_NONCE", issuedAtTimestampMs = now, ttlSeconds = 300L)
        }

        var attempts = 0
        val flakingIntegrityProvider = PlayIntegrityTokenProvider { nonce ->
            attempts++
            if (attempts < 3) {
                throw IOException("Google Play Integrity transient timeout")
            }
            "RECOVERED_TOKEN_$nonce"
        }

        val client = PlayIntegrityClient(
            nonceProvider = mockNonceProvider,
            integrityProvider = flakingIntegrityProvider,
            maxRetries = 3,
            initialBackoffMs = 10L // Fast for unit tests
        )

        val token = client.obtainIntegrityToken()
        assertEquals("RECOVERED_TOKEN_RETRY_NONCE", token)
        assertEquals(3, attempts)
    }

    @Test
    fun `handshake exhausts retries and throws exception after max attempts`() {
        val now = System.currentTimeMillis()
        val mockNonceProvider = NonceProvider {
            AttestationNonce(nonce = "FAIL_NONCE", issuedAtTimestampMs = now, ttlSeconds = 300L)
        }
        val failingProvider = PlayIntegrityTokenProvider { _ ->
            throw IOException("Permanent Play Store service failure")
        }

        val client = PlayIntegrityClient(
            nonceProvider = mockNonceProvider,
            integrityProvider = failingProvider,
            maxRetries = 3,
            initialBackoffMs = 10L
        )

        assertThrows(IOException::class.java) {
            runBlocking { client.obtainIntegrityToken() }
        }
    }
}
