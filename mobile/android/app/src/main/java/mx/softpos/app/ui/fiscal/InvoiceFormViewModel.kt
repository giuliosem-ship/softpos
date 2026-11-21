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
import mx.softpos.app.core.network.KernoJson
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.core.network.isUnknownOutcome
import mx.softpos.app.fiscal.CreateInvoiceRequest
import mx.softpos.app.fiscal.CsdApi
import mx.softpos.app.fiscal.Invoice
import mx.softpos.app.fiscal.InvoiceApi
import mx.softpos.app.fiscal.Persona
import mx.softpos.app.fiscal.Receptor
import mx.softpos.app.fiscal.ReceptorValidator
import mx.softpos.app.fiscal.RegimenFiscal
import mx.softpos.app.fiscal.SatCatalogCache
import mx.softpos.app.fiscal.SatCatalogs
import mx.softpos.app.fiscal.UsoCfdi
import mx.softpos.app.fiscal.invalidReceptorFields
import mx.softpos.app.fiscal.isCsdNotFound
import mx.softpos.app.ui.merchant.LoadError
import mx.softpos.app.ui.merchant.MerchantIdProvider
import mx.softpos.app.ui.merchant.toLoadError
import java.util.UUID

enum class ReceptorField { RFC, NOMBRE, REGIMEN, USO_CFDI, CODIGO_POSTAL, EMAIL }

/** Whether the merchant can stamp: invoicing needs an ACTIVE CSD. */
enum class CsdGate { CHECKING, ACTIVE, MISSING, UNKNOWN }

enum class InvoiceError {
    ALREADY_INVOICED, RECEPTOR_INVALID, CSD_REQUIRED, NOT_ALLOWED, IDEMPOTENCY_CONFLICT, PROVIDER_UNAVAILABLE, NETWORK, UNKNOWN
}

data class ReceptorForm(
    val rfc: String = "",
    val nombre: String = "",
    val regimen: String = "",
    val usoCfdi: String = "",
    val codigoPostal: String = "",
    val email: String = ""
) {
    override fun toString(): String = "ReceptorForm(<redacted>)"
}

data class InvoiceFormUiState(
    val saleIds: List<String> = emptyList(),
    val form: ReceptorForm = ReceptorForm(),
    val catalogs: SatCatalogs? = null,
    val catalogError: LoadError? = null,
    val csd: CsdGate = CsdGate.CHECKING,
    val invalidFields: Set<ReceptorField> = emptySet(),
    val isSubmitting: Boolean = false,
    val error: InvoiceError? = null,
    val result: Invoice? = null
) {
    val persona: Persona? get() = ReceptorValidator.persona(form.rfc)
    val regimenOptions: List<RegimenFiscal> get() = catalogs?.let { ReceptorValidator.regimenesFor(it, persona) }.orEmpty()
    val usoOptions: List<UsoCfdi> get() = catalogs?.let { ReceptorValidator.usosFor(it, form.regimen) }.orEmpty()
    val canSubmit: Boolean get() = saleIds.isNotEmpty() && catalogs != null && csd != CsdGate.MISSING && !isSubmitting && result == null
}

/**
 * CFDI for one or more sales. The `Idempotency-Key` is stable per request
 * body: kept while the outcome is unknown (no response, 5xx such as a `502`
 * from the PAC) so the retry replays the same invoice instead of stamping a
 * second one; dropped on any 4xx or once an invoice comes back, and replaced
 * when the receptor or the sales change.
 */
