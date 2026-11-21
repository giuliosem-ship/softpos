package mx.softpos.app.ui.fiscal

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.fiscal.CertificateSummary
import mx.softpos.app.fiscal.CsdApi
import mx.softpos.app.fiscal.CsdFile
import mx.softpos.app.fiscal.CsdInfo
import mx.softpos.app.fiscal.CsdInspector
import mx.softpos.app.fiscal.CsdIssue
import mx.softpos.app.fiscal.csdInvalidReason
import mx.softpos.app.fiscal.isCsdNotFound
import mx.softpos.app.merchant.MerchantApi
import mx.softpos.app.ui.merchant.LoadError
import mx.softpos.app.ui.merchant.MerchantIdProvider
import mx.softpos.app.ui.merchant.SaleDetailViewModel
import mx.softpos.app.ui.merchant.toLoadError
import java.time.Duration
import java.time.Instant
import java.time.OffsetDateTime
import java.time.format.DateTimeParseException

enum class CsdUploadError { INVALID, NOT_ALLOWED, NETWORK, UNKNOWN }

data class CsdUiState(
    val isOwner: Boolean = false,
    val isLoading: Boolean = false,
    /** Stored certificate, null when none or not loaded yet. */
    val csd: CsdInfo? = null,
    /** `404 CSD_NOT_FOUND`: nothing uploaded yet. */
    val notConfigured: Boolean = false,
    val loadError: LoadError? = null,
    /** Days until `valid_to` (negative = expired), null if unknown. */
    val daysLeft: Long? = null,
    val merchantRfc: String? = null,
    val cerName: String? = null,
    val cerSummary: CertificateSummary? = null,
    val keyName: String? = null,
    val issues: List<CsdIssue> = emptyList(),
    val isUploading: Boolean = false,
    val uploadError: CsdUploadError? = null,
    /** `reason` of `422 CSD_INVALID` (backend code or text). */
    val invalidReason: String? = null,
    /** True right after a successful upload. */
    val uploaded: Boolean = false
) {
    val expiringSoon: Boolean get() = daysLeft != null && daysLeft in 0 until EXPIRY_WARNING_DAYS
    val expired: Boolean get() = daysLeft != null && daysLeft < 0
    val canUpload: Boolean get() = isOwner && cerName != null && keyName != null && issues.isEmpty() && !isUploading

    companion object {
        const val EXPIRY_WARNING_DAYS = 30L
    }
}

/**
 * Profile → *Sellos digitales (CSD)*. Shows the stored certificate and lets
 * the owner upload a new `.cer` + `.key` + password. The key bytes and the
 * password are sensitive: the password arrives as a [CharArray] and is zeroed
 * on every path; the key is zeroed after each upload attempt, on replacement
 * and when the ViewModel is cleared. Neither is ever logged or persisted.
 */
