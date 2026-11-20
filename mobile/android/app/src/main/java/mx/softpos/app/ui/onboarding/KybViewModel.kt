package mx.softpos.app.ui.onboarding

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
import mx.softpos.app.kyb.KybAddress
import mx.softpos.app.kyb.KybApi
import mx.softpos.app.kyb.KybApplication
import mx.softpos.app.kyb.KybDocumentType
import mx.softpos.app.kyb.KybStatus
import mx.softpos.app.kyb.KybStatusValue
import mx.softpos.app.kyb.PickedDocument
import mx.softpos.app.kyb.invalidKybFields
import mx.softpos.app.kyb.kybChecksFailed
import mx.softpos.app.ui.login.PhoneNumbers
import mx.softpos.app.ui.merchant.MerchantIdProvider

enum class KybStep { LOADING, FORM, DOCUMENTS, STATUS }

enum class KybField {
    LEGAL_NAME, RFC, CURP, TAX_REGIME, MCC, STREET, EXT_NUMBER, NEIGHBORHOOD, CITY, STATE, POSTAL_CODE, EMAIL, PHONE
}

enum class KybError { NO_MERCHANT, NOT_OWNER, NETWORK, TOO_LARGE, INCOMPLETE, INVALID_DATA, CHECKS_FAILED, NOT_AVAILABLE, UNKNOWN }

enum class UploadStatus { UPLOADING, RECEIVED, FAILED }

data class DocUpload(val status: UploadStatus, val percent: Int = 0, val documentId: String? = null)

data class KybForm(
    val legalName: String = "",
    val rfc: String = "",
    val curp: String = "",
    val taxRegime: String = KybValidators.RESICO_PF,
    val mcc: String = "",
    val street: String = "",
    val extNumber: String = "",
    val intNumber: String = "",
    val neighborhood: String = "",
    val city: String = "",
    val state: String = "",
    val postalCode: String = "",
    val email: String = "",
    val phone: String = ""
)

data class KybUiState(
    val step: KybStep = KybStep.LOADING,
    val status: KybStatus? = null,
    val form: KybForm = KybForm(),
    val fieldErrors: Set<KybField> = emptySet(),
    val uploads: Map<String, DocUpload> = emptyMap(),
    val isOwner: Boolean = false,
    val isBusy: Boolean = false,
    val error: KybError? = null
) {
    /** Documents the backend asks for; all six when it does not say. */
    val requiredDocuments: List<String>
        get() = status?.requiredDocuments?.ifEmpty { null } ?: KybDocumentType.ALL

    val canSubmit: Boolean
        get() = !isBusy && (status?.checks?.documentsComplete == true ||
            requiredDocuments.all { uploads[it]?.status == UploadStatus.RECEIVED })
}

/**
 * KYB onboarding (contract: `GET|PUT /merchants/{id}/kyb`, `POST
 * .../kyb/documents` multipart, `POST .../kyb/submit`). DRAFT → form →
 * documents → submit → status; REJECTED shows the reason and lets the owner
 * correct and resubmit. Non-owners only see the status. Captured documents
 * are personal data: their bytes are zeroed once uploaded, replaced, or when
 * the ViewModel is cleared.
 */
