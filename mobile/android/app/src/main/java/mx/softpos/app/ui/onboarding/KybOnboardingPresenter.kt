package mx.softpos.app.ui.onboarding

import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.MutableStateFlow
import java.util.regex.Pattern

data class CsdUploadBundle(
    val merchantId: String,
    val rfc: String,
    val cerFileName: String,
    val cerBytes: ByteArray,
    val keyFileName: String,
    val keyBytes: ByteArray,
    val password: String
)

sealed interface KybValidationResult {
    data class Valid(
        val merchantId: String,
        val normalizedRfc: String,
        val cerSizeBytes: Int,
        val keySizeBytes: Int
    ) : KybValidationResult

    data class Invalid(val errors: List<String>) : KybValidationResult
}

object KybOnboardingPresenter {
    private val RFC_REGEX = Regex("^[A-Z&Ñ]{3,4}[0-9]{6}[A-Z0-9]{3}$")

    fun validateCsdBundle(bundle: CsdUploadBundle): KybValidationResult {
        val errors = mutableListOf<String>()

        if (bundle.merchantId.isBlank()) {
            errors.add("MERCHANT_ID_REQUIRED")
        }

        val normalizedRfc = bundle.rfc.trim().uppercase()
        if (!RFC_REGEX.matches(normalizedRfc)) {
            errors.add("INVALID_RFC_FORMAT")
        }

        if (!bundle.cerFileName.trim().lowercase().endsWith(".cer") || bundle.cerBytes.isEmpty()) {
            errors.add("INVALID_CER_FILE")
        }

        if (!bundle.keyFileName.trim().lowercase().endsWith(".key") || bundle.keyBytes.isEmpty()) {
            errors.add("INVALID_KEY_FILE")
        }

        if (bundle.password.length < 4) {
            errors.add("CSD_PASSWORD_TOO_SHORT")
        }

        return if (errors.isEmpty()) {
            KybValidationResult.Valid(
                merchantId = bundle.merchantId.trim(),
                normalizedRfc = normalizedRfc,
                cerSizeBytes = bundle.cerBytes.size,
                keySizeBytes = bundle.keyBytes.size
            )
        } else {
            KybValidationResult.Invalid(errors)
        }
    }
}
