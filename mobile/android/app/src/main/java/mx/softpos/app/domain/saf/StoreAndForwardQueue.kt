package mx.softpos.app.domain.saf

import java.security.SecureRandom
import java.util.Base64
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

class AmountExceedsOfflineLimitException(message: String) : IllegalArgumentException(message)

data class StoredOfflineTransaction(
    val id: String,
    val merchantId: String,
    val amountCents: Long,
    val encryptedPayload: String,
    val queuedAtTimestampMs: Long
)

data class DispatchResult(
    val dispatchedCount: Int,
    val remainingCount: Int,
    val isFullyFlushed: Boolean
)

fun interface OfflineTransactionDispatcher {
    suspend fun dispatch(id: String, merchantId: String, amountCents: Long, payload: String): Boolean
}

interface PayloadCipher {
    fun encrypt(plaintext: String): String
    fun decrypt(ciphertext: String): String
}

class AesGcmPayloadCipher(
    private val secretKey: SecretKey = generateAesKey()
) : PayloadCipher {

    companion object {
        private const val AES_GCM_NOPADDING = "AES/GCM/NoPadding"
        private const val GCM_IV_LENGTH = 12
        private const val GCM_TAG_LENGTH_BITS = 128

        private fun generateAesKey(): SecretKey {
            val keyGen = KeyGenerator.getInstance("AES")
            keyGen.init(256, SecureRandom())
            return keyGen.generateKey()
        }
    }

    override fun encrypt(plaintext: String): String {
        val iv = ByteArray(GCM_IV_LENGTH).apply { SecureRandom().nextBytes(this) }
        val cipher = Cipher.getInstance(AES_GCM_NOPADDING)
        cipher.init(Cipher.ENCRYPT_MODE, secretKey, GCMParameterSpec(GCM_TAG_LENGTH_BITS, iv))
        val cipherText = cipher.doFinal(plaintext.toByteArray(Charsets.UTF_8))

        val combined = ByteArray(iv.size + cipherText.size)
        System.arraycopy(iv, 0, combined, 0, iv.size)
        System.arraycopy(cipherText, 0, combined, iv.size, cipherText.size)

        return Base64.getEncoder().encodeToString(combined)
    }

    override fun decrypt(ciphertext: String): String {
        val combined = Base64.getDecoder().decode(ciphertext)
        require(combined.size > GCM_IV_LENGTH) { "Ciphertext truncated" }

        val iv = ByteArray(GCM_IV_LENGTH)
        val cipherBytes = ByteArray(combined.size - GCM_IV_LENGTH)
        System.arraycopy(combined, 0, iv, 0, GCM_IV_LENGTH)
        System.arraycopy(combined, GCM_IV_LENGTH, cipherBytes, 0, cipherBytes.size)

        val cipher = Cipher.getInstance(AES_GCM_NOPADDING)
        cipher.init(Cipher.DECRYPT_MODE, secretKey, GCMParameterSpec(GCM_TAG_LENGTH_BITS, iv))
        val plainBytes = cipher.doFinal(cipherBytes)

        return String(plainBytes, Charsets.UTF_8)
    }
}

class StoreAndForwardQueue(
    private val cipher: PayloadCipher = AesGcmPayloadCipher()
) {
    companion object {
        const val MAX_OFFLINE_AMOUNT_CENTS: Long = 40_000L // $400.00 MXN max offline limit
    }

    private val queue = ArrayDeque<StoredOfflineTransaction>()
    private val lock = Any()

    fun enqueue(
        id: String,
        merchantId: String,
        amountCents: Long,
        payload: String
    ): StoredOfflineTransaction {
        if (amountCents > MAX_OFFLINE_AMOUNT_CENTS) {
            throw AmountExceedsOfflineLimitException(
                "Offline transaction amount $amountCents cents exceeds regulatory limit of $MAX_OFFLINE_AMOUNT_CENTS cents ($400.00 MXN)"
            )
        }
        require(amountCents > 0L) { "Amount must be strictly positive" }
        require(id.isNotBlank()) { "Transaction ID must not be blank" }

        val encryptedPayload = cipher.encrypt(payload)
        val stored = StoredOfflineTransaction(
            id = id,
            merchantId = merchantId,
            amountCents = amountCents,
            encryptedPayload = encryptedPayload,
            queuedAtTimestampMs = System.currentTimeMillis()
        )

        synchronized(lock) {
            queue.addLast(stored)
        }
        return stored
    }

    fun size(): Int = synchronized(lock) { queue.size }

    fun peekAll(): List<StoredOfflineTransaction> = synchronized(lock) { queue.toList() }

    fun decryptPayload(encryptedPayload: String): String = cipher.decrypt(encryptedPayload)

    suspend fun dispatchAll(dispatcher: OfflineTransactionDispatcher): DispatchResult {
        var dispatched = 0

        while (true) {
            val item = synchronized(lock) {
                queue.firstOrNull()
            } ?: break

            val plainPayload = try {
                decryptPayload(item.encryptedPayload)
            } catch (e: Exception) {
                // If corrupted or decryption fails, halt
                break
            }

            val success = try {
                dispatcher.dispatch(item.id, item.merchantId, item.amountCents, plainPayload)
            } catch (e: Exception) {
                false
            }

            if (success) {
                synchronized(lock) {
                    if (queue.isNotEmpty() && queue.first().id == item.id) {
                        queue.removeFirst()
                    }
                }
                dispatched++
            } else {
                // Halt on dispatch failure to preserve strict FIFO ordering
                break
            }
        }

        val remaining = size()
        return DispatchResult(
            dispatchedCount = dispatched,
            remainingCount = remaining,
            isFullyFlushed = remaining == 0
        )
    }
}
