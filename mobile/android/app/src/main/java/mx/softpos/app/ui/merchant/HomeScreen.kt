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

@Composable
fun HomeScreen(viewModel: HomeViewModel, onCharge: () -> Unit) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { viewModel.load() }

    Column(
        modifier = Modifier.fillMaxSize().padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp)
    ) {
        Text(stringResource(R.string.home_title), style = MaterialTheme.typography.headlineSmall)
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
        Button(onClick = onCharge, modifier = Modifier.fillMaxWidth()) {
            Text(stringResource(R.string.home_new_charge))
        }
        TextButton(onClick = viewModel::load) { Text(stringResource(R.string.action_refresh)) }
    }
}
