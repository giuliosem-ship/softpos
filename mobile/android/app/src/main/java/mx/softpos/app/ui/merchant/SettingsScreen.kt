package mx.softpos.app.ui.merchant

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import mx.softpos.app.R

@Composable
fun SettingsScreen(
    viewModel: SettingsViewModel,
    onSwitchMerchant: () -> Unit,
    onCsd: () -> Unit = {},
    onInvoices: () -> Unit = {}
) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { viewModel.load() }

    Column(Modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text(stringResource(R.string.settings_title), style = MaterialTheme.typography.headlineSmall)
        state.profile?.let { p ->
            InfoCard(title = stringResource(R.string.settings_business), value = p.name.ifBlank { "—" }, caption = p.rfc)
            InfoCard(
                title = stringResource(R.string.settings_clabe),
                value = p.virtualClabe.ifBlank { "—" },
                caption = stringResource(R.string.settings_clabe_caption)
            )
            if (p.isResico) Text(stringResource(R.string.settings_resico))
        }
        if (state.role.isNotBlank()) Text(stringResource(R.string.settings_role, stringResource(roleRes(state.role))))
        OutlinedButton(onClick = onInvoices, modifier = Modifier.fillMaxWidth()) {
            Text(stringResource(R.string.settings_invoices))
        }
        if (state.role == SaleDetailViewModel.ROLE_OWNER) {
            OutlinedButton(onClick = onCsd, modifier = Modifier.fillMaxWidth()) {
                Text(stringResource(R.string.settings_csd))
            }
        }
        if (state.canSwitchMerchant) {
            OutlinedButton(onClick = onSwitchMerchant, modifier = Modifier.fillMaxWidth()) {
                Text(stringResource(R.string.settings_switch_merchant))
            }
        }
        if (state.isLoading) CircularProgressIndicator()
        state.error?.let { LoadErrorMessage(it, viewModel::load) }
        OutlinedButton(onClick = viewModel::logout, modifier = Modifier.fillMaxWidth()) {
            Text(stringResource(R.string.action_logout))
        }
    }
}
