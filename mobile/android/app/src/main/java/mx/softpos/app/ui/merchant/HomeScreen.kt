package mx.softpos.app.ui.merchant

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import mx.softpos.app.R
import mx.softpos.app.core.money.MoneyFormatter
import mx.softpos.app.kyb.KybStatusValue
import mx.softpos.app.ui.onboarding.statusLabel

@Composable
fun HomeScreen(viewModel: HomeViewModel, onCharge: () -> Unit, onOnboarding: () -> Unit) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { viewModel.load() }
    LaunchedEffect(state.kyb) { if (viewModel.consumeAutoRoute()) onOnboarding() }

    Column(
        modifier = Modifier.fillMaxSize().padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp)
    ) {
        Text(stringResource(R.string.home_title), style = MaterialTheme.typography.headlineSmall)
        state.kyb?.takeIf { state.needsOnboarding }?.let { kyb ->
            InfoCard(
                title = stringResource(R.string.kyb_title),
                value = stringResource(statusLabel(kyb.status)),
                caption = kyb.reason?.takeIf { kyb.status == KybStatusValue.REJECTED }
                    ?.let { stringResource(R.string.kyb_reason, it) }
                    ?: stringResource(R.string.kyb_status_draft_text)
            )
            Button(onClick = onOnboarding, modifier = Modifier.fillMaxWidth()) {
                Text(
                    stringResource(
                        if (kyb.status == KybStatusValue.DRAFT || kyb.status == KybStatusValue.REJECTED) R.string.home_kyb_cta
                        else R.string.home_kyb_view
                    )
                )
            }
        }
        state.balances?.let { b ->
            InfoCard(
                title = stringResource(R.string.home_available),
                value = MoneyFormatter.formatMxn(b.availableCents),
                caption = stringResource(R.string.home_available_caption)
            )
            InfoCard(
                title = stringResource(R.string.home_tax_set_aside),
                value = MoneyFormatter.formatMxn(b.taxEscrowCents),
                caption = stringResource(R.string.home_tax_set_aside_caption)
            )
        }
        if (state.isLoading) CircularProgressIndicator()
        state.error?.let { LoadErrorMessage(it, viewModel::load) }
        Button(onClick = onCharge, enabled = !state.needsOnboarding, modifier = Modifier.fillMaxWidth()) {
            Text(stringResource(R.string.home_new_charge))
        }
        TextButton(onClick = viewModel::load) { Text(stringResource(R.string.action_refresh)) }
    }
}