class KybViewModel(
    private val kybApi: KybApi,
    private val merchantId: MerchantIdProvider,
    role: () -> String,
    private val io: CoroutineDispatcher = Dispatchers.IO
) : ViewModel() {
    private val _state = MutableStateFlow(KybUiState(isOwner = role() == "owner"))
    val state: StateFlow<KybUiState> = _state.asStateFlow()

    /** Documents picked but not yet accepted by the backend (kept for a retry). */
    private val pending = HashMap<String, PickedDocument>()

    fun load() {
        val id = merchantId() ?: return _state.update { it.copy(step = KybStep.STATUS, error = KybError.NO_MERCHANT) }
        _state.update { it.copy(isBusy = true, error = null) }
        viewModelScope.launch {
            val r = withContext(io) { kybApi.getKyb(id) }
            _state.update { s ->
                when {
                    r is ApiResult.Success -> resume(s.copy(isBusy = false, status = r.data, step = stepFor(r.data, s)), r.data)
                    // No application yet: start one.
                    r is ApiResult.Error && r.statusCode == 404 && r.errorCode != "HTTP_404" -> {
                        val draft = KybStatus(KybStatusValue.DRAFT)
                        s.copy(isBusy = false, status = draft, step = stepFor(draft, s))
                    }
                    else -> s.copy(isBusy = false, step = KybStep.STATUS, error = (r as ApiResult.Error).toKybError())
                }
            }
        }
    }

    /**
     * Resumes an onboarding started earlier: the owner's saved application
     * fills an untouched form and documents the backend already holds count
     * as received (`uploaded_documents`).
     */
    private fun resume(s: KybUiState, status: KybStatus): KybUiState {
        val app = status.application
        val form = if (app != null && s.form == KybForm()) {
            KybForm(
                legalName = app.legalName, rfc = app.rfc, curp = app.curp.orEmpty(),
                taxRegime = app.taxRegime.ifBlank { KybValidators.RESICO_PF }, mcc = app.mcc,
                street = app.address.street, extNumber = app.address.extNumber, intNumber = app.address.intNumber.orEmpty(),
                neighborhood = app.address.neighborhood, city = app.address.city, state = app.address.state,
                postalCode = app.address.postalCode, email = app.contactEmail,
                phone = app.contactPhone.removePrefix("+52")
            )
        } else s.form
        val received = status.uploadedDocuments.filter { s.uploads[it] == null }
            .associateWith { DocUpload(UploadStatus.RECEIVED, 100) }
        return s.copy(form = form, uploads = s.uploads + received)
    }

    private fun stepFor(status: KybStatus, s: KybUiState): KybStep =
        if (status.status == KybStatusValue.DRAFT && s.isOwner) KybStep.FORM else KybStep.STATUS

    /** From the REJECTED status screen: correct the data and resubmit. */
    fun edit() {
        if (_state.value.isOwner) _state.update { it.copy(step = KybStep.FORM, error = null) }
    }

    fun goToDocuments() = _state.update { it.copy(step = KybStep.DOCUMENTS, error = null) }

    fun onFormChange(transform: (KybForm) -> KybForm) =
        _state.update { s ->
            val f = transform(s.form)
            s.copy(form = f.copy(rfc = f.rfc.uppercase().take(13), curp = f.curp.uppercase().take(18)), error = null)
        }

    fun validate(form: KybForm): Set<KybField> = buildSet {
        if (form.legalName.isBlank()) add(KybField.LEGAL_NAME)
        if (!KybValidators.isValidRfc(form.rfc)) add(KybField.RFC)
        // CURP is optional in the contract but always valid-shaped when given.
        if (form.curp.isNotBlank() && !KybValidators.isValidCurp(form.curp)) add(KybField.CURP)
        if (!KybValidators.isValidTaxRegime(form.taxRegime)) add(KybField.TAX_REGIME)
        if (!KybValidators.isValidMcc(form.mcc)) add(KybField.MCC)
        if (form.street.isBlank()) add(KybField.STREET)
        if (form.extNumber.isBlank()) add(KybField.EXT_NUMBER)
        if (form.neighborhood.isBlank()) add(KybField.NEIGHBORHOOD)
        if (form.city.isBlank()) add(KybField.CITY)
        if (form.state.isBlank()) add(KybField.STATE)
        if (!KybValidators.isValidPostalCode(form.postalCode)) add(KybField.POSTAL_CODE)
        if (!KybValidators.isValidEmail(form.email)) add(KybField.EMAIL)
        if (PhoneNumbers.normalizeMx(form.phone) == null) add(KybField.PHONE)
    }

    /** Validates and `PUT`s the application, then moves on to the documents. */
    fun saveForm() {
        val s = _state.value
        val id = merchantId() ?: return
        if (!s.isOwner) return _state.update { it.copy(error = KybError.NOT_OWNER) }
        val errors = validate(s.form)
        _state.update { it.copy(fieldErrors = errors) }
        if (errors.isNotEmpty() || s.isBusy) return
        val f = s.form
        val application = KybApplication(
            legalName = f.legalName.trim(),
            rfc = f.rfc.trim().uppercase(),
            curp = f.curp.trim().uppercase().ifBlank { null },
            taxRegime = f.taxRegime.trim(),
            mcc = f.mcc.trim(),
            address = KybAddress(
                street = f.street.trim(), extNumber = f.extNumber.trim(), intNumber = f.intNumber.trim().ifBlank { null },
                neighborhood = f.neighborhood.trim(), city = f.city.trim(), state = f.state.trim(), postalCode = f.postalCode.trim()
            ),
            contactEmail = f.email.trim(),
            contactPhone = PhoneNumbers.normalizeMx(f.phone)!!
        )
        _state.update { it.copy(isBusy = true, error = null) }
        viewModelScope.launch {
            val r = withContext(io) { kybApi.putKyb(id, application) }
            _state.update {
                when (r) {
                    is ApiResult.Success -> it.copy(isBusy = false, step = KybStep.DOCUMENTS)
                    is ApiResult.Error -> {
                        val fields = r.invalidKybFields().mapNotNull(::fieldFor).toSet()
                        it.copy(isBusy = false, fieldErrors = fields, error = if (fields.isEmpty()) r.toKybError() else KybError.INVALID_DATA)
                    }
                }
            }
        }
    }

    /** Uploads a captured/picked document with progress; a new pick replaces (and zeroes) the old one. */
    fun upload(type: String, document: PickedDocument) {
        val id = merchantId() ?: return
        if (document.bytes.size > MAX_DOCUMENT_BYTES) {
            document.bytes.fill(0)
            return _state.update { it.copy(error = KybError.TOO_LARGE) }
        }
        if (_state.value.uploads[type]?.status == UploadStatus.UPLOADING) return
        pending.put(type, document)?.takeIf { it !== document }?.bytes?.fill(0)
        _state.update { it.copy(uploads = it.uploads + (type to DocUpload(UploadStatus.UPLOADING)), error = null) }
        viewModelScope.launch {
            val r = withContext(io) {
                kybApi.uploadDocument(id, type, document) { sent, total ->
                    val percent = if (total <= 0L) 100 else (sent * 100L / total).toInt()
                    _state.update { s ->
                        val current = s.uploads[type]
                        if (current?.status != UploadStatus.UPLOADING || current.percent == percent) s
                        else s.copy(uploads = s.uploads + (type to current.copy(percent = percent)))
                    }
                }
            }
            when (r) {
                is ApiResult.Success -> {
                    pending.remove(type)?.bytes?.fill(0)
                    _state.update { it.copy(uploads = it.uploads + (type to DocUpload(UploadStatus.RECEIVED, 100, r.data.documentId))) }
                }
                is ApiResult.Error -> _state.update {
                    it.copy(uploads = it.uploads + (type to DocUpload(UploadStatus.FAILED)), error = r.toKybError())
                }
            }
        }
    }

    /** Retries a failed upload with the document still held in memory. */
    fun retryUpload(type: String) {
        val doc = pending[type] ?: return
        upload(type, doc)
    }

    fun submit() {
        val id = merchantId() ?: return
        val s = _state.value
        if (!s.isOwner) return _state.update { it.copy(error = KybError.NOT_OWNER) }
        if (!s.canSubmit) return _state.update { it.copy(error = KybError.INCOMPLETE) }
        _state.update { it.copy(isBusy = true, error = null) }
        viewModelScope.launch {
            when (val r = withContext(io) { kybApi.submit(id) }) {
                // IN_REVIEW, or REJECTED (RFC in SAT 69-B): never SUBMITTED in practice.
                is ApiResult.Success -> _state.update { it.copy(isBusy = false, status = r.data, step = KybStep.STATUS) }
                is ApiResult.Error -> {
                    val failed = r.kybChecksFailed()
                    when {
                        // Automatic checks failed: still DRAFT, show why and let the owner fix it.
                        failed != null -> _state.update {
                            it.copy(isBusy = false, status = failed, step = KybStep.STATUS, error = KybError.CHECKS_FAILED)
                        }
                        // Submitted meanwhile (another device): show the real status.
                        r.statusCode == 409 && r.errorCode == "KYB_ALREADY_SUBMITTED" -> {
                            _state.update { it.copy(isBusy = false) }
                            load()
                        }
                        else -> _state.update { it.copy(isBusy = false, error = r.toKybError()) }
                    }
                }
            }
        }
    }

    override fun onCleared() {
        pending.values.forEach { it.bytes.fill(0) }
        pending.clear()
    }

    private fun ApiResult.Error.toKybError(): KybError = when {
        statusCode == NETWORK_ERROR_STATUS -> KybError.NETWORK
        statusCode == 403 && errorCode == "MERCHANT_SCOPE_REQUIRED" -> KybError.NO_MERCHANT
        statusCode == 403 -> KybError.NOT_OWNER
        statusCode == 413 -> KybError.TOO_LARGE
        statusCode == 404 || statusCode == 405 || statusCode == 501 -> KybError.NOT_AVAILABLE
        statusCode == 409 || errorCode.contains("INCOMPLETE") -> KybError.INCOMPLETE
        else -> KybError.UNKNOWN
    }

    companion object {
        /** Backend cap per document (pkg/kyb MaxDocumentBytes); photos are ~0.3–1 MB after compression. */
        const val MAX_DOCUMENT_BYTES = 5 * 1024 * 1024

        /** Backend `INVALID_KYB_DATA` field names → form fields. */
        internal fun fieldFor(name: String): KybField? = when (name) {
            "legal_name" -> KybField.LEGAL_NAME
            "rfc" -> KybField.RFC
            "curp" -> KybField.CURP
            "tax_regime" -> KybField.TAX_REGIME
            "mcc" -> KybField.MCC
            "address.street" -> KybField.STREET
            "address.ext_number" -> KybField.EXT_NUMBER
            "address.neighborhood" -> KybField.NEIGHBORHOOD
            "address.city" -> KybField.CITY
            "address.state" -> KybField.STATE
            "address.postal_code" -> KybField.POSTAL_CODE
            "contact_email" -> KybField.EMAIL
            "contact_phone" -> KybField.PHONE
            else -> null
        }
    }
}
