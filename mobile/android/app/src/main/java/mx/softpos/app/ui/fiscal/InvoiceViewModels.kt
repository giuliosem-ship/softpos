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
import mx.softpos.app.fiscal.CancelInvoiceRequest
import mx.softpos.app.fiscal.CancelMotivo
import mx.softpos.app.fiscal.Invoice
import mx.softpos.app.fiscal.InvoiceApi
import mx.softpos.app.fiscal.InvoiceFileKind
import mx.softpos.app.fiscal.ReceptorValidator
import mx.softpos.app.ui.merchant.LoadError
import mx.softpos.app.ui.merchant.MerchantIdProvider
import mx.softpos.app.ui.merchant.SaleDetailViewModel
import mx.softpos.app.ui.merchant.toLoadError

data class InvoicesUiState(
    val isLoading: Boolean = false,
    val isLoadingMore: Boolean = false,
    val invoices: List<Invoice> = emptyList(),
    val nextCursor: String? = null,
    val error: LoadError? = null
) {
    val canLoadMore: Boolean get() = nextCursor != null && !isLoading && !isLoadingMore
}

/** Invoices list with cursor pagination (`next_cursor` → `before`), same rules as *Ventas*. */
class InvoicesViewModel(
    private val api: InvoiceApi,
    private val merchantId: MerchantIdProvider,
    private val io: CoroutineDispatcher = Dispatchers.IO
) : ViewModel() {
    private val _state = MutableStateFlow(InvoicesUiState())
    val state: StateFlow<InvoicesUiState> = _state.asStateFlow()

    fun load() {
        val id = merchantId()
        if (id == null) {
            _state.value = InvoicesUiState(error = LoadError.NO_MERCHANT)
            return
        }
        if (_state.value.isLoading) return
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            val r = withContext(io) { api.list(id) }
            _state.value = when (r) {
                is ApiResult.Success -> InvoicesUiState(invoices = r.data.items, nextCursor = r.data.nextCursor)
                is ApiResult.Error -> _state.value.copy(isLoading = false, error = r.toLoadError())
            }
        }
    }

    fun loadMore() {
        val id = merchantId() ?: return
        val current = _state.value
        val cursor = current.nextCursor ?: return
        if (!current.canLoadMore) return
        _state.update { it.copy(isLoadingMore = true, error = null) }
        viewModelScope.launch {
            val r = withContext(io) { api.list(id, before = cursor) }
            _state.update { s ->
                when (r) {
                    is ApiResult.Success -> {
                        val seen = s.invoices.mapTo(HashSet()) { it.invoiceId }
                        s.copy(
                            isLoadingMore = false,
                            invoices = s.invoices + r.data.items.filter { it.invoiceId !in seen },
                            nextCursor = r.data.nextCursor
                        )
                    }
                    is ApiResult.Error -> s.copy(isLoadingMore = false, error = r.toLoadError())
                }
            }
        }
    }
}

/** A downloaded invoice file ready for the share sheet (content Uri from the FileProvider). */
data class SharedFile(val uri: String, val mimeType: String)

/** Writes downloaded PDF/XML into the app cache (`cache/invoices/`), never external storage. */
fun interface InvoiceFileStore {
    /** Null if it could not be written. */
    fun save(invoiceId: String, kind: InvoiceFileKind, bytes: ByteArray): SharedFile?
}

enum class CancelError { FOLIO_REQUIRED, NOT_ALLOWED, INVALID, PROVIDER_UNAVAILABLE, NETWORK, UNKNOWN }

data class InvoiceDetailUiState(
    val invoice: Invoice? = null,
    val isLoading: Boolean = false,
    val error: LoadError? = null,
    val isOwner: Boolean = false,
    val downloading: InvoiceFileKind? = null,
    val downloadFailed: Boolean = false,
    /** One-shot: the screen opens the share sheet and calls [InvoiceDetailViewModel.consumeShare]. */
    val share: SharedFile? = null,
    val motivo: String = CancelMotivo.WITHOUT_RELATION,
    val folioSustitucion: String = "",
    val isCanceling: Boolean = false,
    val cancelError: CancelError? = null
) {
    /** Only a stamped CFDI can be canceled, and only by the owner (backend enforces it too). */
    val canCancel: Boolean get() = isOwner && invoice?.isStamped == true && !isCanceling
    val needsFolio: Boolean get() = motivo == CancelMotivo.WITH_RELATION
    val motivoOptions: List<String> get() = CancelMotivo.allowedFor(invoice)
}

