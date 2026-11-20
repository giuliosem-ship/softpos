package mx.softpos.app.auth

import android.content.Context
import android.content.SharedPreferences
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import androidx.core.content.edit
import java.security.KeyStore
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey

/**
 * AES-256 key that lives in the Android Keystore (hardware-backed when the
 * device has a TEE/StrongBox); its material never enters app memory.
 */
object AndroidKeystoreKeys {
    private const val PROVIDER = "AndroidKeyStore"
    const val SESSION_KEY_ALIAS = "kerno_session_key_v1"

    @Synchronized
    fun getOrCreate(alias: String = SESSION_KEY_ALIAS): SecretKey {
        val keyStore = KeyStore.getInstance(PROVIDER).apply { load(null) }
        (keyStore.getKey(alias, null) as? SecretKey)?.let { return it }

        val generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, PROVIDER)
        generator.init(
            KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .setRandomizedEncryptionRequired(true)
                .build()
        )
        return generator.generateKey()
    }
}

class SharedPreferencesKeyValueStore(private val prefs: SharedPreferences) : KeyValueStore {
    override fun getString(key: String): String? = prefs.getString(key, null)
    override fun putString(key: String, value: String) = prefs.edit { putString(key, value) }
    override fun remove(key: String) = prefs.edit { remove(key) }
}

/** Production token store: Keystore AES-GCM ciphertext in private prefs. */
fun keystoreTokenStore(context: Context): TokenStore = EncryptedTokenStore(
    kv = SharedPreferencesKeyValueStore(context.getSharedPreferences("kerno_session", Context.MODE_PRIVATE)),
    cipher = AesGcmSecretCipher { AndroidKeystoreKeys.getOrCreate() }
)
