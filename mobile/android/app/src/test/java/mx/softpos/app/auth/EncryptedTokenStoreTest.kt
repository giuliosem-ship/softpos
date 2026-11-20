package mx.softpos.app.auth

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import javax.crypto.KeyGenerator

class EncryptedTokenStoreTest {
    private class MapStore : KeyValueStore {
        val map = mutableMapOf<String, String>()
        override fun getString(key: String) = map[key]
        override fun putString(key: String, value: String) { map[key] = value }
        override fun remove(key: String) { map.remove(key) }
    }

    private val key = KeyGenerator.getInstance("AES").apply { init(256) }.generateKey()
    private val session = StoredSession("at_secret", "rt_secret", 1_000L, "usr_1", "mer_1", "owner")

    @Test
    fun roundTripsAndNeverStoresPlaintext() {
        val kv = MapStore()
        val store = EncryptedTokenStore(kv, AesGcmSecretCipher { key })
        store.save(session)

        val blob = kv.map.getValue(EncryptedTokenStore.KEY)
        assertFalse(blob.contains("secret"))
        assertEquals(session, EncryptedTokenStore(kv, AesGcmSecretCipher { key }).load())
    }

    @Test
    fun eachSaveUsesAFreshIv() {
        val kv = MapStore()
        val store = EncryptedTokenStore(kv, AesGcmSecretCipher { key })
        store.save(session)
        val first = kv.map.getValue(EncryptedTokenStore.KEY)
        store.save(session)
        assertFalse(first == kv.map.getValue(EncryptedTokenStore.KEY))
    }

    @Test
    fun tamperedOrForeignKeyBlobIsDroppedInsteadOfCrashing() {
        val kv = MapStore()
        EncryptedTokenStore(kv, AesGcmSecretCipher { key }).save(session)
        val otherKey = KeyGenerator.getInstance("AES").apply { init(256) }.generateKey()

        assertNull(EncryptedTokenStore(kv, AesGcmSecretCipher { otherKey }).load())
        assertNull(kv.map[EncryptedTokenStore.KEY])
    }

    @Test
    fun clearRemovesSession() {
        val kv = MapStore()
        val store = EncryptedTokenStore(kv, AesGcmSecretCipher { key })
        store.save(session)
        store.clear()
        assertNull(store.load())
    }

    @Test
    fun toStringDoesNotLeakTokens() {
        assertFalse(session.toString().contains("secret"))
    }
}