class InvoiceDetailViewModel(
    private val invoiceId: String,
    private val api: InvoiceApi,
    private val merchantId: MerchantIdProvider,
    role: () -> String,
    private val files: InvoiceFileStore,
    private val io: CoroutineDispatcher = Dispatchers.IO
) : ViewModel() {
    private val _state = MutableStateFlow(InvoiceDetailUiState(isOwner = role() == SaleDetailViewModel.ROLE_OWNER))
    val state: StateFlow<InvoiceDetailUiState> = _state.asStateFlow()

    fun load() {
        val id = merchantId()
        if (id == null) {
            _state.update { it.copy(error = LoadError.NO_MERCHANT) }
            return
        }
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            val r = withContext(io) { api.get(id, invoiceId) }
            _state.update {
                when (r) {
                    is ApiResult.Success -> it.copy(isLoading = false, invoice = r.data).withAllowedMotivo()
                    is ApiResult.Error -> it.copy(isLoading = false, error = r.toLoadError())
                }
            }
        }
    }

    /** Keeps the selected motivo within what the backend accepts for this invoice kind. */
    private fun InvoiceDetailUiState.withAllowedMotivo(): InvoiceDetailUiState =
        if (motivo in motivoOptions) this else copy(motivo = CancelMotivo.WITHOUT_RELATION, folioSustitucion = "")

    fun share(kind: InvoiceFileKind) {
        val id = merchantId() ?: return
        if (_state.value.downloading != null || _state.value.invoice?.uuid.isNullOrBlank()) return
        _state.update { it.copy(downloading = kind, downloadFailed = false) }
        viewModelScope.launch {
            val file = withContext(io) {
                when (val r = api.download(id, invoiceId, kind)) {
                    is ApiResult.Success -> try {
                        files.save(invoiceId, kind, r.data)
                    } finally {
                        r.data.fill(0)
                    }
                    is ApiResult.Error -> null
                }
            }
            _state.update { it.copy(downloading = null, share = file, downloadFailed = file == null) }
        }
    }

    fun consumeShare() = _state.update { it.copy(share = null) }

    fun onMotivo(code: String) {
        _state.update { if (code in it.motivoOptions) it.copy(motivo = code, cancelError = null) else it }
    }

    fun onFolio(value: String) =
        _state.update { it.copy(folioSustitucion = value.trim().take(36), cancelError = null) }

    fun cancel() {
        val id = merchantId() ?: return
        val s = _state.value
        if (!s.canCancel || s.motivo !in s.motivoOptions) return
        if (s.needsFolio && !ReceptorValidator.isValidUuid(s.folioSustitucion)) {
            _state.update { it.copy(cancelError = CancelError.FOLIO_REQUIRED) }
            return
        }
        val request = CancelInvoiceRequest(s.motivo, s.folioSustitucion.uppercase().takeIf { s.needsFolio })
        _state.update { it.copy(isCanceling = true, cancelError = null) }
        viewModelScope.launch {
            val r = withContext(io) { api.cancel(id, invoiceId, request) }
            _state.update {
                when (r) {
                    is ApiResult.Success -> it.copy(isCanceling = false, invoice = r.data, folioSustitucion = "")
                    is ApiResult.Error -> it.copy(isCanceling = false, cancelError = r.toCancelError())
                }
            }
        }
    }

    private fun ApiResult.Error.toCancelError(): CancelError = when {
        statusCode == NETWORK_ERROR_STATUS -> CancelError.NETWORK
        statusCode == 403 -> CancelError.NOT_ALLOWED
        statusCode in 400..499 -> CancelError.INVALID
        statusCode >= 500 -> CancelError.PROVIDER_UNAVAILABLE
        else -> CancelError.UNKNOWN
    }
}
