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
import mx.softpos.app.payments.Sale
import mx.softpos.app.payments.SalesApi

/** Tenant of the current session, or null when no merchant is selected. */
typealias MerchantIdProvider = () -> String?

enum class LoadError { NO_MERCHANT, NETWORK, NOT_AVAILABLE, UNKNOWN }

internal fun ApiResult.Error.toLoadError(): LoadError = when {
    statusCode == NETWORK_ERROR_STATUS -> LoadError.NETWORK
    statusCode == 403 && errorCode == "MERCHANT_SCOPE_REQUIRED" -> LoadError.NO_MERCHANT
    statusCode == 404 || statusCode == 405 || statusCode == 501 -> LoadError.NOT_AVAILABLE
    else -> LoadError.UNKNOWN
}

data class HomeUiState(
    val isLoading: Boolean = false,
    val balances: MerchantBalances? = null,
    val error: LoadError? = null
)

class HomeViewModel(
    private val merchantApi: MerchantApi,
    private val merchantId: MerchantIdProvider,
    private val io: CoroutineDispatcher = Dispatchers.IO
) : ViewModel() {
    private val _state = MutableStateFlow(HomeUiState())
    val state: StateFlow<HomeUiState> = _state.asStateFlow()

    fun load() {
        val id = merchantId()
        if (id == null) {
            _state.value = HomeUiState(error = LoadError.NO_MERCHANT)
            return
        }
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            val result = withContext(io) { merchantApi.getBalances(id) }
            _state.value = when (result) {
                is ApiResult.Success -> HomeUiState(balances = result.data)
                is ApiResult.Error -> _state.value.copy(isLoading = false, error = result.toLoadError())
            }
        }
    }
}

data class SalesUiState(
    val isLoading: Boolean = false,
    val sales: List<Sale> = emptyList(),
    val error: LoadError? = null
)

class SalesViewModel(
    private val salesApi: SalesApi,
    private val merchantId: MerchantIdProvider,
    private val io: CoroutineDispatcher = Dispatchers.IO
) : ViewModel() {
    private val _state = MutableStateFlow(SalesUiState())
    val state: StateFlow<SalesUiState> = _state.asStateFlow()

    fun load() {
        val id = merchantId()
        if (id == null) {
            _state.value = SalesUiState(error = LoadError.NO_MERCHANT)
            return
        }
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            val result = withContext(io) { salesApi.listSales(id) }
            _state.value = when (result) {
                is ApiResult.Success -> SalesUiState(sales = result.data.sales)
                is ApiResult.Error -> _state.value.copy(isLoading = false, error = result.toLoadError())
            }
        }
    }

    fun sale(id: String): Sale? = _state.value.sales.firstOrNull { it.id == id }
}

data class SettingsUiState(
    val isLoading: Boolean = false,
    val profile: MerchantProfile? = null,
    val role: String = "",
    val error: LoadError? = null
)

class SettingsViewModel(
    private val merchantApi: MerchantApi,
    private val merchantId: MerchantIdProvider,
    private val role: () -> String,
    private val onLogout: () -> Unit,
    private val io: CoroutineDispatcher = Dispatchers.IO
) : ViewModel() {
    private val _state = MutableStateFlow(SettingsUiState())
    val state: StateFlow<SettingsUiState> = _state.asStateFlow()

    fun load() {
        val id = merchantId()
        if (id == null) {
            _state.value = SettingsUiState(role = role(), error = LoadError.NO_MERCHANT)
            return
        }
        _state.update { it.copy(isLoading = true, error = null, role = role()) }
        viewModelScope.launch {
            val result = withContext(io) { merchantApi.getMerchant(id) }
            _state.value = when (result) {
                is ApiResult.Success -> SettingsUiState(profile = result.data, role = role())
                is ApiResult.Error -> _state.value.copy(isLoading = false, error = result.toLoadError())
            }
        }
    }

    fun logout() = onLogout()
}