class InvoiceFormViewModel(
    saleIds: List<String>,
    private val invoiceApi: InvoiceApi,
    private val csdApi: CsdApi,
    private val catalogs: SatCatalogCache,
    private val merchantId: MerchantIdProvider,
    private val io: CoroutineDispatcher = Dispatchers.IO,
    private val newIdempotencyKey: () -> String = { UUID.randomUUID().toString() }
) : ViewModel() {
    private val _state = MutableStateFlow(InvoiceFormUiState(saleIds = saleIds.distinct()))
    val state: StateFlow<InvoiceFormUiState> = _state.asStateFlow()

    /** (request fingerprint, key) of the last attempt whose outcome is unknown. */
    private var attemptKey: Pair<String, String>? = null

    /** Loads the catalogs (cached) and re-checks the CSD; call on every visit (back from the CSD screen). */
    fun load() {
        val id = merchantId() ?: return
        viewModelScope.launch {
            val csd = withContext(io) { csdApi.getCsd(id) }
            val gate = when {
                csd is ApiResult.Success -> if (csd.data.isActive) CsdGate.ACTIVE else CsdGate.MISSING
                csd is ApiResult.Error && csd.isCsdNotFound() -> CsdGate.MISSING
                else -> CsdGate.UNKNOWN // the backend still rejects the create if there is none
            }
            _state.update { it.copy(csd = gate) }
            if (_state.value.catalogs == null) {
                val r = withContext(io) { catalogs.get() }
                _state.update {
                    when (r) {
                        is ApiResult.Success -> it.copy(catalogs = r.data, catalogError = null).sanitized()
                        is ApiResult.Error -> it.copy(catalogError = r.toLoadError())
                    }
                }
            }
        }
    }

    fun onRfc(value: String) = edit(ReceptorField.RFC) { it.copy(rfc = value.uppercase().filter { c -> !c.isWhitespace() }.take(13)) }
    fun onNombre(value: String) = edit(ReceptorField.NOMBRE) { it.copy(nombre = value.take(254)) }
    fun onRegimen(code: String) = edit(ReceptorField.REGIMEN) { it.copy(regimen = code) }
    fun onUsoCfdi(code: String) = edit(ReceptorField.USO_CFDI) { it.copy(usoCfdi = code) }
    fun onCodigoPostal(value: String) = edit(ReceptorField.CODIGO_POSTAL) { it.copy(codigoPostal = value.filter(Char::isDigit).take(5)) }
    fun onEmail(value: String) = edit(ReceptorField.EMAIL) { it.copy(email = value.trim().take(254)) }

    private fun edit(field: ReceptorField, change: (ReceptorForm) -> ReceptorForm) =
        _state.update { it.copy(form = change(it.form), invalidFields = it.invalidFields - field, error = null).sanitized() }

    /** Drops a régimen that no longer fits the persona type and a uso CFDI that no longer fits the régimen. */
    private fun InvoiceFormUiState.sanitized(): InvoiceFormUiState {
        if (catalogs == null) return this
        var f = form
        if (f.regimen.isNotEmpty() && regimenOptions.none { it.code == f.regimen }) f = f.copy(regimen = "", usoCfdi = "")
        val usos = ReceptorValidator.usosFor(catalogs, f.regimen)
        if (f.usoCfdi.isNotEmpty() && usos.none { it.code == f.usoCfdi }) f = f.copy(usoCfdi = "")
        return copy(form = f)
    }

    fun submit() {
        val id = merchantId() ?: return
        val s = _state.value
        if (!s.canSubmit) return
        val invalid = validate(s)
        if (invalid.isNotEmpty()) {
            _state.update { it.copy(invalidFields = invalid, error = InvoiceError.RECEPTOR_INVALID) }
            return
        }
        val f = s.form
        val request = CreateInvoiceRequest(
            saleIds = s.saleIds,
            receptor = Receptor(
                rfc = ReceptorValidator.normalizeRfc(f.rfc),
                nombre = f.nombre.trim(),
                regimenFiscal = f.regimen,
                codigoPostal = f.codigoPostal,
                usoCfdi = f.usoCfdi,
                email = f.email.ifBlank { null }
            )
        )
        val fingerprint = KernoJson.encodeToString(CreateInvoiceRequest.serializer(), request)
        val key = attemptKey?.takeIf { it.first == fingerprint }?.second
            ?: newIdempotencyKey().also { attemptKey = fingerprint to it }
        _state.update { it.copy(isSubmitting = true, error = null) }
        viewModelScope.launch {
            val r = withContext(io) { invoiceApi.create(id, key, request) }
            when (r) {
                is ApiResult.Success -> {
                    attemptKey = null
                    _state.update { it.copy(isSubmitting = false, result = r.data) }
                }
                is ApiResult.Error -> {
                    if (!r.isUnknownOutcome()) attemptKey = null
                    _state.update {
                        it.copy(
                            isSubmitting = false,
                            error = r.toInvoiceError(),
                            invalidFields = r.invalidReceptorFields().mapNotNull(::fieldOf).toSet(),
                            csd = if (r.errorCode in CSD_ERRORS) CsdGate.MISSING else it.csd
                        )
                    }
                }
            }
        }
    }

    /** True when the attempt key survives (the next submit replays it). Visible for tests. */
    internal val hasPendingKey: Boolean get() = attemptKey != null

    private fun validate(s: InvoiceFormUiState): Set<ReceptorField> = buildSet {
        val f = s.form
        if (s.persona == null) add(ReceptorField.RFC)
        if (f.nombre.isBlank()) add(ReceptorField.NOMBRE)
        if (s.regimenOptions.none { it.code == f.regimen }) add(ReceptorField.REGIMEN)
        if (s.usoOptions.none { it.code == f.usoCfdi }) add(ReceptorField.USO_CFDI)
        if (!ReceptorValidator.isValidPostalCode(f.codigoPostal)) add(ReceptorField.CODIGO_POSTAL)
        if (f.email.isNotBlank() && !ReceptorValidator.isValidEmail(f.email)) add(ReceptorField.EMAIL)
    }

    private fun ApiResult.Error.toInvoiceError(): InvoiceError = when {
        statusCode == NETWORK_ERROR_STATUS -> InvoiceError.NETWORK
        errorCode == "SALE_ALREADY_INVOICED" -> InvoiceError.ALREADY_INVOICED
        errorCode == "RECEPTOR_INVALID" -> InvoiceError.RECEPTOR_INVALID
        errorCode in CSD_ERRORS -> InvoiceError.CSD_REQUIRED
        errorCode == "IDEMPOTENCY_KEY_REUSED" -> InvoiceError.IDEMPOTENCY_CONFLICT
        statusCode == 403 -> InvoiceError.NOT_ALLOWED
        statusCode >= 500 -> InvoiceError.PROVIDER_UNAVAILABLE
        else -> InvoiceError.UNKNOWN
    }

    companion object {
        /** Codes the create may answer when the merchant has no usable CSD (assumed, not in the contract yet). */
        val CSD_ERRORS = setOf("CSD_NOT_FOUND", "CSD_REQUIRED", "CSD_NOT_ACTIVE", "CSD_EXPIRED")

        /** Backend field names, with or without the `receptor.` prefix. */
        fun fieldOf(name: String): ReceptorField? = when (name.removePrefix("receptor.")) {
            "rfc" -> ReceptorField.RFC
            "nombre" -> ReceptorField.NOMBRE
            "regimen_fiscal" -> ReceptorField.REGIMEN
            "uso_cfdi" -> ReceptorField.USO_CFDI
            "codigo_postal" -> ReceptorField.CODIGO_POSTAL
            "email" -> ReceptorField.EMAIL
            else -> null
        }
    }
}
