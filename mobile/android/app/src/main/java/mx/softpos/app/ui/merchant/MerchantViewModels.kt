package mx.softpos.app.ui.merchant

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
import mx.softpos.app.merchant.MerchantApi
import mx.softpos.app.merchant.MerchantBalances
import mx.softpos.app.merchant.MerchantProfile
import mx.softpos.app.kyb.KybApi
import mx.softpos.app.kyb.KybStatus
import mx.softpos.app.kyb.KybStatusValue
import mx.softpos.app.payments.Sale
import mx.softpos.app.payments.SalesApi

/** Tenant of the current session, or null when no merchant is selected. */
typealias MerchantIdProvider = () -> String?

enum class LoadError { NO_MERCHANT, NETWORK, NOT_AVAILABLE, UNKNOWN }

/**
 * 403 because the merchant's KYB is not APPROVED (charging/disbursing is
 * blocked until it is). The exact backend code is matched loosely on purpose.
 */
/** Money operations are gated until onboarding is APPROVED (backend requireActiveMerchant). */
internal fun ApiResult.Error.isKybBlocked(): Boolean = statusCode == 403 && errorCode == "MERCHANT_NOT_APPROVED"

/** Approved merchant blocked by an operator (backoffice): no onboarding step can fix it. */
internal fun ApiResult.Error.isMerchantBlocked(): Boolean = statusCode == 403 && errorCode == "MERCHANT_BLOCKED"

internal fun ApiResult.Error.toLoadError(): LoadError = when {
    statusCode == NETWORK_ERROR_STATUS -> LoadError.NETWORK
    statusCode == 403 && errorCode == "MERCHANT_SCOPE_REQUIRED" -> LoadError.NO_MERCHANT
    statusCode == 404 || statusCode == 405 || statusCode == 501 -> LoadError.NOT_AVAILABLE
    else -> LoadError.UNKNOWN
}

data class HomeUiState(
    val isLoading: Boolean = false,
    val balances: MerchantBalances? = null,
    /** Null until read, or when the backend has no KYB endpoint. */
    val kyb: KybStatus? = null,
    val error: LoadError? = null
) {
    /** Charging and disbursing are blocked by the backend until KYB is APPROVED. */
    val needsOnboarding: Boolean get() = kyb != null && !kyb.isApproved
}

class HomeViewModel(
    private val merchantApi: MerchantApi,
    private val merchantId: MerchantIdProvider,
    private val io: CoroutineDispatcher = Dispatchers.IO,
    private val kybApi: KybApi? = null
) : ViewModel() {
    private val _state = MutableStateFlow(HomeUiState())
    val state: StateFlow<HomeUiState> = _state.asStateFlow()

    /** Home sends the merchant to onboarding automatically once per app session. */
    private var autoRouted = false

    fun load() {
        val id = merchantId()
        if (id == null) {
            _state.value = HomeUiState(error = LoadError.NO_MERCHANT)
            return
        }
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            val kyb = kybApi?.let { api ->
                when (val r = withContext(io) { api.getKyb(id) }) {
                    is ApiResult.Success -> r.data
                    // 404 with a backend code = no application yet; a bare 404 = no endpoint.
                    is ApiResult.Error -> if (r.statusCode == 404 && r.errorCode != "HTTP_404") KybStatus(KybStatusValue.DRAFT) else _state.value.kyb
                }
            }
            val result = withContext(io) { merchantApi.getBalances(id) }
            _state.value = when (result) {
                is ApiResult.Success -> HomeUiState(balances = result.data, kyb = kyb)
                is ApiResult.Error -> _state.value.copy(isLoading = false, kyb = kyb, error = result.toLoadError())
            }
        }
    }

    /** True once, the first time onboarding is needed (the screen then navigates there). */
    fun consumeAutoRoute(): Boolean {
        if (autoRouted || !_state.value.needsOnboarding) return false
        autoRouted = true
        return true
    }
}

data class SalesUiState(
    val isLoading: Boolean = false,
    val isLoadingMore: Boolean = false,
    val sales: List<Sale> = emptyList(),
    /** Cursor for `before=`; null once the last page was read. */
    val nextCursor: String? = null,
    val error: LoadError? = null
) {
    val canLoadMore: Boolean get() = nextCursor != null && !isLoading && !isLoadingMore
}

/** Sales history with cursor pagination (`next_cursor` → `before`). */
class SalesViewModel(
    private val salesApi: SalesApi,
    private val merchantId: MerchantIdProvider,
    private val io: CoroutineDispatcher = Dispatchers.IO
) : ViewModel() {
    private val _state = MutableStateFlow(SalesUiState())
    val state: StateFlow<SalesUiState> = _state.asStateFlow()

    /** Reloads the first page (pull to refresh). */
    fun load() {
        val id = merchantId()
        if (id == null) {
            _state.value = SalesUiState(error = LoadError.NO_MERCHANT)
            return
        }
        if (_state.value.isLoading) return
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            val result = withContext(io) { salesApi.listSales(id) }
            _state.value = when (result) {
                is ApiResult.Success -> SalesUiState(sales = result.data.items, nextCursor = result.data.nextCursor)
                is ApiResult.Error -> _state.value.copy(isLoading = false, error = result.toLoadError())
            }
        }
    }

    /** Appends the next page; duplicates (a sale that moved across pages) are dropped. */
    fun loadMore() {
        val id = merchantId() ?: return
        val current = _state.value
        val cursor = current.nextCursor ?: return
        if (!current.canLoadMore) return
        _state.update { it.copy(isLoadingMore = true, error = null) }
        viewModelScope.launch {
            val result = withContext(io) { salesApi.listSales(id, before = cursor) }
            _state.update { s ->
                when (result) {
                    is ApiResult.Success -> {
                        val seen = s.sales.mapTo(HashSet()) { it.id }
                        s.copy(
                            isLoadingMore = false,
                            sales = s.sales + result.data.items.filter { it.id !in seen },
                            nextCursor = result.data.nextCursor
                        )
                    }
                    is ApiResult.Error -> s.copy(isLoadingMore = false, error = result.toLoadError())
                }
            }
        }
    }

    fun sale(id: String): Sale? = _state.value.sales.firstOrNull { it.id == id }
}

data class SettingsUiState(
    val isLoading: Boolean = false,
    val profile: MerchantProfile? = null,
    val role: String = "",
    /** The user belongs to several merchants: Profile offers the picker. */
    val canSwitchMerchant: Boolean = false,
    val error: LoadError? = null
)

class SettingsViewModel(
    private val merchantApi: MerchantApi,
    private val merchantId: MerchantIdProvider,
    private val role: () -> String,
    private val onLogout: () -> Unit,
    private val io: CoroutineDispatcher = Dispatchers.IO,
    private val membershipCount: () -> Int = { 0 }
) : ViewModel() {
    private val _state = MutableStateFlow(SettingsUiState())
    val state: StateFlow<SettingsUiState> = _state.asStateFlow()

    fun load() {
        val id = merchantId()
        if (id == null) {
            _state.value = SettingsUiState(role = role(), canSwitchMerchant = membershipCount() > 1, error = LoadError.NO_MERCHANT)
            return
        }
        _state.update { it.copy(isLoading = true, error = null, role = role(), canSwitchMerchant = membershipCount() > 1) }
        viewModelScope.launch {
            val result = withContext(io) { merchantApi.getMerchant(id) }
            _state.value = when (result) {
                is ApiResult.Success -> _state.value.copy(isLoading = false, profile = result.data)
                is ApiResult.Error -> _state.value.copy(isLoading = false, error = result.toLoadError())
            }
        }
    }

    fun logout() = onLogout()
}
