package mx.softpos.app.ui.merchant

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import mx.softpos.app.R
import mx.softpos.app.auth.MembershipInfo
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS

enum class SwitchError { NOT_A_MEMBER, NETWORK, UNKNOWN }

data class MerchantPickerUiState(
    val memberships: List<MembershipInfo> = emptyList(),
    val currentMerchantId: String? = null,
    val switchingTo: String? = null,
    val error: SwitchError? = null
)

/**
 * Chooses the tenant when the user belongs to several merchants: right after
 * login (no merchant in the token) and from *Perfil*. Switching re-scopes the
 * session with `/auth/refresh {merchant_id}`; the new session re-keys the app.
 */
class MerchantPickerViewModel(
    memberships: List<MembershipInfo>,
    currentMerchantId: String?,
    private val switchMerchant: (String) -> ApiResult<Unit>,
    private val onSwitched: () -> Unit = {},
    private val io: CoroutineDispatcher = Dispatchers.IO
) : ViewModel() {
    private val _state = MutableStateFlow(MerchantPickerUiState(memberships, currentMerchantId?.ifBlank { null }))
    val state: StateFlow<MerchantPickerUiState> = _state.asStateFlow()

    fun select(merchantId: String) {
        val s = _state.value
        if (s.switchingTo != null) return
        if (merchantId == s.currentMerchantId) return onSwitched()
        _state.update { it.copy(switchingTo = merchantId, error = null) }
        viewModelScope.launch {
            val r = withContext(io) { switchMerchant(merchantId) }
            when (r) {
                is ApiResult.Success -> {
                    _state.update { it.copy(switchingTo = null, currentMerchantId = merchantId) }
                    onSwitched()
                }
                is ApiResult.Error -> _state.update {
                    it.copy(
                        switchingTo = null,
                        error = when {
                            r.statusCode == NETWORK_ERROR_STATUS -> SwitchError.NETWORK
                            r.errorCode == "NOT_A_MERCHANT_MEMBER" -> SwitchError.NOT_A_MEMBER
                            else -> SwitchError.UNKNOWN
                        }
                    )
                }
            }
        }
    }
}

@Composable
fun MerchantPickerScreen(viewModel: MerchantPickerViewModel, onLogout: (() -> Unit)? = null, onBack: (() -> Unit)? = null) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    Column(Modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(stringResource(R.string.picker_title), style = MaterialTheme.typography.headlineSmall)
        Text(stringResource(R.string.picker_subtitle))
        state.error?.let { Text(stringResource(it.messageRes()), color = MaterialTheme.colorScheme.error) }
        LazyColumn(Modifier.weight(1f)) {
            items(state.memberships, key = { it.merchantId }) { m ->
                ListItem(
                    // Business name from the token pair; the id when the backend sent none.
                    headlineContent = { Text(m.displayName ?: stringResource(R.string.picker_merchant, m.merchantId)) },
                    supportingContent = {
                        val role = stringResource(roleRes(m.role))
                        Text(if (m.displayName != null) "$role · ${m.merchantId}" else role)
                    },
                    trailingContent = {
                        when {
                            state.switchingTo == m.merchantId -> CircularProgressIndicator()
                            state.currentMerchantId == m.merchantId -> Text(stringResource(R.string.picker_current))
                        }
                    },
                    modifier = Modifier.clickable(enabled = state.switchingTo == null) { viewModel.select(m.merchantId) }
                )
                HorizontalDivider()
            }
        }
        onBack?.let { TextButton(onClick = it) { Text(stringResource(R.string.action_back)) } }
        onLogout?.let { TextButton(onClick = it) { Text(stringResource(R.string.action_logout)) } }
    }
}

internal fun roleRes(role: String): Int = when (role) {
    "owner" -> R.string.role_owner
    "cashier" -> R.string.role_cashier
    "operator" -> R.string.role_operator
    else -> R.string.role_unknown
}

private fun SwitchError.messageRes(): Int = when (this) {
    SwitchError.NOT_A_MEMBER -> R.string.picker_error_not_member
    SwitchError.NETWORK -> R.string.error_network
    SwitchError.UNKNOWN -> R.string.error_unknown
}
