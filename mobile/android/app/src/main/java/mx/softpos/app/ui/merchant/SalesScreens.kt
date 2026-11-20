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
import mx.softpos.app.payments.Sale
import mx.softpos.app.payments.SaleKind
import mx.softpos.app.payments.SaleStatus
import mx.softpos.app.payments.SaleStatusGroup

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
        TextButton(onClick = viewModel::load) { Text(stringResource(R.string.action_refresh)) }
        LazyColumn(Modifier.weight(1f)) {
            items(state.sales, key = { it.id }) { sale ->
                ListItem(
                    headlineContent = { Text(MoneyFormatter.formatMxn(sale.amountCents)) },
                    supportingContent = { Text(stringResource(sale.kindRes()) + " · " + stringResource(statusRes(sale.status))) },
                    trailingContent = { Text(sale.createdAt.take(10)) },
                    modifier = Modifier.clickable { onOpen(sale.id) }
                )
                HorizontalDivider()
            }
            if (state.nextCursor != null) {
                item(key = "more") {
                    // Reaching the end of the list asks for the next page.
                    LaunchedEffect(state.nextCursor) { viewModel.loadMore() }
                    if (state.isLoadingMore) CircularProgressIndicator(Modifier.padding(16.dp))
                    else TextButton(onClick = viewModel::loadMore) { Text(stringResource(R.string.sales_load_more)) }
                }
            }
        }
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
                caption = stringResource(sale.kindRes()) + " · " + stringResource(statusRes(sale.status))
            )
            Text(stringResource(R.string.sale_id, sale.id))
            if (sale.createdAt.isNotBlank()) Text(stringResource(R.string.sale_date, sale.createdAt))
        }
        TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
    }
}

internal fun Sale.kindRes(): Int = when (kind.lowercase()) {
    SaleKind.SPEI -> R.string.method_spei
    SaleKind.CODI -> R.string.method_codi
    else -> R.string.method_card
}

internal fun statusRes(status: String): Int = when (SaleStatus.group(status)) {
    SaleStatusGroup.APPROVED -> R.string.status_approved
    SaleStatusGroup.FAILED -> R.string.status_failed
    SaleStatusGroup.PENDING -> R.string.status_pending
    SaleStatusGroup.PARTIALLY_REFUNDED -> R.string.status_partially_refunded
    SaleStatusGroup.REFUNDED -> R.string.status_refunded
    SaleStatusGroup.CHARGEBACK -> R.string.status_chargeback
}
