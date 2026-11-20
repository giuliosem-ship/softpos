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
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import mx.softpos.app.R
import mx.softpos.app.core.money.MoneyFormatter
import mx.softpos.app.payments.CardPaymentStatus
import mx.softpos.app.payments.Sale
import mx.softpos.app.payments.SaleMethod

@Composable
fun SalesScreen(viewModel: SalesViewModel, onOpen: (String) -> Unit) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { viewModel.load() }

    Column(Modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(stringResource(R.string.sales_title), style = MaterialTheme.typography.headlineSmall)
        if (state.isLoading) CircularProgressIndicator()
        state.error?.let { LoadErrorMessage(it, viewModel::load) }
        if (!state.isLoading && state.error == null && state.sales.isEmpty()) {
            Text(stringResource(R.string.sales_empty))
        }
        LazyColumn {
            items(state.sales, key = { it.id }) { sale ->
                ListItem(
                    headlineContent = { Text(MoneyFormatter.formatMxn(sale.amountCents)) },
                    supportingContent = { Text(stringResource(sale.methodRes()) + " · " + stringResource(sale.statusRes())) },
                    trailingContent = { Text(sale.createdAt.take(10)) },
                    modifier = Modifier.clickable { onOpen(sale.id) }
                )
                HorizontalDivider()
            }
        }
        TextButton(onClick = viewModel::load) { Text(stringResource(R.string.action_refresh)) }
    }
}

@Composable
fun SaleDetailScreen(sale: Sale?, onBack: () -> Unit) {
    Column(Modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        if (sale == null) {
            Text(stringResource(R.string.sale_not_found))
        } else {
            InfoCard(
                title = stringResource(R.string.sale_amount),
                value = MoneyFormatter.formatMxn(sale.amountCents),
                caption = stringResource(sale.methodRes()) + " · " + stringResource(sale.statusRes())
            )
            Text(stringResource(R.string.sale_id, sale.id))
            if (sale.createdAt.isNotBlank()) Text(stringResource(R.string.sale_date, sale.createdAt))
            if (sale.reference.isNotBlank()) Text(stringResource(R.string.sale_reference, sale.reference))
        }
        TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
    }
}

private fun Sale.methodRes(): Int =
    if (method.equals(SaleMethod.SPEI, ignoreCase = true)) R.string.method_spei else R.string.method_card

private fun Sale.statusRes(): Int = when {
    CardPaymentStatus.isApproved(status) -> R.string.status_approved
    CardPaymentStatus.isFailed(status) -> R.string.status_failed
    else -> R.string.status_pending
}