class CsdViewModel(
    private val csdApi: CsdApi,
    private val merchantApi: MerchantApi,
    private val merchantId: MerchantIdProvider,
    role: () -> String,
    private val io: CoroutineDispatcher = Dispatchers.IO,
    private val clock: () -> Instant = Instant::now
) : ViewModel() {
    private val _state = MutableStateFlow(CsdUiState(isOwner = role() == SaleDetailViewModel.ROLE_OWNER))
    val state: StateFlow<CsdUiState> = _state.asStateFlow()

    private var cer: CsdFile? = null
    private var key: CsdFile? = null

    fun load() {
        val id = merchantId()
        if (id == null) {
            _state.update { it.copy(loadError = LoadError.NO_MERCHANT) }
            return
        }
        _state.update { it.copy(isLoading = true, loadError = null) }
        viewModelScope.launch {
            val rfc = (withContext(io) { merchantApi.getMerchant(id) } as? ApiResult.Success)?.data?.rfc?.ifBlank { null }
            val r = withContext(io) { csdApi.getCsd(id) }
            _state.update {
                val withRfc = it.copy(isLoading = false, merchantRfc = rfc ?: it.merchantRfc)
                when {
                    r is ApiResult.Success -> withRfc.withCsd(r.data)
                    r is ApiResult.Error && r.isCsdNotFound() -> withRfc.copy(csd = null, daysLeft = null, notConfigured = true)
                    r is ApiResult.Error -> withRfc.copy(loadError = r.toLoadError())
                    else -> withRfc
                }.recheck()
            }
        }
    }

    /** [bytes] is null when the picked file could not be read (or is too large). */
    fun onCerPicked(name: String, bytes: ByteArray?) {
        cer?.bytes?.fill(0)
        cer = bytes?.let { CsdFile(name, it) }
        val summary = bytes?.let { CsdInspector.inspect(it) }
        _state.update { it.copy(cerName = name, cerSummary = summary, uploadError = null, invalidReason = null, uploaded = false).recheck(cerReadable = bytes != null) }
    }

    fun onKeyPicked(name: String, bytes: ByteArray?) {
        key?.bytes?.fill(0)
        key = bytes?.let { CsdFile(name, it) }
        _state.update {
            it.copy(keyName = name, uploadError = null, invalidReason = null, uploaded = false)
                .recheck(keyReadable = bytes != null)
        }
    }

    /** Takes ownership of [password]: it is zeroed before this returns or when the request ends. */
    fun upload(password: CharArray) {
        val id = merchantId()
        val c = cer
        val k = key
        val s = _state.value
        if (id == null || c == null || k == null || !s.canUpload) {
            password.fill('\u0000')
            return
        }
        if (password.isEmpty()) {
            _state.update { it.copy(issues = it.issues + CsdIssue.PASSWORD_REQUIRED) }
            return
        }
        _state.update { it.copy(isUploading = true, uploadError = null, invalidReason = null) }
        viewModelScope.launch {
            val r = try {
                withContext(io) { csdApi.uploadCsd(id, c, k, password) }
            } finally {
                password.fill('\u0000')
                // A new attempt needs the key picked again: it is never kept after being sent.
                k.bytes.fill(0)
                if (key === k) key = null
            }
            _state.update {
                when (r) {
                    is ApiResult.Success -> {
                        cer?.bytes?.fill(0)
                        cer = null
                        it.copy(
                            isUploading = false, uploaded = true, notConfigured = false,
                            cerName = null, cerSummary = null, keyName = null, issues = emptyList()
                        ).withCsd(r.data)
                    }
                    is ApiResult.Error -> it.copy(
                        isUploading = false, keyName = null,
                        uploadError = r.toUploadError(), invalidReason = r.csdInvalidReason()
                    )
                }
            }
        }
    }

    /** The password field changed after a "password required" hint. */
    fun onPasswordEdited() = _state.update { it.copy(issues = it.issues - CsdIssue.PASSWORD_REQUIRED) }

    private fun CsdUiState.withCsd(info: CsdInfo): CsdUiState = copy(csd = info, notConfigured = false, daysLeft = daysUntil(info.validTo))

    private fun CsdUiState.recheck(
        cerReadable: Boolean = cer != null,
        keyReadable: Boolean = key != null
    ): CsdUiState {
        val issues = CsdInspector.issues(cerSummary, cerName != null, key?.bytes, merchantRfc, clock()).toMutableList()
        if (cerName != null && !cerReadable && CsdIssue.CER_UNREADABLE !in issues) issues += CsdIssue.CER_UNREADABLE
        if (keyName != null && !keyReadable) issues += CsdIssue.KEY_INVALID
        return copy(issues = issues.distinct())
    }

    private fun daysUntil(validTo: String): Long? = parseInstant(validTo)?.let { end ->
        val now = clock()
        // Duration.toDays truncates toward zero: an expiry a few hours ago is still "expired".
        Duration.between(now, end).toDays().let { d -> if (d == 0L && now.isAfter(end)) -1L else d }
    }

    private fun ApiResult.Error.toUploadError(): CsdUploadError = when {
        statusCode == NETWORK_ERROR_STATUS -> CsdUploadError.NETWORK
        statusCode == 422 -> CsdUploadError.INVALID
        statusCode == 403 -> CsdUploadError.NOT_ALLOWED
        else -> CsdUploadError.UNKNOWN
    }

    override fun onCleared() {
        cer?.bytes?.fill(0)
        key?.bytes?.fill(0)
        cer = null
        key = null
    }
}

/** RFC 3339 with `Z` or an offset (`-06:00`); null if unparsable. */
internal fun parseInstant(value: String): Instant? = try {
    OffsetDateTime.parse(value).toInstant()
} catch (_: DateTimeParseException) {
    null
}
